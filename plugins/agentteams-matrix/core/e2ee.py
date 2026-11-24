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
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Optional

__all__ = [
    "CRYPTO_BACKENDS",
    "E2EEFailureReport",
    "E2EE_MAINTENANCE_FAILURE_THRESHOLD",
    "E2EEMaintainer",
    "crypto_available",
    "detect_crypto_backend",
    "e2ee_store_path",
    "probe_olm",
]

logger = logging.getLogger("agentteams.matrix.core.e2ee")

#: Number of consecutive E2EE maintenance passes that fail before the
#: maintainer escalates through its ``on_persistent_failure`` callback
#: (issue #1701).  Five passes give a transient blip (network dip,
#: homeserver hiccup) room to recover before the owning channel is
#: marked fatal.  The counter is in-process state on purpose: no
#: existing state-file pattern covers it, so a restart re-arms the
#: threshold (worst case the escalation is reported once more).
E2EE_MAINTENANCE_FAILURE_THRESHOLD = 5


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


@dataclass(frozen=True)
class E2EEFailureReport:
    """Payload for the ``E2EEMaintainer.on_persistent_failure`` callback.

    Attributes
    ----------
    count:
        The consecutive-failure count that just crossed
        ``E2EE_MAINTENANCE_FAILURE_THRESHOLD``.
    last_error:
        The message of the most recent maintenance error (``str``
        of the failing pass's exception).
    """

    count: int
    last_error: str


class E2EEMaintainer:
    """Post-sync E2EE key maintenance on a matrix-nio client (M4c).

    Mirrors what nio's sync_forever() does between syncs:
    - Upload device keys when needed
    - Query device keys for new/changed users
    - Claim one-time keys to establish Olm sessions
    - Send outgoing to-device messages (key shares, key requests)

    Consecutive maintenance failures are counted in-process and, once
    ``E2EE_MAINTENANCE_FAILURE_THRESHOLD`` has been crossed, the
    maintainer escalates exactly once through the optional
    ``on_persistent_failure`` callback (issue #1701).  The core layer
    never couples to any adapter mechanism: the owning layer injects
    the handler that surfaces the persistent failure (e.g. a platform
    fatal error).

    Parameters
    ----------
    client:
        An authenticated matrix-nio ``AsyncClient`` exposing
        ``olm`` / ``should_upload_keys`` / ``should_query_keys`` /
        ``should_claim_keys`` / ``keys_upload`` / ``keys_query`` /
        ``keys_claim`` / ``get_users_for_key_claiming`` /
        ``send_to_device_messages``.  ``None`` (or a client without
        ``olm``) disables the maintenance.
    on_persistent_failure:
        Optional synchronous callback invoked with an
        :class:`E2EEFailureReport` exactly once when the consecutive
        failure count reaches ``E2EE_MAINTENANCE_FAILURE_THRESHOLD``
        (the threshold-th consecutive failure of a streak; the counter
        keeps growing afterwards but the callback fires only at the
        crossing).  A raising callback is logged and ignored so a
        broken escalation handler can never take the sync loop down.
    """

    def __init__(
        self,
        client: Any,
        *,
        on_persistent_failure: Optional[
            Callable[[E2EEFailureReport], None]
        ] = None,
    ) -> None:
        self._client = client
        self._on_persistent_failure = on_persistent_failure
        self._consecutive_failures = 0
        self._last_error = ""

    @property
    def consecutive_failures(self) -> int:
        """Consecutive maintenance passes that have errored out.

        Read-only health surface for issue #1701: a successful pass
        resets it to zero, so it reads ``0`` while maintenance is
        healthy.
        """
        return self._consecutive_failures

    @property
    def last_error(self) -> str:
        """Message of the most recent maintenance error.

        Empty string while healthy (never failed, or recovered).
        """
        return self._last_error

    async def run_once(self) -> None:
        """Run one maintenance pass.

        Each failing pass is warned (legacy behaviour) and counted;
        when the consecutive-failure count reaches
        ``E2EE_MAINTENANCE_FAILURE_THRESHOLD`` the maintainer logs an
        error with the count and invokes ``on_persistent_failure``
        (issue #1701).  A successful pass resets the streak — a later
        streak escalates again.
        """
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
            self._consecutive_failures += 1
            self._last_error = str(exc)
            logger.warning("MatrixChannel: E2EE maintenance error: %s", exc)
            if (
                self._consecutive_failures
                == E2EE_MAINTENANCE_FAILURE_THRESHOLD
            ):
                logger.error(
                    "MatrixChannel: E2EE maintenance failed %d "
                    "consecutive passes — escalating (last error: %s)",
                    self._consecutive_failures,
                    exc,
                )
                callback = self._on_persistent_failure
                if callback is not None:
                    try:
                        callback(
                            E2EEFailureReport(
                                count=self._consecutive_failures,
                                last_error=self._last_error,
                            )
                        )
                    except Exception as cb_exc:
                        logger.error(
                            "MatrixChannel: on_persistent_failure "
                            "callback raised: %s",
                            cb_exc,
                        )
            return
        # Successful pass: reset the streak (and pair a recovery line
        # with the earlier warnings when there was one).
        if self._consecutive_failures:
            logger.info(
                "MatrixChannel: E2EE maintenance recovered after %d "
                "failed passes",
                self._consecutive_failures,
            )
        self._consecutive_failures = 0
        self._last_error = ""
