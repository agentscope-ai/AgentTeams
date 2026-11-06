"""MatrixCore: wires transport, session store, and runtime bridge (P9, PR1 M1).

M1 skeleton scope: the sync engine, E2EE, media cache, mention parsing
and history persistence land in M2+; this class owns the lifecycle —
the single-writer lock (fail-closed), dispatching transport events to
the runtime bridge, and outbound submission to the transport.

Transport reconnection on disconnect is the *transport's* job; the core
records the resulting errors via ``last_error`` / the ``on_error`` hook.
The core has no matrix-nio dependency: the transport is injected.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Awaitable, Callable, Optional

from .interfaces import CoreConfig, InboundEvent, OutboundMessage
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
        """
        if self._running:
            raise RuntimeError("core already running")
        StateLayout(self._config.state_dir).ensure()
        self._lock = SingleWriterLock(StateLayout(self._config.state_dir).lock)
        self._lock.acquire()
        self._running = True
        self._stop_event = asyncio.Event()
        try:
            try:
                await self._transport.start(self._on_transport_event)
            except BaseException as exc:
                self._record_error("transport_start", exc)
                raise
            await self._stop_event.wait()
        finally:
            self._running = False
            self._stop_event = None
            if self._lock is not None:
                self._lock.release()
                self._lock = None

    async def stop(self) -> None:
        """Stop the transport and release the single-writer lock (idempotent)."""
        if not self._running:
            return
        try:
            await self._transport.stop()
        except BaseException as exc:
            self._record_error("transport_stop", exc)
        if self._stop_event is not None:
            self._stop_event.set()

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

    async def submit(self, msg: OutboundMessage) -> str:
        """Send an outbound message through the transport.

        Returns the resulting Matrix event id.  The bridge is notified
        via ``on_runtime_output`` (core callback) after a successful
        send; a bridge notification failure is recorded but does not
        hide the send result.
        """
        if not self._running:
            raise RuntimeError("core is not running")
        try:
            event_id = await self._transport.send(msg)
        except BaseException as exc:
            self._record_error("submit", exc)
            raise
        try:
            await self._bridge.on_runtime_output(msg)
        except BaseException as exc:
            self._record_error("on_runtime_output", exc)
        return event_id

    # -- error recording --------------------------------------------------------

    def _record_error(self, context: str, exc: BaseException) -> None:
        self.last_error = f"{context}: {exc!r}"
        log.error("matrix core %s: %r", context, exc)
        if self._on_error is not None:
            try:
                self._on_error(context, exc)
            except Exception:
                log.exception("matrix core on_error hook failed")
