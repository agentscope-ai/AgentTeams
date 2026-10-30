"""matrix-nio transport for the AgentTeams Matrix core (P9, PR1).

This is the only core module that imports matrix-nio.  The runtime-
neutral core (``core.interfaces.MatrixTransport``) receives an already
constructed client; the QwenPaw adapter channel builds it through
:func:`build_client` so the client construction lives in one place and
the future Hermes / OpenClaw in-process adapters can reuse it.

The construction logic is equivalent to the QwenPaw channel's legacy
``_init_async_client``: same ``AsyncClient`` kwargs, same
``AsyncClientConfig`` and the same query-token ``send`` compatibility
(the latter only behind the explicit ``TransportConfig.query_token``
flag — off by default, since matrix-nio already sends the
``Authorization`` header and modern Synapse rejects requests that mix
it with an ``access_token`` query parameter).

M5b adds :class:`NioTransport` — the full ``MatrixTransport``
implementation over matrix-nio (sync loop, event normalisation,
outbound send/edit/react/typing, media download) — plus the pure
:func:`normalize_room_event` mapping from nio events to the core's
:class:`~core.interfaces.InboundEvent` vocabulary.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import urllib.parse
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, Awaitable, Callable, Dict, List, Optional, Tuple, Union
from uuid import uuid4

from nio import (
    AsyncClient,
    AsyncClientConfig,
    KeysUploadResponse,
    MegolmEvent,
    RedactionEvent,
    RoomEncryptedMedia,
    RoomMessage,
    RoomMessageMedia,
    RoomMessageNotice,
    RoomMessageText,
    RoomSendResponse,
    RoomTypingResponse,
)

from .e2ee import E2EEMaintainer, crypto_available
from .interfaces import (
    InboundEvent,
    MediaRef,
    OutboundMessage,
    SessionStore,
    TYPING_STATES,
)
from .media import MediaCache
from .mentions import extract_mentions_from_text
from .state import StateLayout
from .sync import SyncEngine

__all__ = [
    "MIN_SYNC_TIMEOUT_MS",
    "NioTransport",
    "QwenPawMatrixClient",
    "ROOM_EVENT_FILTER",
    "TYPING_TIMEOUT_MS",
    "TransportConfig",
    "build_client",
    "normalize_room_event",
]

# Lower bound for `sync_timeout_ms`: a value of `0` would turn the
# nio `sync()` loop into a busy-spin and a negative value would feed
# `max(sync_s + 30, 60)` a nonsense duration downstream. 1 second is
# the smallest value that still gives the homeserver a chance to
# return at least one long-poll iteration.
MIN_SYNC_TIMEOUT_MS = 1000


class QwenPawMatrixClient(AsyncClient):
    """Keep query-token auth for homeservers/proxies that drop auth headers.

    Optional compatibility layer only — enabled exclusively by
    ``TransportConfig.query_token=True`` (the QwenPaw adapter sets the
    flag to keep its legacy behaviour).  :func:`build_client` returns
    a plain :class:`nio.AsyncClient` by default.
    """

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
    ``None`` (no opinion) resolves to the floor.  ``query_token``
    keeps the legacy QwenPaw query-token ``send`` compatibility (off
    by default: matrix-nio already sends the ``Authorization`` header
    and modern Synapse rejects the header + query-token mix).
    """

    homeserver: str
    user_id: str = ""
    access_token: str = ""
    device_id: str = ""
    store_path: Optional[Path] = None
    encryption: bool = False
    sync_timeout_ms: Optional[int] = None
    query_token: bool = False


