"""Fake of core.transport: TransportConfig + a NioTransport that records
sends and accepts injected inbound events. No matrix-nio, no network.

The constructor mirrors the real one's public signature (config plus the
keyword-only store/state_dir/encryption/...) so sidecar/app.py is
exercised against the same call shape it uses in production.
"""

from __future__ import annotations

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
        # test observability
        self.started = threading.Event()
        self.stopped = threading.Event()
        self.sent: list = []
        self.on_event = None  # set by start(); the core's dispatcher
        self._counter = 0

    async def start(self, on_event) -> None:
        self.on_event = on_event
        self.started.set()

    async def stop(self) -> None:
        self.stopped.set()

    async def send(self, msg) -> str:
        self._counter += 1
        event_id = f"$fake-{self._counter}"
        self.sent.append(msg)
        return event_id
