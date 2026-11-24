"""Fake of core.transport: TransportConfig + a NioTransport that records
sends and accepts injected inbound events. No matrix-nio, no network.

The constructor mirrors the real one's public signature (config plus the
keyword-only store/state_dir/encryption/...) so sidecar/app.py is
exercised against the same call shape it uses in production, and the
instance mirrors the real one's 'sync_task' surface (the production
runtime monitors the sync loop's death through it).
"""

from __future__ import annotations

import asyncio
import threading
from dataclasses import dataclass


@dataclass
class TransportConfig:
    homeserver: str
    user_id: str
    access_token: str
    encryption: bool = False


class NioTransport:
    def __init__(
        self,
        config: TransportConfig,
        *,
        store,
        state_dir,
        sync_timeout_ms=None,
        encryption: bool = True,
        client=None,
        on_sync=None,
        on_token_invalid=None,
    ) -> None:
        self.config = config
        self.store = store
        self.state_dir = str(state_dir)
        self.sync_timeout_ms = sync_timeout_ms
        self.encryption = bool(encryption)
        # The real transport keeps the sync engine's task so a runtime
        # can observe the loop's death (the sidecar's liveness monitor);
        # this fake mirrors that surface.
        self.sync_task = None
        self._sync_stop = None
        self._loop = None
        # test observability
        self.started = threading.Event()
        self.stopped = threading.Event()
        self.sent: list = []
        self.on_event = None  # set by start(); the core's dispatcher
        self._counter = 0

    async def _run_sync_loop(self):
        # Stand-in for the long-poll loop: live until signalled.
        await self._sync_stop.wait()

    def finish_sync_task(self) -> None:
        """Finish the sync task as if the loop returned on its own --
        the token-invalidation path of the real engine (the task
        completes normally, without an exception).

        Thread-safe: the event is set on the loop thread via
        call_soon_threadsafe (an asyncio Event set() from a foreign
        thread does not reliably wake its waiters).
        """
        if self._sync_stop is not None and self._loop is not None:
            self._loop.call_soon_threadsafe(self._sync_stop.set)

    async def start(self, on_event) -> None:
        self.on_event = on_event
        self.started.set()
        self._loop = asyncio.get_running_loop()
        self._sync_stop = asyncio.Event()
        self.sync_task = asyncio.create_task(self._run_sync_loop())

    async def stop(self) -> None:
        if self.sync_task is not None:
            self.sync_task.cancel()
            try:
                await self.sync_task
            except asyncio.CancelledError:
                pass
            self.sync_task = None
        self.stopped.set()

    async def send(self, msg) -> str:
        self._counter += 1
        event_id = f"$fake-{self._counter}"
        self.sent.append(msg)
        return event_id
