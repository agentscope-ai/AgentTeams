"""matrix-nio transport for the AgentTeams Matrix core (P9, PR1 M4a).

This is the only core module that imports matrix-nio.  The runtime-
neutral core (``core.interfaces.MatrixTransport``) receives an already
constructed client; the QwenPaw adapter channel builds it through
:func:`build_client` so the client construction lives in one place and
the future Hermes / OpenClaw in-process adapters can reuse it.

The construction logic is equivalent to the QwenPaw channel's legacy
``_init_async_client``: same ``AsyncClient`` kwargs, same
``AsyncClientConfig`` and the same query-token ``send`` compatibility.
"""

from __future__ import annotations

import urllib.parse
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Optional

from nio import AsyncClient, AsyncClientConfig

__all__ = [
    "MIN_SYNC_TIMEOUT_MS",
    "QwenPawMatrixClient",
    "TransportConfig",
    "build_client",
]

# Lower bound for `sync_timeout_ms`: a value of `0` would turn the
# nio `sync()` loop into a busy-spin and a negative value would feed
# `max(sync_s + 30, 60)` a nonsense duration downstream. 1 second is
# the smallest value that still gives the homeserver a chance to
# return at least one long-poll iteration.
MIN_SYNC_TIMEOUT_MS = 1000


class QwenPawMatrixClient(AsyncClient):
    """Keep query-token auth for homeservers/proxies that drop auth headers."""

    async def send(
        self,
        method: str,
        path: str,
        data: Any = None,
        headers: Optional[Dict[str, str]] = None,
        trace_context: Optional[Any] = None,
        timeout: Optional[float] = None,
    ) -> Any:
        if self.access_token and "access_token=" not in path:
            url = urllib.parse.urlparse(path)
            query = urllib.parse.parse_qs(url.query)
            query["access_token"] = [self.access_token]
            path = urllib.parse.urlunparse(
                url._replace(
                    query=urllib.parse.urlencode(query, doseq=True),
                ),
            )
        return await super().send(
            method,
            path,
            data,
            headers,
            trace_context,
            timeout,
        )


@dataclass(frozen=True)
class TransportConfig:
    """Construction parameters for :func:`build_client`.

    ``store_path`` is the directory matrix-nio persists its state into
    (the E2EE key store when encryption is on); ``None`` disables the
    on-disk store.  ``sync_timeout_ms`` is the sync long-poll timeout;
    values below :data:`MIN_SYNC_TIMEOUT_MS` are clamped to it and
    ``None`` (no opinion) resolves to the floor.
    """

    homeserver: str
    user_id: str = ""
    access_token: str = ""
    device_id: str = ""
    store_path: Optional[Path] = None
    encryption: bool = False
    sync_timeout_ms: Optional[int] = None


def build_client(config: TransportConfig) -> QwenPawMatrixClient:
    """Build the QwenPaw Matrix client (matrix-nio transport).

    Equivalent to the QwenPaw channel's legacy construction: the same
    ``AsyncClient`` kwargs, the same ``AsyncClientConfig`` (sync-token
    store off, request timeout derived from the clamped sync long-poll
    timeout, encryption flag passed through), the same store-path
    handling and the same query-token ``send`` compatibility.
    """
    effective_sync_ms = (
        max(MIN_SYNC_TIMEOUT_MS, int(config.sync_timeout_ms))
        if config.sync_timeout_ms is not None
        else MIN_SYNC_TIMEOUT_MS
    )
    store_path: Optional[Path] = None
    if config.store_path is not None:
        store_path = Path(config.store_path)
        store_path.mkdir(parents=True, exist_ok=True)
    client_config = AsyncClientConfig(
        store_sync_tokens=False,
        encryption_enabled=config.encryption,
        request_timeout=max(effective_sync_ms / 1000 + 30, 60),
    )
    client = QwenPawMatrixClient(
        config.homeserver,
        # Keep user neutral before auth; token/whoami or login response
        # will set the canonical MXID.
        user=config.user_id,
        store_path=str(store_path) if store_path else "",
        config=client_config,
    )
    # Expose the effective (clamped) sync long-poll timeout so the
    # sync-loop driver can reuse the exact value the client config was
    # built from.
    client.sync_timeout_ms = effective_sync_ms
    if config.device_id:
        client.device_id = config.device_id
    if config.access_token:
        client.access_token = config.access_token
    return client
