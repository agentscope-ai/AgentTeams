"""MatrixCore: wires transport, session store, and runtime bridge (P9, PR1 M1).

M1 skeleton scope: the sync engine, E2EE, media cache, mention parsing
and history persistence land in M2+; this class owns the lifecycle —
the single-writer lock (fail-closed), dispatching transport events to
the runtime bridge, and outbound delivery through the persistent
outbox (MX-1): ``submit`` enqueues (store before send) and a worker
claims due rows, sends them with a deterministic ``tx_id`` + trailing
marker, and settles them (complete / backoff / dead letter) on the
``(kind, key, seq, generation)`` quadruple.

Transport reconnection on disconnect is the *transport's* job; the core
records the resulting errors via ``last_error`` / the ``on_error`` hook.
The core has no matrix-nio dependency: the transport is injected.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
from dataclasses import replace
from typing import Awaitable, Callable, Optional, Set
from uuid import uuid4

from .interfaces import CoreConfig, InboundEvent, OutboundMessage
from .outbox import (
    OutboundQueue,
    OutboxRow,
    deterministic_tx_id,
    outbox_marker,
    with_marker,
)
from .state import SingleWriterLock, StateLayout

log = logging.getLogger("agentteams.matrix.core")

# on_error(context, exception) — called after the core records a failure.
OnErrorHook = Callable[[str, BaseException], None]


class MatrixCore:
    """Runtime-neutral core over an injected transport and bridge.

    Parameters
    ----------
    transport:
        A :class:`~core.interfaces.MatrixTransport` implementation.
    store:
        A :class:`~core.interfaces.SessionStore` implementation.
    bridge:
        A :class:`~core.interfaces.RuntimeBridge` implementation
        (owned by the runtime side).
    config:
        A :class:`~core.interfaces.CoreConfig`.
    on_error:
        Optional hook invoked with ``(context, exception)`` after the
        core records a failure.
    """

    def __init__(
        self,
        transport: object,
        store: object,
        bridge: object,
        config: CoreConfig,
        on_error: Optional[OnErrorHook] = None,
    ) -> None:
        self._transport = transport
        self._store = store
        self._bridge = bridge
        self._config = config
        self._on_error = on_error
        self._lock: Optional[SingleWriterLock] = None
        self._stop_event: Optional[asyncio.Event] = None
        self._queue: Optional[OutboundQueue] = None
        self._worker_task: Optional[asyncio.Task] = None
        self._in_flight: Set[asyncio.Task] = set()
        self._running = False
        self.last_error: Optional[str] = None

    # -- properties ---------------------------------------------------------

    @property
    def config(self) -> CoreConfig:
        return self._config

    @property
    def store(self) -> object:
        return self._store

    @property
    def queue(self) -> Optional[OutboundQueue]:
        """The persistent outbox (available while running)."""
        return self._queue

    @property
    def running(self) -> bool:
        return self._running

    @property
    def lock_path(self):
        return StateLayout(self._config.state_dir).lock

    # -- lifecycle ------------------------------------------------------------

    async def run(self) -> None:
        """Acquire the single-writer lock and run the transport loop.

        Blocks until :meth:`stop` is called.  Fails closed with
        :class:`~core.state.CoreLockedError` when another core process
        already holds the lock.

        The outbox (MX-1) is built and restored *before* the transport
        starts — the bridge may submit synchronously during
        ``transport.start`` — and the delivery worker is started right
        after it.  Rows that were ``in_flight`` at crash time are
        replayed with the same deterministic ``tx_id`` (Matrix
        deduplicates, so no second event).
        """
        if self._running:
            raise RuntimeError("core already running")
        layout = StateLayout(self._config.state_dir)
        layout.ensure()
        self._lock = SingleWriterLock(layout.lock)
        self._lock.acquire()
        self._running = True
        self._stop_event = asyncio.Event()
        try:
            config = self._config
            self._queue = OutboundQueue(
                layout.outbox,
                concurrency=config.outbox_concurrency,
                backoff_initial=config.outbox_backoff_initial,
                backoff_max=config.outbox_backoff_max,
                long_running_kinds=config.outbox_long_running_kinds,
                backoff_long_initial=config.outbox_backoff_long_initial,
                backoff_long_max=config.outbox_backoff_long_max,
            )
            self._queue.restore()
            try:
                await self._transport.start(self._on_transport_event)
            except BaseException as exc:
                self._record_error("transport_start", exc)
                raise
            self._worker_task = asyncio.create_task(
                self._delivery_loop(), name="matrix-outbox-worker"
            )
            self._worker_task.add_done_callback(self._on_worker_done)
            await self._stop_event.wait()
        finally:
            self._running = False
            worker = self._worker_task
            self._worker_task = None
            if worker is not None:
                if self._stop_event is not None and self._stop_event.is_set():
                    # Normal stop: stop() is already draining the worker;
                    # awaiting here returns as soon as the drain completes.
                    with contextlib.suppress(BaseException):
                        await worker
                else:
                    # Abnormal exit (outer cancellation): do not await —
                    # awaiting could swallow this task's own cancellation.
                    worker.cancel()
                    for task in self._in_flight:
                        task.cancel()
            self._in_flight.clear()
            self._queue = None
            self._stop_event = None
            if self._lock is not None:
                self._lock.release()
                self._lock = None

    def _on_worker_done(self, task: "asyncio.Task") -> None:
        """Worker done callback: surface unexpected worker crashes."""
        if not task.cancelled() and task.exception() is not None:
            self._record_error("outbox_worker", task.exception())

    async def stop(self) -> None:
        """Drain the outbox, stop the transport, release the lock.

        Idempotent.  Order matters (MX-1): the stop event first (the
        worker stops claiming and lets in-flight deliveries finish
        against the live transport), then the worker drain, and only
        then the transport stops — a graceful stop must not be counted
        as a send failure.
        """
        if not self._running:
            return
        if self._stop_event is not None:
            self._stop_event.set()
        worker = self._worker_task
        if worker is not None:
            with contextlib.suppress(BaseException):
                await worker
        try:
            await self._transport.stop()
        except BaseException as exc:
            self._record_error("transport_stop", exc)

    # -- event flow -----------------------------------------------------------

    async def _on_transport_event(self, event: InboundEvent) -> None:
        """Transport callback: deliver one normalised event to the bridge."""
        try:
            await self._bridge.on_inbound(event)
        except asyncio.CancelledError:
            raise
        except BaseException as exc:
            # A broken bridge handler must not block the sync loop.
            self._record_error("on_inbound", exc)

    async def submit(
        self,
        msg: OutboundMessage,
        key: Optional[str] = None,
        delay: float = 0.0,
    ) -> str:
        """Enqueue an outbound message for delivery (store before send).

        Returns the queue id ``<kind>:<key>``; delivery is *asynchronous*
        — the worker claims due rows and sends them.  Re-submitting a
        live ``key`` replaces the pending payload (generation+1,
        attempts reset, queue position kept).  ``delay`` defers the
        first claim; waiting on a future ``due`` never counts as a
        failed attempt.
        """
        if not self._running or self._queue is None:
            raise RuntimeError("core is not running")
        if delay < 0:
            raise ValueError("submit delay must be >= 0")
        if key is None:
            key = uuid4().hex
        self._queue.enqueue(msg.kind, key, msg, delay=delay)
        return f"{msg.kind}:{key}"

    # -- outbox delivery (MX-1) --------------------------------------------------

    async def _delivery_loop(self) -> None:
        """Claim due rows and deliver them (one task per row).

        While stopping, no new rows are claimed; the loop returns once
        all in-flight deliveries have settled.
        """
        while True:
            stopping = self._stop_event is not None and self._stop_event.is_set()
            if not stopping and self._queue is not None:
                for row in self._queue.claim_due():
                    task = asyncio.create_task(
                        self._deliver(row), name=f"outbox-{row.kind}-{row.key}"
                    )
                    self._in_flight.add(task)
                    task.add_done_callback(self._in_flight.discard)
            if stopping and not self._in_flight:
                return
            await asyncio.sleep(0.01 if stopping else 0.05)

    async def _deliver(self, row: OutboxRow) -> None:
        """Send one claimed row and settle it on the quadruple.

        Success: ``sent`` + ``delivered`` receipts, the row is removed
        (completed), and the bridge gets the *original* (unmarked)
        message via ``on_runtime_output``.  Failure: ``fail`` schedules
        a backoff retry or dead-letters the row after three consecutive
        failures (health error ``outbox_dead_letter``).
        """
        try:
            msg = row.as_message()
            tx_id = msg.tx_id or deterministic_tx_id(
                row.kind, row.key, row.seq, row.generation
            )
            send_msg = replace(
                msg,
                text=with_marker(
                    msg.text, outbox_marker(row.kind, row.key, row.seq, row.generation)
                ),
                tx_id=tx_id,
            )
            event_id = await self._transport.send(send_msg)
        except asyncio.CancelledError:
            raise
        except BaseException as exc:
            self._record_error("submit", exc)
            outcome = self._queue.fail(row.kind, row.key, row.seq, row.generation)
            if outcome == "dead_letter":
                self._record_error("outbox_dead_letter", exc)
            return
        self._queue.record(row.kind, row.key, row.seq, row.generation, "sent", tx_id=tx_id)
        self._queue.record(
            row.kind, row.key, row.seq, row.generation, "delivered",
            event_id=event_id, tx_id=tx_id,
        )
        self._queue.complete(row.kind, row.key, row.seq, row.generation)
        try:
            await self._bridge.on_runtime_output(msg)
        except BaseException as exc:
            self._record_error("on_runtime_output", exc)

    def ack_outbox(self, kind: str, key: str, seq: int, generation: int) -> bool:
        """Record an ``accepted`` (channel_accepted) receipt for a row.

        Callback point for a channel accept acknowledgement — MX-1
        leaves the sidecar/channel wiring unconnected.
        """
        if self._queue is None:
            raise RuntimeError("core is not running")
        return self._queue.record(kind, key, seq, generation, "accepted")

    # -- error recording --------------------------------------------------------

    def _record_error(self, context: str, exc: BaseException) -> None:
        self.last_error = f"{context}: {exc!r}"
        log.error("matrix core %s: %r", context, exc)
        if self._on_error is not None:
            try:
                self._on_error(context, exc)
            except Exception:
                log.exception("matrix core on_error hook failed")
