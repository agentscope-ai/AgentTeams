"""Inbound filter/bridge and outbound ack worker, shared by both entry
points.

The production sidecar (``sidecar/app.py``) and the e2e harness
(``scripts/e2e-openclaw.sh``) must honour the same protocol semantics,
so the behaviour lives here once:

  * events the agent itself sent are suppressed (own echo);
  * each event id is forwarded to OpenClaw exactly once (the adapter-
    boundary dedupe the plugin's no-requeue design relies on);
  * every ``outbound`` frame is submitted through the core and acked
    with the event id the core returns — never a fabricated one;
  * a failed submit is NOT acked: a wrong ack would desynchronise the
    client's FIFO correlation, so the frame is dropped and the
    connection left to surface the failure;
  * ``typing`` frames are ignored (logged only).

The frame builders (``make_inbound`` / ``make_ack`` /
``frame_to_outbound``) are resolved from the core at runtime via
:mod:`sidecar._core`, so this module stays importable without the core
present.
"""

from __future__ import annotations

import asyncio
import logging
import time
from typing import Callable, Optional

from sidecar import _core

_default_log: Callable[[str], None] = lambda msg: logging.getLogger(
    "sidecar.bridge"
).info(msg)


class AgentBridge:
    """Core-side runtime bridge: own-echo suppression + event-id dedupe.

    ``agent_user`` is the agent's own MXID; events from it are the
    agent's outbound messages echoing back through sync. ``log`` is a
    line sink (defaults to the ``sidecar.bridge`` logger); the e2e
    harness passes its own so its log format stays unchanged.
    """

    def __init__(
        self,
        server,
        agent_user: str,
        log: Optional[Callable[[str], None]] = None,
        media_enricher: Optional[Callable[[dict], int]] = None,
    ):
        self.server = server
        self._agent_user = agent_user
        self._log = log or _default_log
        # Optional in-place enrich hook (sidecar.media.fetch_frame_media):
        # the OpenClaw model pipeline only hydrates attachments it can read
        # from disk, so the sidecar materializes each mxc reference and adds
        # ``local_path`` before the frame leaves for the plugin. Best-effort:
        # a failure logs and the frame is forwarded unchanged.
        self._media_enricher = media_enricher
        self.seen: set = set()
        self.last_event = None
        self.forwarded = 0
        self.dropped = 0
        self.suppressed = 0

    async def on_inbound(self, event) -> None:
        if event.sender == self._agent_user:
            self.suppressed += 1
            self._log(
                f"own echo suppressed: {event.event_id} from {event.sender}"
            )
            return
        if event.event_id in self.seen:
            self.dropped += 1
            self._log(f"dedupe: dropping duplicate delivery of {event.event_id}")
            return
        self.seen.add(event.event_id)
        self.last_event = event
        self.forwarded += 1
        core = _core.load_core().core
        frame = core.make_inbound(event)
        if self._media_enricher is not None and frame.get("media"):
            try:
                fetched = self._media_enricher(frame)
                if fetched:
                    self._log(
                        f"materialized {fetched} media attachment(s) for {event.event_id}"
                    )
            except Exception as exc:  # never drop an event over an attachment
                self._log(f"media enrichment failed for {event.event_id}: {exc}")
        self.server.send(frame)
        self._log(
            f"forwarded inbound {event.event_id} ({len(event.text)} chars) to sidecar"
        )

    async def on_runtime_output(self, msg) -> None:
        self._log(f"runtime output delivered to {msg.room_id} (kind={msg.kind})")


def run_outbound_worker(
    server,
    core,
    loop,
    submit_timeout: float = 120.0,
    log: Optional[Callable[[str], None]] = None,
    should_stop: Optional[Callable[[], bool]] = None,
) -> None:
    """Read sidecar frames and answer every outbound with a real Matrix send.

    Blocking — runs on its own thread. ``loop`` is the running event
    loop of the core; submits are scheduled onto it. Returns when the
    connection ends (or a submit fails); the production runtime
    (``sidecar/app.py``) restarts the worker for the next connection.
    ``should_stop`` lets the runtime break the "no active connection"
    poll on shutdown.
    """
    log = log or _default_log
    should_stop = should_stop or (lambda: False)
    core_module = _core.load_core().core
    while True:
        if should_stop():
            return
        try:
            frame = server.recv()
        except Exception as exc:
            if "no active connection" in str(exc):
                # The worker starts before the gateway's plugin connects;
                # the first recv on an empty server raises instead of
                # blocking. Retry instead of exiting (Pi run: the worker
                # died at startup and every outbound frame went unread,
                # so the client's sendOutbound never got its ack).
                time.sleep(0.2)
                continue
            log(f"sidecar worker: connection ended ({exc})")
            return
        ftype = frame.get("type")
        if ftype == "outbound":
            msg = core_module.frame_to_outbound(frame)
            log(f"outbound to {msg.room_id}: {msg.text[:120]!r} (reply_to={msg.reply_to})")
            try:
                event_id = asyncio.run_coroutine_threadsafe(
                    core.submit(msg), loop
                ).result(timeout=submit_timeout)
            except Exception as exc:
                # No ack: a wrong ack would desync the client's FIFO
                # correlation. The connection will surface the failure.
                log(f"sidecar worker: submit failed: {exc!r}")
                return
            server.send(core_module.make_ack(event_id))
            log(f"acked outbound with {event_id}")
        elif ftype == "typing":
            log(f"typing frame (ignored): state={frame.get('state')}")
        else:
            log(f"unhandled frame type: {ftype!r}")