def build_client(config: TransportConfig) -> AsyncClient:
    """Build the matrix-nio Matrix client.

    By default (``query_token=False``) a plain :class:`nio.AsyncClient`
    is returned: matrix-nio already authenticates with the
    ``Authorization`` header, and appending the access token to the
    query as well is rejected by modern Synapse ("mixing
    Authorization headers and access_token query parameters").  With
    ``query_token=True`` the :class:`QwenPawMatrixClient`
    compatibility layer is returned instead (the QwenPaw adapter sets
    the flag to keep its legacy behaviour).

    Equivalent to the QwenPaw channel's legacy construction: the same
    ``AsyncClient`` kwargs, the same ``AsyncClientConfig`` (sync-token
    store off, request timeout derived from the clamped sync long-poll
    timeout, encryption flag passed through) and the same store-path
    handling.
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
    client_cls = QwenPawMatrixClient if config.query_token else AsyncClient
    client = client_cls(
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


# ---------------------------------------------------------------------------
# NioTransport — the matrix-nio MatrixTransport implementation (M5b)
# ---------------------------------------------------------------------------

#: Server-side typing-indication timeout (ms).  The QwenPaw channel
#: renews with the same constant, so the core path matches its behaviour.
TYPING_TIMEOUT_MS = 30000

logger = logging.getLogger("agentteams.matrix.core.transport")

# Event types the transport registers for: the plaintext message surface
# (text/notice/media families), nio's decrypted media classes,
# undecryptable Megolm events (warned and suppressed — never downgraded
# to plaintext) and redactions.  Decrypted *text* events arrive as the
# plaintext classes with ``decrypted=True``, so no extra decrypted-text
# class is needed.
ROOM_EVENT_FILTER: Tuple[type, ...] = (
    RoomMessageText,
    RoomMessageNotice,
    RoomMessageMedia,
    RoomEncryptedMedia,
    MegolmEvent,
    RedactionEvent,
)


def _content_of(source: Dict[str, Any]) -> Dict[str, Any]:
    content = source.get("content")
    return content if isinstance(content, dict) else {}


def _relates_to_of(content: Dict[str, Any]) -> Dict[str, Any]:
    relates = content.get("m.relates_to")
    return relates if isinstance(relates, dict) else {}


def normalize_room_event(room: Any, event: Any) -> Optional[InboundEvent]:
    """Normalise one nio room event into an :class:`InboundEvent`.

    Returns ``None`` when the event must not be delivered:
    undecryptable Megolm events (a warning is logged — they are never
    downgraded to plaintext) and anything outside the text/media/
    edit/redaction surface.

    Mapping: plaintext ``RoomMessageText``/``RoomMessageNotice`` and
    decrypted text (the same classes with ``decrypted=True``) ->
    ``kind="text"``; ``RoomMessageMedia`` / decrypted media
    (``RoomEncrypted*``) -> ``kind="media"`` with a :class:`MediaRef`;
    ``m.replace`` edits -> ``kind="edit"``; redactions ->
    ``kind="redaction"`` (no text).  ``mentions`` comes from
    :func:`extract_mentions_from_text`, ``thread_root`` from an
    ``m.thread`` relation, ``ts`` from ``server_timestamp`` (``0``
    when missing).
    """
    source = getattr(event, "source", None)
    if not isinstance(source, dict):
        return None
    room_id = getattr(room, "room_id", None) or source.get("room_id") or ""
    event_id = getattr(event, "event_id", None) or source.get("event_id") or ""
    sender = getattr(event, "sender", None) or source.get("sender") or ""
    if not (event_id and room_id and sender):
        return None
    ts = int(getattr(event, "server_timestamp", 0) or 0)

    if isinstance(event, MegolmEvent):
        logger.warning(
            "NioTransport: event %s in %s could not be decrypted "
            "(session_id=%s); not delivering",
            event_id,
            room_id,
            getattr(event, "session_id", None),
        )
        return None

    if source.get("type") == "m.room.redaction" or isinstance(event, RedactionEvent):
        return InboundEvent(
            event_id=event_id,
            room_id=room_id,
            sender=sender,
            ts=ts,
            text="",
            kind="redaction",
        )

    if not isinstance(event, (RoomMessage, RoomEncryptedMedia)):
        return None

    content = _content_of(source)
    relates = _relates_to_of(content)
    thread_root = (
        relates.get("event_id") if relates.get("rel_type") == "m.thread" else None
    )

    if relates.get("rel_type") == "m.replace":
        new_content = content.get("m.new_content")
        new_content = new_content if isinstance(new_content, dict) else {}
        text = str(new_content.get("body", ""))
        media: List[MediaRef] = []
        if new_content.get("url"):
            info = new_content.get("info")
            mime = info.get("mimetype") if isinstance(info, dict) else None
            media = [
                MediaRef(
                    mxc_uri=str(new_content["url"]),
                    mime_type=mime,
                    file_name=text or None,
                )
            ]
        return InboundEvent(
            event_id=event_id,
            room_id=room_id,
            sender=sender,
            ts=ts,
            text=text,
            media=media,
            mentions=extract_mentions_from_text(text),
            thread_root=thread_root,
            kind="edit",
        )

    body = str(getattr(event, "body", "") or "")
    if isinstance(event, (RoomMessageMedia, RoomEncryptedMedia)):
        url = str(getattr(event, "url", "") or "")
        info = content.get("info")
        mime = info.get("mimetype") if isinstance(info, dict) else None
        media = (
            [MediaRef(mxc_uri=url, mime_type=mime, file_name=body or None)]
            if url
            else []
        )
        return InboundEvent(
            event_id=event_id,
            room_id=room_id,
            sender=sender,
            ts=ts,
            text=body,
            media=media,
            mentions=extract_mentions_from_text(body),
            thread_root=thread_root,
            kind="media",
        )

    return InboundEvent(
        event_id=event_id,
        room_id=room_id,
        sender=sender,
        ts=ts,
        text=body,
        mentions=extract_mentions_from_text(body),
        thread_root=thread_root,
        kind="text",
    )


class NioTransport:
    """matrix-nio implementation of :class:`~core.interfaces.MatrixTransport`.

    The transport owns the nio client lifecycle:

    - **build path** (no injected client): the client comes from
      :func:`build_client`.  When ``encryption`` is requested but
      :func:`core.e2ee.crypto_available` is false the transport degrades
      to non-encrypted mode and logs the error instead of raising
      (parity with the QwenPaw channel preflight);
    - **keys upload**: when encryption is on and ``should_upload_keys``
      is true, ``keys_upload()`` runs once during :meth:`start`; a
      failed upload aborts the start so the core records a
      ``transport_start`` error instead of running an
      encryption-broken sync loop;
    - **sync loop**: :class:`~core.sync.SyncEngine` with the injected
      :class:`~core.interfaces.SessionStore`; :meth:`E2EEMaintainer.run_once`
      runs after every successful sync (and the optional ``on_sync``
      hook right after that maintenance pass).

    Inbound room events flow through :func:`normalize_room_event` into
    the ``on_event`` callback from :meth:`start`; a failing handler is
    logged and never breaks the sync loop.  Outbound: :meth:`send`
    (``final``/``notice`` -> ``m.text``, ``tool`` -> ``m.notice``,
    ``reply_to`` -> ``m.in_reply_to``), :meth:`edit` (``m.replace``),
    :meth:`react` (``m.reaction``), :meth:`typing` (``room_typing``)
    and :meth:`download` (bytes through :class:`~core.media.MediaCache`
    backed by ``client.download``; nio 0.26 returns a response object
    whose ``body`` holds the bytes).  Outbound failures raise;
    :class:`~core.core.MatrixCore` records and re-raises them.
    """

    def __init__(
        self,
        config: TransportConfig,
        *,
        store: SessionStore,
        state_dir: Union[str, Path],
        sync_timeout_ms: Optional[int] = None,
        encryption: bool = True,
        client: Optional[AsyncClient] = None,
        on_sync: Optional[Callable[[], Any]] = None,
    ) -> None:
        self._config = config
        self._store = store
        self._state_dir = Path(state_dir)
        self._sync_timeout_ms = sync_timeout_ms
        self.encryption = bool(encryption)
        self._client: Optional[AsyncClient] = client
        self._on_sync = on_sync
        self._on_event: Optional[Callable[[InboundEvent], Awaitable[None]]] = None
        self._engine: Optional[SyncEngine] = None
        self._maintainer: Optional[E2EEMaintainer] = None
        self._callback_registered = False
        self._client_closed = False
        self._media_cache = MediaCache(
            self._fetch_media_bytes, StateLayout(self._state_dir).media_cache
        )

    # -- lifecycle ------------------------------------------------------------

    async def start(
        self, on_event: Callable[[InboundEvent], Awaitable[None]]
    ) -> None:
        """Start the transport: client, keys upload, callbacks, sync loop.

        Returns once the sync loop is running (the loop itself is an
        asyncio task owned by the :class:`SyncEngine`).
        """
        if self._engine is not None:
            raise RuntimeError("NioTransport already started")
        self._on_event = on_event
        client = self._client
        if client is None:
            if self.encryption and not crypto_available():
                logger.error(
                    "NioTransport: no Matrix E2EE backend (vodozemac/olm) "
                    "— falling back to non-encrypted mode. Install "
                    "matrix-nio[e2e]; on matrix-nio < 0.26 also install "
                    "libolm (apt/dnf install libolm-dev).",
                )
                self.encryption = False
            build_config = self._config
            if self._sync_timeout_ms is not None:
                build_config = replace(
                    build_config, sync_timeout_ms=self._sync_timeout_ms
                )
            if self.encryption:
                store_path = build_config.store_path or StateLayout(
                    self._state_dir
                ).e2ee
                build_config = replace(
                    build_config, encryption=True, store_path=store_path
                )
            else:
                build_config = replace(build_config, encryption=False)
            client = build_client(build_config)
            self._client = client
        self._maintainer = E2EEMaintainer(client)

        if self.encryption:
            if getattr(client, "should_upload_keys", False):
                resp = await client.keys_upload()
                if not isinstance(resp, KeysUploadResponse):
                    raise RuntimeError(
                        "E2E keys upload failed after start "
                        f"(response_type={type(resp).__name__})"
                    )
                logger.info("NioTransport: E2E keys uploaded component=matrix")

        # nio 0.26 requires the event-type filter on registration; the
        # SyncEngine's catch-up suppression window clears and restores
        # client.event_callbacks, which covers this callback as well.
        client.add_event_callback(self._on_room_event, ROOM_EVENT_FILTER)
        self._callback_registered = True

        timeout_ms = getattr(client, "sync_timeout_ms", None)
        if timeout_ms is None:
            raw = self._sync_timeout_ms
            if raw is None:
                raw = self._config.sync_timeout_ms
            timeout_ms = (
                max(MIN_SYNC_TIMEOUT_MS, int(raw)) if raw is not None else MIN_SYNC_TIMEOUT_MS
            )
        self._engine = SyncEngine(
            client=client,
            store=self._store,
            sync_timeout_ms=timeout_ms,
            logger=logger,
            on_maintenance=self._after_sync,
        )
        self._engine.start()
        logger.info("NioTransport: sync loop started component=matrix")

    async def stop(self) -> None:
        """Stop the sync loop and close the client (idempotent)."""
        if self._engine is not None:
            await self._engine.stop()
            self._engine = None
        if self._callback_registered and self._client is not None:
            callbacks = getattr(self._client, "event_callbacks", None)
            if callbacks is not None:
                # nio stores ClientCallback dataclasses; duck-typed test
                # clients may store plain (callback, filter) tuples.
                for entry in list(callbacks):
                    if isinstance(entry, tuple):
                        cb = entry[0]
                    else:
                        cb = (
                            getattr(entry, "func", None)
                            or getattr(entry, "callback", None)
                        )
                    if cb == self._on_room_event:
                        try:
                            callbacks.remove(entry)
                        except ValueError:
                            pass
                        break
            self._callback_registered = False
        if self._client is not None and not self._client_closed:
            await self._client.close()
            self._client_closed = True

    # -- inbound ------------------------------------------------------------

    async def _on_room_event(self, room: Any, event: Any) -> None:
        """nio room-event callback: normalise and deliver.

        A failing normalisation or handler is logged and swallowed so
        one bad event can never break the sync loop.
        """
        try:
            inbound = normalize_room_event(room, event)
        except Exception as exc:
            logger.warning(
                "NioTransport: event normalisation failed "
                "event_id=%s: %s",
                getattr(event, "event_id", "?"),
                exc,
            )
            return
        if inbound is None or self._on_event is None:
            return
        try:
            await self._on_event(inbound)
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            logger.warning(
                "NioTransport: inbound handler failed event_id=%s: %s",
                inbound.event_id,
                exc,
            )

    async def _after_sync(self) -> None:
        """After each successful sync: E2EE key maintenance, then the hook."""
        if self._maintainer is not None:
            await self._maintainer.run_once()
        if self._on_sync is not None:
            result = self._on_sync()
            if inspect.isawaitable(result):
                await result

    # -- outbound -----------------------------------------------------------

    def _require_client(self) -> AsyncClient:
        if self._client is None:
            raise RuntimeError(
                "NioTransport has no client (call start() first)"
            )
        return self._client

    async def send(self, msg: OutboundMessage) -> str:
        """Send a message and return the resulting Matrix event id."""
        client = self._require_client()
        msgtype = "m.notice" if msg.kind == "tool" else "m.text"
        content: Dict[str, Any] = {"msgtype": msgtype, "body": msg.text}
        if msg.reply_to:
            content["m.in_reply_to"] = {"event_id": msg.reply_to}
        resp = await client.room_send(
            msg.room_id,
            "m.room.message",
            content,
            tx_id=str(uuid4()),
            ignore_unverified_devices=True,
        )
        if not isinstance(resp, RoomSendResponse):
            raise RuntimeError(
                f"room_send failed for {msg.room_id}: {type(resp).__name__}"
            )
        return resp.event_id

    async def edit(self, room_id: str, event_id: str, text: str) -> None:
        """Edit a previously sent message (``m.replace``)."""
        client = self._require_client()
        content: Dict[str, Any] = {
            "msgtype": "m.text",
            "body": f"* {text}",
            "m.new_content": {"msgtype": "m.text", "body": text},
            "m.relates_to": {"rel_type": "m.replace", "event_id": event_id},
        }
        resp = await client.room_send(
            room_id,
            "m.room.message",
            content,
            tx_id=str(uuid4()),
            ignore_unverified_devices=True,
        )
        if not isinstance(resp, RoomSendResponse):
            raise RuntimeError(
                f"edit of {event_id} failed: {type(resp).__name__}"
            )

    async def react(self, room_id: str, event_id: str, key: str) -> None:
        """Add an annotation reaction to an event (``m.reaction``)."""
        client = self._require_client()
        content: Dict[str, Any] = {"m.annotation": {"key": key}}
        resp = await client.room_send(
            room_id,
            "m.reaction",
            content,
            tx_id=str(uuid4()),
            ignore_unverified_devices=True,
        )
        if not isinstance(resp, RoomSendResponse):
            raise RuntimeError(
                f"reaction on {event_id} failed: {type(resp).__name__}"
            )

    async def typing(self, room_id: str, state: str) -> None:
        """Set the typing indicator (``state`` must be in TYPING_STATES)."""
        if state not in TYPING_STATES:
            raise ValueError(
                f"typing state must be one of {TYPING_STATES}, got {state!r}"
            )
        client = self._require_client()
        resp = await client.room_typing(
            room_id, typing_state=state == "start", timeout=TYPING_TIMEOUT_MS
        )
        if not isinstance(resp, RoomTypingResponse):
            raise RuntimeError(
                f"room_typing failed for {room_id}: {type(resp).__name__}"
            )

    async def download(self, media: MediaRef) -> bytes:
        """Download one media object and return its bytes."""
        client = self._require_client()
        filename = media.file_name or media.mxc_uri.rsplit("/", 1)[-1]
        path = await self._media_cache.fetch_to_file(media.mxc_uri, filename)
        if path is None:
            raise RuntimeError(f"failed to download media {media.mxc_uri}")
        return path.read_bytes()

    async def _fetch_media_bytes(self, mxc_uri: str) -> bytes:
        """Authenticated fetch for the :class:`MediaCache`.

        nio 0.26 ``AsyncClient.download`` returns a response object
        (``MemoryDownloadResponse`` / ``DiskDownloadResponse``) whose
        ``body`` holds the bytes.
        """
        client = self._require_client()
        resp = await client.download(mxc_uri)
        body = getattr(resp, "body", None)
        if isinstance(body, (bytes, bytearray)):
            return bytes(body)
        raise RuntimeError(
            f"media download failed for {mxc_uri}: {type(resp).__name__}"
        )
