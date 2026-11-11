"""Long-poll sync driver with sync-token persistence (P9, PR1 M4b).

Extracted from the QwenPaw channel's ``_sync_loop`` so every runtime
adapter can drive the same loop: one initial sync, then long-poll
increments, with the ``next_batch`` token persisted between syncs so an
upgrade or restart resumes where it left off instead of replaying
messages.

The behaviour is the channel's legacy behaviour, moved verbatim:

- No persisted token (fresh install / old-version upgrade) — one
  ``sync(timeout=0, full_state=True)`` with the client's event
  callbacks removed for its duration (nio fires them inside ``sync()``
  via ``receive_response()``, so they must be removed, not merely
  ignored); the token is captured so the loop that follows is
  incremental.  No long-poll here: it can swallow fresh messages sent
  during worker boot.
- Persisted token — one ``sync(timeout=sync_timeout_ms, since=token,
  full_state=True)`` with callbacks active (messages received during
  the offline window are processed normally; full state populates room
  member display names).
- Incremental — ``sync(timeout=sync_timeout_ms, since=last_next_batch,
  full_state=False)``; every successful response persists its
  ``next_batch`` and auto-joins invited rooms.
- ``SyncError`` with a ``status_code`` of ``M_UNKNOWN_TOKEN`` /
  ``M_MISSING_TOKEN`` — clear the client's access token, tell the
  owner, stop the loop (re-login required).
- Any other failed sync — log, back off 5 s, retry.

Channel-specific pieces stay in the adapter and are injected as hooks:
the logger (so the moved log lines read exactly as before), E2EE
maintenance after each successful sync (``on_maintenance``), the
readiness marker after the initial phase (``on_ready``), and clearing
the channel's own ``access_token`` state on token invalidation
(``on_token_invalid``).  ``on_event`` is an optional room-event sink:
when given, it is registered on the client alongside the channel's own
room callbacks, so the catch-up window suppresses it the same way.

Like ``core.transport`` (M4a), this module imports matrix-nio (for the
``SyncResponse`` / ``SyncError`` response types the loop inspects); it
must not be imported from ``core/__init__.py``.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Any, Callable, Optional

from nio import SyncResponse
from nio.responses import SyncError

from .interfaces import SessionStore

__all__ = ["SyncEngine"]


class SyncEngine:
    """Drives the matrix-nio long-poll sync loop with token persistence.

    Parameters
    ----------
    client:
        An authenticated matrix-nio ``AsyncClient``.  The engine calls
        ``sync`` / ``join`` on it, clears and restores its
        ``event_callbacks`` for the catch-up suppression window, and
        registers ``on_event`` through ``add_event_callback`` when
        given.
    store:
        A :class:`~core.interfaces.SessionStore`; only the
        ``load_sync_token`` / ``save_sync_token`` pair is used.  The
        QwenPaw channel passes a store over its legacy token file
        (``WORKING_DIR/matrix_sync_token``, plain text) so the on-disk
        path and format survive the move unchanged.
    sync_timeout_ms:
        Long-poll timeout for the full-state and incremental syncs
        (the initial catch-up sync always uses ``timeout=0``).
    logger:
        Logger for the moved log lines; the channel passes its own so
        the observable output is unchanged.
    on_event:
        Optional room-event callback (``(room, event)``).  Registered
        on the client when :meth:`run` starts; suppressed by the
        catch-up window like any other event callback.
    on_ready:
        Optional no-arg callable invoked after the initial phase,
        before the incremental loop (channel: readiness marker).
    on_maintenance:
        Optional async no-arg callable invoked after each successful
        sync (channel: E2EE key maintenance).
    on_token_invalid:
        Optional no-arg callable invoked right before the loop stops
        when the homeserver rejects the token (channel: clears its own
        ``access_token`` state).
    """

    def __init__(
        self,
        client: Any,
        store: SessionStore,
        sync_timeout_ms: int,
        logger: Optional[logging.Logger] = None,
        on_event: Optional[Callable[..., Any]] = None,
        on_ready: Optional[Callable[[], None]] = None,
        on_maintenance: Optional[Callable[[], Any]] = None,
        on_token_invalid: Optional[Callable[[], None]] = None,
    ) -> None:
        self._client = client
        self._store = store
        self._sync_timeout_ms = sync_timeout_ms
        self._logger = logger or logging.getLogger("agentteams.matrix.core.sync")
        self._on_event = on_event
        self._on_ready = on_ready
        self._on_maintenance = on_maintenance
        self._on_token_invalid = on_token_invalid
        self._task: Optional[asyncio.Task] = None
        self._event_registered = False

    # -- lifecycle -----------------------------------------------------------

    def start(self) -> "asyncio.Task":
        """Schedule :meth:`run` as a background task and return it.

        The channel keeps the returned task as ``self._sync_task`` so
        the stop-time semantics (cancel + await) are unchanged.
        """
        self._task = asyncio.create_task(self.run())
        return self._task

    async def stop(self) -> None:
        """Cancel the running task and wait for it (idempotent).

        Mirrors the channel's legacy stop: cancel, await, and swallow
        the resulting ``CancelledError`` with a debug log.
        """
        task, self._task = self._task, None
        if task is None:
            return
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            self._logger.debug("MatrixChannel: sync task cancelled during stop component=matrix")

    # -- the loop (moved verbatim from the channel's _sync_loop) -------------

    # pylint: disable=too-many-branches,too-many-statements
    async def run(self) -> None:
        if self._on_event is not None and not self._event_registered:
            # Registered on the client alongside the channel's own room
            # callbacks, so the catch-up window below suppresses it too.
            self._client.add_event_callback(self._on_event)
            self._event_registered = True

        next_batch: Optional[str] = self._store.load_sync_token()

        # When no persisted token exists (old version upgrade or first
        # deploy), do an initial sync with callbacks suppressed — only capture
        # next_batch so subsequent syncs are incremental.  This prevents
        # replaying old messages when the token file doesn't exist yet.
        #
        # Use timeout=0 so startup does not long-poll while callbacks are
        # temporarily suppressed. A long-poll here can swallow fresh messages
        # sent during worker boot.
        #
        # To truly suppress callbacks, temporarily remove event callbacks
        # before the sync and restore them after, because nio's sync()
        # internally calls receive_response() which fires callbacks.
        if next_batch is None:
            self._logger.info(
                "MatrixChannel: no sync token found, "
                "performing catch-up sync component=matrix messages_suppressed=True",
            )
            try:
                saved_cbs = self._client.event_callbacks[:]
                self._client.event_callbacks.clear()
                try:
                    resp = await self._client.sync(
                        timeout=0,
                        full_state=True,
                    )
                finally:
                    self._client.event_callbacks.extend(saved_cbs)
                if isinstance(resp, SyncResponse):
                    next_batch = resp.next_batch
                    if next_batch is not None:
                        self._store.save_sync_token(next_batch)
                    # Still auto-join invited rooms during catch-up
                    for room_id in resp.rooms.invite:
                        self._logger.info("MatrixChannel: auto-joining component=matrix room_id=%s", room_id)
                        await self._client.join(room_id)
                    if self._on_maintenance is not None:
                        await self._on_maintenance()
                    self._logger.info(
                        "MatrixChannel: catch-up sync done, "
                        "will process messages from next sync component=matrix",
                    )
                else:
                    self._logger.warning(
                        "MatrixChannel: catch-up sync error component=matrix response_type=%s",
                        type(resp).__name__,
                    )
            except Exception as exc:
                self._logger.exception(
                    "MatrixChannel: catch-up sync exception component=matrix error_type=%s",
                    type(exc).__name__,
                )
        else:
            # Restored from token — do a full_state sync to populate room
            # member display names (nio needs full state for user_name()).
            # Event callbacks are already registered so any messages received
            # during the offline window will be processed normally.
            self._logger.info(
                "MatrixChannel: restored token, "
                "performing full-state sync component=matrix",
            )
            try:
                resp = await self._client.sync(
                    timeout=self._sync_timeout_ms,
                    since=next_batch,
                    full_state=True,
                )
                if isinstance(resp, SyncResponse):
                    next_batch = resp.next_batch
                    if next_batch is not None:
                        self._store.save_sync_token(next_batch)
                    for room_id in resp.rooms.invite:
                        self._logger.info("MatrixChannel: auto-joining component=matrix room_id=%s", room_id)
                        await self._client.join(room_id)
                    if self._on_maintenance is not None:
                        await self._on_maintenance()
                else:
                    self._logger.warning(
                        "MatrixChannel: full-state sync error component=matrix response_type=%s",
                        type(resp).__name__,
                    )
            except Exception as exc:
                self._logger.exception(
                    "MatrixChannel: full-state sync exception component=matrix error_type=%s",
                    type(exc).__name__,
                )

        if self._on_ready is not None:
            self._on_ready()

        while True:
            try:
                resp = await self._client.sync(
                    timeout=self._sync_timeout_ms,
                    since=next_batch,
                    full_state=False,
                )
                if isinstance(resp, SyncResponse):
                    next_batch = resp.next_batch
                    if next_batch is not None:
                        self._store.save_sync_token(next_batch)
                    # Auto-join invited rooms
                    for room_id in resp.rooms.invite:
                        self._logger.info("MatrixChannel: auto-joining component=matrix room_id=%s", room_id)
                        await self._client.join(room_id)
                    # E2EE: full key maintenance (upload, query, claim,
                    # to-device)
                    if self._on_maintenance is not None:
                        await self._on_maintenance()
                else:
                    if isinstance(resp, SyncError) and getattr(
                        resp,
                        "status_code",
                        None,
                    ) in {"M_UNKNOWN_TOKEN", "M_MISSING_TOKEN"}:
                        self._logger.error(
                            "MatrixChannel: sync stopped due to "
                            "invalid/missing access token; please re-login "
                            "(password or fresh token) component=matrix",
                        )
                        if self._client:
                            self._client.access_token = ""
                        if self._on_token_invalid is not None:
                            self._on_token_invalid()
                        return
                    self._logger.warning("MatrixChannel: sync error component=matrix response_type=%s", type(resp).__name__)
                    await asyncio.sleep(5)
            except asyncio.CancelledError:
                self._logger.debug("MatrixChannel: sync loop cancelled component=matrix")
                raise
            except Exception as exc:
                self._logger.exception("MatrixChannel: sync exception component=matrix error_type=%s", type(exc).__name__)
                await asyncio.sleep(5)
