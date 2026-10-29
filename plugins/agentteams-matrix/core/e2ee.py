"""E2EE maintenance for the AgentTeams Matrix core (P9, PR1 M4c).

The olm preflight probe and the post-sync key maintenance (upload /
query / claim / to-device) moved out of the QwenPaw channel.  The
maintainer drives the matrix-nio client it is given; the not-ready
guard and the warning-on-error degradation are the channel's legacy
behaviour, moved verbatim (same log message wording).

This module never imports matrix-nio at import time: ``probe_olm``
only probes for the ``olm`` module, and the client is an injected
dependency.
"""

from __future__ import annotations

import importlib
import logging
from pathlib import Path
from typing import Any

__all__ = [
    "E2EEMaintainer",
    "e2ee_store_path",
    "probe_olm",
]

logger = logging.getLogger("agentteams.matrix.core.e2ee")


def e2ee_store_path(working_dir: Path) -> Path:
    """Return the directory for persisting Olm/Megolm crypto state.

    The layout is fixed for M4c: ``<working_dir>/matrix_crypto_store``
    (a layout change is M5 territory and requires a migration test).
    """
    return Path(working_dir) / "matrix_crypto_store"


def probe_olm() -> bool:
    """Return True when the ``olm`` module imports; False otherwise.

    No logging here: the caller keeps the existing error wording.
    """
    try:
        importlib.import_module("olm")
    except ImportError:
        return False
    return True


class E2EEMaintainer:
    """Post-sync E2EE key maintenance on a matrix-nio client (M4c).

    Mirrors what nio's sync_forever() does between syncs:
    - Upload device keys when needed
    - Query device keys for new/changed users
    - Claim one-time keys to establish Olm sessions
    - Send outgoing to-device messages (key shares, key requests)

    Parameters
    ----------
    client:
        An authenticated matrix-nio ``AsyncClient`` exposing
        ``olm`` / ``should_upload_keys`` / ``should_query_keys`` /
        ``should_claim_keys`` / ``keys_upload`` / ``keys_query`` /
        ``keys_claim`` / ``get_users_for_key_claiming`` /
        ``send_to_device_messages``.  ``None`` (or a client without
        ``olm``) disables the maintenance.
    """

    def __init__(self, client: Any) -> None:
        self._client = client

    async def run_once(self) -> None:
        """Run one maintenance pass; errors are swallowed with a warning."""
        client = self._client
        if client is None or not client.olm:
            return
        try:
            if client.should_upload_keys:
                await client.keys_upload()
            if client.should_query_keys:
                await client.keys_query()
            if client.should_claim_keys:
                await client.keys_claim(
                    client.get_users_for_key_claiming(),
                )
            await client.send_to_device_messages()
        except Exception as exc:
            logger.warning("MatrixChannel: E2EE maintenance error: %s", exc)
