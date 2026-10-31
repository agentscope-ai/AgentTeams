"""E2EE maintenance for the AgentTeams Matrix core (P9, PR1 M4c).

The olm preflight probe and the post-sync key maintenance (upload /
query / claim / to-device) moved out of the QwenPaw channel.  The
maintainer drives the matrix-nio client it is given; the not-ready
guard and the warning-on-error degradation are the channel's legacy
behaviour, moved verbatim (same log message wording).

This module never imports matrix-nio at import time: the crypto
backend probe only checks whether ``vodozemac`` (matrix-nio 0.26+)
or ``olm`` (older releases) is importable, and the client is an
injected dependency.
"""

from __future__ import annotations

import importlib
import logging
from pathlib import Path
from typing import Any

__all__ = [
    "CRYPTO_BACKENDS",
    "E2EEMaintainer",
    "crypto_available",
    "detect_crypto_backend",
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


# Crypto backends matrix-nio can drive E2EE with, in preference order:
# 0.26 and newer ship the ``e2e`` extra as ``vodozemac`` (no top-level
# ``olm`` module); older releases use ``olm``.
CRYPTO_BACKENDS: tuple[str, ...] = ("vodozemac", "olm")


def _importable(module: str) -> bool:
    """Return True when ``module`` can be imported; False otherwise.

    Only import failures are swallowed (``ImportError`` and its
    ``ModuleNotFoundError`` subclass); any other exception raised while
    importing the module propagates to the caller.
    """
    try:
        importlib.import_module(module)
    except (ImportError, ModuleNotFoundError):
        return False
    return True


def detect_crypto_backend() -> str | None:
    """Return the first importable name from ``CRYPTO_BACKENDS``.

    ``None`` when no backend is importable.  Preference order matters:
    a runtime where both are importable reports ``vodozemac``.
    """
    for backend in CRYPTO_BACKENDS:
        if _importable(backend):
            return backend
    return None


def crypto_available() -> bool:
    """Return True when an E2EE crypto backend is importable."""
    return detect_crypto_backend() is not None


def probe_olm() -> bool:
    """Return True when the ``olm`` module imports; False otherwise.

    Legacy nio compatibility check only: matrix-nio 0.26+ no longer
    provides a top-level ``olm`` module, so E2EE availability
    decisions must use :func:`crypto_available`.  No logging here:
    the caller keeps the existing error wording.
    """
    return _importable("olm")


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
