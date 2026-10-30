"""AgentTeams Matrix platform adapter for Hermes Agent (P9, PR1 M6).

Second runtime of the ``agentteams-matrix`` bundle: the same
runtime-neutral core package the QwenPaw channel uses
(``plugins/agentteams-matrix/core``), bridged into the Hermes gateway
as a platform plugin.  The adapter owns one in-process
:class:`core.transport.NioTransport`:

* **transport** — client created by :func:`core.transport.build_client`
  and injected into the transport (client-injection path: the adapter
  keeps the client reference for the joined-members DM probe, read
  receipts, display names and media upload, which the core transport
  does not own);
* **inbound** — every core :class:`~core.interfaces.InboundEvent` is
  bridged to the gateway through
  :meth:`BasePlatformAdapter.handle_message` with the QwenPaw
  channel's behaviour: DM detection (joined_members + 30 s cache,
  room.users/member_count fallback), mention gating in group rooms
  (default on), history buffering for unmentioned group messages
  (``core.history.RoomHistory``), read receipt + typing feedback,
  mention-prefix strip and the ``NO_REPLY`` drop protocol;
* **outbound** — text via :meth:`NioTransport.send`, media via
  ``client.upload`` + ``room_send`` (the core transport has no upload
  surface — the client-injection path exists for exactly this), typing
  via :meth:`NioTransport.typing`.

Configuration is env-var driven (see ``plugin.yaml``).  Secrets are
read through ``gateway.platforms._shared.get_scoped_secret`` — never
``os.getenv`` — and ``env_enablement_fn`` seeds
``PlatformConfig.extra`` from env before construction so env-only
setups surface in ``hermes gateway status``.
"""

import asyncio
import contextlib
import datetime
import io
import logging
import mimetypes
import os
import sys
import tempfile
import time
import urllib.request
import uuid
from pathlib import Path
from typing import Any, Dict, List, Optional

import nio
from nio import (
    JoinedMembersResponse,
    RoomSendResponse,
    UploadResponse,
)

from gateway.config import Platform
from gateway.platforms._shared import (
    get_scoped_secret as _get_scoped_secret,
    seed_extra_from_env as _seed_extra_from_env,
    send_error,
)
from gateway.platforms.base import BasePlatformAdapter, SendResult
from gateway.platforms.event import MessageEvent, MessageType

# The runtime-neutral core package lives at the bundle root
# (``plugins/agentteams-matrix/core``) in the source tree and next to
# this file in the installed layout (install.sh copies ``core/`` into
# the plugin directory).  Try both; fail closed when neither provides
# the package.
_here = Path(__file__).resolve()
for _base in (_here.parents[0], _here.parents[2]):
    if (_base / "core" / "__init__.py").is_file():
        if str(_base) not in sys.path:
            sys.path.insert(0, str(_base))
        break
else:
    raise ImportError(
        "agentteams-matrix: the core package is missing; the bundle's "
        "core/ directory must be installed next to the plugin "
        "(install.sh copies it)"
    )

from core.e2ee import crypto_available  # noqa: E402
from core.history import (  # noqa: E402
    DEFAULT_HISTORY_LIMIT,
    HistoryEntry,
    RoomHistory,
)
from core.interfaces import InboundEvent, OutboundMessage  # noqa: E402
from core.mentions import strip_mention_prefix, text_mentions_worker  # noqa: E402
from core.state import FileSessionStore, StateLayout  # noqa: E402
from core.transport import (  # noqa: E402
    MIN_SYNC_TIMEOUT_MS,
    NioTransport,
    TransportConfig,
    build_client,
)

logger = logging.getLogger("agentteams.matrix.hermes")

PLATFORM_NAME = "agentteams_matrix"
PLATFORM_LABEL = "AgentTeams Matrix"

ENV_SERVER = "AGENTTEAMS_MATRIX_SERVER"
ENV_USER_ID = "AGENTTEAMS_MATRIX_USER_ID"
ENV_TOKEN = "AGENTTEAMS_MATRIX_TOKEN"
ENV_DEVICE_ID = "AGENTTEAMS_MATRIX_DEVICE_ID"
ENV_STATE_DIR = "AGENTTEAMS_MATRIX_STATE_DIR"
ENV_ENCRYPTION = "AGENTTEAMS_MATRIX_ENCRYPTION"
ENV_HISTORY_LIMIT = "AGENTTEAMS_MATRIX_HISTORY_LIMIT"
ENV_SYNC_TIMEOUT_MS = "AGENTTEAMS_MATRIX_SYNC_TIMEOUT_MS"
ENV_HOME_ROOM = "AGENTTEAMS_MATRIX_HOME_ROOM"
ENV_ALLOWED_USERS = "AGENTTEAMS_MATRIX_ALLOWED_USERS"
ENV_ALLOW_ALL_USERS = "AGENTTEAMS_MATRIX_ALLOW_ALL_USERS"

_TRUTHY = {"1", "true", "yes"}
_NO_REPLY = "NO_REPLY"
# QwenPaw parity: safe single-text budget under the 64 KiB Matrix event
# protocol limit (3/4 of the limit minus 1 KiB of protocol overhead).
MAX_MESSAGE_LENGTH = (64 * 1024 * 3) // 4 - 1024
DM_CACHE_TTL_MS = 30_000
DEFAULT_SYNC_TIMEOUT_MS = 30_000
_DOWNLOAD_TIMEOUT_S = 30


# ── Config helpers (env wins over config.yaml extra) ────────────────────


def _env_or_extra(extra: dict, env: str, key: str, default: Any = "") -> Any:
    """Env var overrides config.yaml ``extra`` (same pattern as the IRC adapter)."""
    return _get_scoped_secret(env) or extra.get(key, default)


def _extra_bool(extra: dict, env: str, key: str, default: bool = False) -> bool:
    raw = _get_scoped_secret(env)
    if raw is None or str(raw).strip() == "":
        raw = extra.get(key, default)
    if isinstance(raw, bool):
        return raw
    return str(raw).strip().lower() in _TRUTHY


def _extra_int(
    extra: dict, env: str, key: str, default: Optional[int] = None
) -> Optional[int]:
    raw = _get_scoped_secret(env)
    if raw is None or str(raw).strip() == "":
        raw = extra.get(key)
    if raw is None or str(raw).strip() == "":
        return default
    try:
        return int(str(raw).strip())
    except ValueError:
        return default


def _home_room(extra: dict) -> str:
    """Cron / notification home room.

    Env wins; the ``home_channel`` extra value may be a plain room id
    (config.yaml) or the ``{"chat_id", "name"}`` dict that
    ``seed_extra_from_env`` seeds under env enablement.
    """
    raw = _env_or_extra(extra, ENV_HOME_ROOM, "home_channel")
    if isinstance(raw, dict):
        raw = raw.get("chat_id", "")
    return str(raw or "").strip()


def _resolve_state_dir(extra: dict) -> Path:
    """State directory: env/extra override, else ``<HERMES_HOME>/state/agentteams_matrix``."""
    raw = _env_or_extra(extra, ENV_STATE_DIR, "store_path")
    if raw:
        return Path(str(raw)).expanduser()
    hermes_home = os.environ.get("HERMES_HOME", "").strip()
    base = Path(hermes_home).expanduser() if hermes_home else Path.home() / ".hermes"
    return base / "state" / "agentteams_matrix"


def _download_url(url: str, dest: Path) -> None:
    with urllib.request.urlopen(url, timeout=_DOWNLOAD_TIMEOUT_S) as resp:
        dest.write_bytes(resp.read())


# ── History text-part shim ───────────────────────────────────────────────
# core.history.apply_history_to_parts operates on content parts with a
# ``.text`` attribute; the gateway event is a single text string, so the
# adapter feeds it one duck-typed part (the merged prefix comes back on
# parts[0]).  The marker composition itself stays in core.


class _TextPart:
    __slots__ = ("text",)

    def __init__(self, text: str) -> None:
        self.text = text


def _is_text_part(part: Any) -> bool:
    return isinstance(part, _TextPart)


def _make_text_part(text: str) -> _TextPart:
    return _TextPart(text)


# ── Adapter ─────────────────────────────────────────────────────────────


class AgentTeamsMatrixAdapter(BasePlatformAdapter):
    """Hermes platform adapter: one in-process core NioTransport per bot."""

    def __init__(self, config, **kwargs):
        super().__init__(config=config, platform=Platform(PLATFORM_NAME))
        extra = getattr(config, "extra", {}) or {}
        self.homeserver = str(
            _env_or_extra(extra, ENV_SERVER, "homeserver")
        ).rstrip("/")
        self.user_id = str(_env_or_extra(extra, ENV_USER_ID, "user_id"))
        self.access_token = str(_env_or_extra(extra, ENV_TOKEN, "access_token"))
        self.device_id = str(_env_or_extra(extra, ENV_DEVICE_ID, "device_id"))
        self.encryption = _extra_bool(extra, ENV_ENCRYPTION, "encryption", False)
        self.history_limit = max(
            0,
            _extra_int(
                extra, ENV_HISTORY_LIMIT, "history_limit", DEFAULT_HISTORY_LIMIT
            )
            or DEFAULT_HISTORY_LIMIT,
        )
        self.sync_timeout_ms = max(
            MIN_SYNC_TIMEOUT_MS,
            int(
                _extra_int(
                    extra,
                    ENV_SYNC_TIMEOUT_MS,
                    "sync_timeout_ms",
                    DEFAULT_SYNC_TIMEOUT_MS,
                )
                or DEFAULT_SYNC_TIMEOUT_MS
            ),
        )
        self.require_mention = _extra_bool(
            extra, "AGENTTEAMS_MATRIX_REQUIRE_MENTION", "require_mention", True
        )
        self.home_room = _home_room(extra)
        self.state_dir = _resolve_state_dir(extra)
        self._client: Optional[Any] = None
        self._transport: Optional[NioTransport] = None
        self._room_history = RoomHistory(limit=self.history_limit)
        self._dm_room_cache: Dict[str, Dict[str, Any]] = {}

    @property
    def name(self) -> str:
        return PLATFORM_LABEL

    def _fail(self, code: str, message: str, *, retryable: bool) -> bool:
        self._set_fatal_error(code, message, retryable=retryable)
        return False

    # -- lifecycle --------------------------------------------------------

    async def connect(self, *, is_reconnect: bool = False) -> bool:
        """Build the core transport in-process and start the sync loop."""
        if not (self.homeserver and self.user_id and self.access_token):
            logger.error(
                "AgentTeams Matrix: homeserver, user id and access token "
                "must be configured"
            )
            return self._fail(
                "config_missing",
                f"{ENV_SERVER}, {ENV_USER_ID} and {ENV_TOKEN} must be set",
                retryable=False,
            )
        if not self._acquire_platform_lock(
            PLATFORM_NAME,
            f"{self.homeserver}:{self.user_id}",
            f"Matrix identity {self.user_id}@{self.homeserver}",
        ):
            return False
        try:
            encryption = self.encryption and crypto_available()
            if self.encryption and not encryption:
                # Same wording as the QwenPaw channel preflight.
                logger.error(
                    "AgentTeams Matrix: no Matrix E2EE backend "
                    "(vodozemac/olm) — falling back to non-encrypted "
                    "mode. Install matrix-nio[e2e]; on matrix-nio < 0.26 "
                    "also install libolm (apt/dnf install libolm-dev).",
                )
            self.state_dir.mkdir(parents=True, exist_ok=True)
            config = TransportConfig(
                homeserver=self.homeserver,
                user_id=self.user_id,
                access_token=self.access_token,
                device_id=self.device_id,
                # The client-injection path does not resolve the E2EE
                # store path itself (the NioTransport build path does);
                # mirror it so the injected client persists its key
                # store under <state_dir>/e2ee.
                store_path=StateLayout(self.state_dir).e2ee if encryption else None,
                encryption=encryption,
                sync_timeout_ms=self.sync_timeout_ms,
            )
            client = build_client(config)
            self._client = client
            transport = NioTransport(
                config,
                store=FileSessionStore(self.state_dir),
                state_dir=self.state_dir,
                sync_timeout_ms=self.sync_timeout_ms,
                encryption=encryption,
                client=client,
            )
            await transport.start(self._on_inbound)
            self._transport = transport
        except Exception as e:
            logger.error("AgentTeams Matrix: failed to start core transport — %s", e)
            await self._close_client()
            return self._fail("connect_failed", str(e), retryable=True)
        self._mark_connected()
        logger.info(
            "AgentTeams Matrix: connected to %s as %s",
            self.homeserver,
            self.user_id,
        )
        self._wire_plugin_handlers(None)
        return True

    async def _close_client(self) -> None:
        client, self._client = self._client, None
        if client is not None:
            with contextlib.suppress(Exception):
                await client.close()

    async def disconnect(self) -> None:
        """Stop the sync loop and close the client."""
        transport, self._transport = self._transport, None
        if transport is not None:
            with contextlib.suppress(Exception):
                await transport.stop()
        # transport.stop() closes the injected client; the direct close
        # covers a transport that never started.
        await self._close_client()
        with contextlib.suppress(Exception):
            self._release_platform_lock()
        self._mark_disconnected()

    # -- outbound ---------------------------------------------------------

    async def send(
        self,
        chat_id: str,
        content: str,
        reply_to: Optional[str] = None,
        metadata: Optional[Dict[str, Any]] = None,
    ) -> SendResult:
        if self._transport is None:
            return SendResult(success=False, error="Not connected")
        try:
            event_id = await self._transport.send(
                OutboundMessage(room_id=chat_id, text=content, reply_to=reply_to)
            )
        except Exception as e:
            logger.warning("AgentTeams Matrix: send failed to %s: %s", chat_id, e)
            return SendResult(success=False, error=str(e))
        return SendResult(success=True, message_id=event_id)

    async def send_typing(self, chat_id: str, metadata=None) -> None:
        if self._transport is None:
            return
        try:
            await self._transport.typing(chat_id, "start")
        except Exception as e:
            logger.debug("AgentTeams Matrix: typing failed for %s: %s", chat_id, e)

    async def get_chat_info(self, chat_id: str) -> Dict[str, Any]:
        name = chat_id
        room = self._client.rooms.get(chat_id) if self._client is not None else None
        if room is not None:
            for attr in ("name", "display_name"):
                value = getattr(room, attr, "")
                if isinstance(value, str) and value.strip():
                    name = value.strip()
                    break
        cached = self._dm_room_cache.get(chat_id) or {}
        members = cached.get("members") or []
        is_dm = len(members) == 2 and bool(self.user_id) and self.user_id in members
        return {"name": name, "type": "dm" if is_dm else "group", "chat_id": chat_id}

    # -- outbound media (client.upload + room_send; the core transport
    #    has no upload surface, hence the injected client reference) -----

    async def send_image(
        self,
        chat_id: str,
        image_url: str,
        caption: Optional[str] = None,
        reply_to: Optional[str] = None,
        metadata: Optional[Dict[str, Any]] = None,
    ) -> SendResult:
        """Download *image_url* and send it as an image message."""
        if not image_url:
            return SendResult(success=False, error="empty image url")
        try:
            with tempfile.TemporaryDirectory(prefix="agentteams-matrix-") as tmp:
                dest = Path(tmp) / "image"
                await asyncio.to_thread(_download_url, image_url, dest)
                if not dest.is_file():
                    return SendResult(
                        success=False, error=f"download failed: {image_url}"
                    )
                return await self._send_media_file(chat_id, str(dest), "m.image", caption or "")
        except Exception as e:
            logger.warning("AgentTeams Matrix: image download failed: %s", e)
            return SendResult(success=False, error=str(e))

    async def send_image_file(
        self,
        chat_id: str,
        image_path: str,
        caption: Optional[str] = None,
        reply_to: Optional[str] = None,
        metadata: Optional[Dict[str, Any]] = None,
        **kwargs: Any,
    ) -> SendResult:
        return await self._send_media_file(chat_id, image_path, "m.image", caption or "")

    async def send_document(
        self,
        chat_id: str,
        file_path: str,
        caption: Optional[str] = None,
        file_name: Optional[str] = None,
        reply_to: Optional[str] = None,
        metadata: Optional[Dict[str, Any]] = None,
        **kwargs: Any,
    ) -> SendResult:
        return await self._send_media_file(chat_id, file_path, "m.file", caption or "")

    async def send_video(
        self,
        chat_id: str,
        video_path: str,
        caption: Optional[str] = None,
        reply_to: Optional[str] = None,
        metadata: Optional[Dict[str, Any]] = None,
        **kwargs: Any,
    ) -> SendResult:
        return await self._send_media_file(chat_id, video_path, "m.video", caption or "")

    async def _send_media_file(
        self, chat_id: str, path: str, msgtype: str, caption: str
    ) -> SendResult:
        if self._client is None:
            return SendResult(success=False, error="Not connected")
        src = Path(path).expanduser()
        if not src.is_file():
            return SendResult(success=False, error=f"file not found: {path}")
        mxc_uri = await self._upload_file(src)
        if not mxc_uri:
            return SendResult(success=False, error=f"upload failed: {path}")
        content: Dict[str, Any] = {
            "msgtype": msgtype,
            "body": caption or src.name,
            "url": mxc_uri,
        }
        info: Dict[str, Any] = {}
        try:
            size = src.stat().st_size
        except OSError:
            size = None
        if size is not None:
            info["size"] = size
        mime_type, _ = mimetypes.guess_type(str(src))
        if mime_type:
            info["mimetype"] = mime_type
        if info:
            content["info"] = info
        try:
            resp = await self._client.room_send(
                chat_id,
                "m.room.message",
                content,
                tx_id=str(uuid.uuid4()),
                ignore_unverified_devices=True,
            )
        except Exception as e:
            logger.warning("AgentTeams Matrix: media send failed to %s: %s", chat_id, e)
            return SendResult(success=False, error=str(e))
        if not isinstance(resp, RoomSendResponse):
            return SendResult(
                success=False, error=f"room_send failed: {type(resp).__name__}"
            )
        return SendResult(success=True, message_id=resp.event_id)

    async def _upload_file(self, path: Path) -> Optional[str]:
        """Upload a local file to Matrix; return mxc:// URI or None.

        QwenPaw parity: same upload kwargs, same warning wording.
        """
        if self._client is None:
            return None
        try:
            mime_type, _ = mimetypes.guess_type(str(path))
            mime_type = mime_type or "application/octet-stream"
            data = path.read_bytes()
            resp, _ = await self._client.upload(
                io.BytesIO(data),
                content_type=mime_type,
                filename=path.name,
                filesize=len(data),
            )
            if isinstance(resp, UploadResponse):
                logger.debug(
                    "AgentTeams Matrix: uploaded %s → %s",
                    path.name,
                    resp.content_uri,
                )
                return resp.content_uri
            logger.warning("AgentTeams Matrix: upload failed: %s", resp)
            return None
        except Exception as exc:
            logger.warning(
                "AgentTeams Matrix: upload error for %s: %s", path, exc
            )
            return None

    # -- inbound ------------------------------------------------------------

    async def _on_inbound(self, inbound: InboundEvent) -> None:
        """Core InboundEvent → gateway MessageEvent (QwenPaw channel parity)."""
        room_id = inbound.room_id
        sender_id = inbound.sender
        # Skip own messages early (prevent reply loops).
        if sender_id == self.user_id:
            return
        if inbound.kind not in ("text", "media"):
            return  # edits / redactions are not agent triggers
        text = inbound.text or ""
        is_dm = await self._is_dm_room(room_id, sender_id)
        is_thread = inbound.thread_root is not None
        logger.info(
            "_on_inbound: sender=%s room=%s kind=%s is_dm=%s body=%r",
            sender_id,
            room_id,
            inbound.kind,
            is_dm,
            text[:80],
        )
        # Mention gate for group rooms (require mention by default).
        if not is_dm and self.require_mention and not self._was_mentioned(inbound):
            # Thread events that don't mention us are silently ignored
            # (qwenpaw parity); plain group events go to history.
            if is_thread:
                return
            logger.info(
                "AgentTeams Matrix: group %s not mentioned, cached to "
                "history (room=%s sender=%s)",
                inbound.kind,
                room_id,
                sender_id,
            )
            self._record_history(room_id, sender_id, inbound)
            return

        # Mark as read + start typing immediately so the sender sees feedback.
        await self._send_read_receipt(room_id, inbound.event_id)
        await self._notify_typing(room_id, True)

        # Strip the leading @mention so slash commands and NO_REPLY are
        # detected regardless of room type.
        stripped = self._strip_mention_prefix(text, room_id)

        # NO_REPLY protocol: the sender explicitly signals "nothing to
        # say"; drop silently to prevent infinite ping-pong between agents.
        if stripped.strip() == _NO_REPLY:
            logger.info(
                "AgentTeams Matrix: received NO_REPLY from %s in %s, ignoring",
                sender_id,
                room_id,
            )
            await self._notify_typing(room_id, False)
            return

        # Group context: prefix the sender identity so the LLM can tell
        # participants apart, and prepend accumulated history (skipped
        # for threads — they carry their own context; skipped for slash
        # commands — the gateway's command parser needs a leading "/").
        command_text = stripped
        if not is_dm and not command_text.startswith("/"):
            sender_name = self._display_name(room_id, sender_id)
            command_text = f"{sender_name}: {command_text}"
            if not is_thread:
                parts = self._room_history.apply_history_to_parts(
                    room_id,
                    [_TextPart(command_text)],
                    _is_text_part,
                    _make_text_part,
                )
                command_text = parts[0].text

        if not self._message_handler:
            return
        if inbound.ts:
            timestamp = datetime.datetime.fromtimestamp(
                inbound.ts / 1000, tz=datetime.timezone.utc
            )
        else:
            timestamp = datetime.datetime.now(datetime.timezone.utc)
        source = self.build_source(
            chat_id=room_id,
            chat_name=self._room_name(room_id) or room_id,
            chat_type="dm" if is_dm else "group",
            user_id=sender_id,
            user_name=self._display_name(room_id, sender_id),
        )
        await self.handle_message(
            MessageEvent(
                text=command_text,
                message_type=MessageType.TEXT,
                source=source,
                message_id=inbound.event_id,
                timestamp=timestamp,
            )
        )
        if not is_dm and not is_thread:
            self._room_history.clear_history(room_id)

    def _was_mentioned(self, inbound: InboundEvent) -> bool:
        """Mention gate on the normalized InboundEvent.

        ``inbound.mentions`` carries the full-MXID mentions the core
        extracted from the event text; :func:`text_mentions_worker`
        adds the bare ``@localpart`` boundary form.  Structured
        ``m.mentions`` / formatted-body matrix.to links are not part of
        the InboundEvent surface, so the gate is text-form only — a
        documented parity gap versus the QwenPaw raw-event path.
        """
        if not self.user_id:
            return False
        if self.user_id in (inbound.mentions or ()):
            return True
        worker_localpart = self.user_id.split(":")[0].lstrip("@")
        return text_mentions_worker(self.user_id, inbound.text or "", worker_localpart)

    def _strip_mention_prefix(self, text: str, room_id: str) -> str:
        """Strip the leading @mention prefix so slash commands are detected.

        The bot's display name is resolved from the room (qwenpaw
        parity); the stripping itself is :func:`core.mentions.strip_mention_prefix`.
        """
        if not self.user_id:
            return text
        display_name = self._display_name(room_id, self.user_id)
        return strip_mention_prefix(self.user_id, text, display_name)

    def _record_history(
        self, room_id: str, sender_id: str, inbound: InboundEvent
    ) -> None:
        self._room_history.record_history(
            room_id,
            HistoryEntry(
                sender=self._display_name(room_id, sender_id),
                body=inbound.text or "",
                timestamp=inbound.ts or None,
                message_id=inbound.event_id,
                sender_id=sender_id,
            ),
        )

    async def _send_read_receipt(self, room_id: str, event_id: str) -> None:
        if self._client is None or not event_id:
            return
        try:
            await self._client.room_read_markers(
                room_id,
                fully_read_event=event_id,
                read_event=event_id,
            )
        except Exception as exc:
            logger.debug(
                "AgentTeams Matrix: read receipt failed for %s: %s",
                event_id,
                exc,
            )

    async def _notify_typing(self, room_id: str, active: bool) -> None:
        if self._transport is None:
            return
        try:
            await self._transport.typing(room_id, "start" if active else "stop")
        except Exception as exc:
            logger.debug(
                "AgentTeams Matrix: typing %s failed for %s: %s",
                "start" if active else "stop",
                room_id,
                exc,
            )

    # -- display names ------------------------------------------------------

    def _room_name(self, room_id: str) -> str:
        room = self._client.rooms.get(room_id) if self._client is not None else None
        if room is None:
            return ""
        for attr in ("name", "display_name"):
            value = getattr(room, attr, "")
            if isinstance(value, str) and value.strip():
                return value.strip()
        return ""

    def _display_name(self, room_id: str, user_id: str) -> str:
        """Best-effort human-readable name for *user_id* in *room_id*.

        QwenPaw parity: room.user_name first, then the client.rooms
        lookup, then the MXID localpart.
        """
        client = self._client
        room = client.rooms.get(room_id) if client is not None else None
        if room is not None:
            try:
                name = room.user_name(user_id)
                if name:
                    return name
            except Exception as exc:
                logger.debug(
                    "AgentTeams Matrix: user_name failed for %s: %s",
                    user_id,
                    exc,
                )
        return user_id.split(":")[0].lstrip("@") or user_id

    # -- DM room detection (joined_members API + short-lived cache) ---------

    def _looks_like_teamharness_task_room(self, room_id: str) -> bool:
        """Room state looks like a TeamHarness task room (group semantics)."""
        room = self._client.rooms.get(room_id) if self._client is not None else None
        if not room:
            return False
        for attr in ("topic", "name", "display_name"):
            value = getattr(room, attr, "")
            if callable(value):
                continue
            text = str(value or "").strip()
            if text.startswith("Task room for ") or "Task room for " in text:
                return True
        return False

    def _is_dm_room_fallback(self, room_id: str, sender_id: str) -> bool:
        """Best-effort DM check when the joined_members API is unavailable."""
        if not self.user_id:
            return False
        room = self._client.rooms.get(room_id) if self._client is not None else None
        if not room:
            return False
        try:
            users = list(getattr(room, "users", {}).keys())
            if users:
                return (
                    len(users) == 2
                    and self.user_id in users
                    and sender_id in users
                )
            member_count = int(getattr(room, "member_count", 0) or 0)
            return member_count == 2
        except Exception:
            return False

    async def _is_dm_room(self, room_id: str, sender_id: str) -> bool:
        """Check if *room_id* is a DM between self and *sender_id*.

        Uses the Matrix joined_members API (nio's room.users can be
        unreliable after token restore) with a 30 s cache and the
        room.users / member_count fallback — QwenPaw parity.
        """
        if self._client is None or not self.user_id:
            return False
        if self._looks_like_teamharness_task_room(room_id):
            logger.debug(
                "AgentTeams Matrix: room=%s is a TeamHarness task room; "
                "treating as group semantics",
                room_id,
            )
            return False
        now = int(time.time() * 1000)
        cached = self._dm_room_cache.get(room_id)
        if cached and (now - cached["ts"]) < DM_CACHE_TTL_MS:
            members = cached["members"]
            return (
                len(members) == 2
                and self.user_id in members
                and sender_id in members
            )
        try:
            resp = await self._client.joined_members(room_id)
            if isinstance(resp, JoinedMembersResponse):
                members = [m.user_id for m in resp.members]
                self._dm_room_cache[room_id] = {"members": members, "ts": now}
                return (
                    len(members) == 2
                    and self.user_id in members
                    and sender_id in members
                )
            logger.warning(
                "AgentTeams Matrix: joined_members failed for %s: %s",
                room_id,
                resp,
            )
            return self._is_dm_room_fallback(room_id, sender_id)
        except Exception as exc:
            logger.warning(
                "AgentTeams Matrix: joined_members error for %s: %s",
                room_id,
                exc,
            )
            return self._is_dm_room_fallback(room_id, sender_id)


# ── Plugin registration ─────────────────────────────────────────────────


def check_requirements() -> bool:
    """Check that the platform is configured (env vars resolvable)."""
    return bool(
        _get_scoped_secret(ENV_SERVER, "")
        and _get_scoped_secret(ENV_USER_ID, "")
        and _get_scoped_secret(ENV_TOKEN, "")
    )


def validate_config(config) -> bool:
    """Validate that the platform config (env or config.yaml) can connect."""
    extra = getattr(config, "extra", {}) or {}
    return bool(
        _env_or_extra(extra, ENV_SERVER, "homeserver")
        and _env_or_extra(extra, ENV_USER_ID, "user_id")
        and _env_or_extra(extra, ENV_TOKEN, "access_token")
    )


def is_connected(config) -> bool:
    """Check whether the platform is configured (env or config.yaml)."""
    return validate_config(config)


def _env_enablement() -> dict | None:
    """``env_enablement_fn``: seed ``PlatformConfig.extra`` from the profile's env
    BEFORE adapter construction; ``None`` when the platform isn't minimally
    configured. Secrets also land in extra (env wins at construct time, same
    as the IRC adapter); ``home_channel`` carries the cron delivery room."""
    server = _get_scoped_secret(ENV_SERVER, "").strip()
    user_id = _get_scoped_secret(ENV_USER_ID, "").strip()
    token = _get_scoped_secret(ENV_TOKEN, "").strip()
    if not (server and user_id and token):
        return None
    seed = _seed_extra_from_env(
        (
            (ENV_DEVICE_ID, "device_id", None),
            (ENV_STATE_DIR, "store_path", None),
            (ENV_ENCRYPTION, "encryption", lambda v: str(v).strip().lower() in _TRUTHY),
            (ENV_HISTORY_LIMIT, "history_limit", int),
            (ENV_SYNC_TIMEOUT_MS, "sync_timeout_ms", int),
        ),
        home_env=ENV_HOME_ROOM,
    )
    return {
        "homeserver": server,
        "user_id": user_id,
        "access_token": token,
        **seed,
    }


def _resolve_cron_room(pconfig, chat_id: str) -> str:
    extra = getattr(pconfig, "extra", {}) or {}
    raw = chat_id or _env_or_extra(extra, ENV_HOME_ROOM, "home_channel")
    # ``home_channel`` seeded by ``seed_extra_from_env`` is a
    # ``{"chat_id", "name"}`` dict, not a plain room id.
    if isinstance(raw, dict):
        raw = raw.get("chat_id", "")
    target = str(raw or "").strip()
    # Reject control characters in the room target.
    if any(ch in target for ch in ("\r", "\n", "\x00")):
        return ""
    return target


async def _standalone_send(
    pconfig,
    chat_id: str,
    message: str,
    *,
    thread_id: Optional[str] = None,
    media_files: Optional[List[str]] = None,
    force_document: bool = False,
) -> Dict[str, Any]:
    """Out-of-process delivery (cron jobs running separately from the gateway).

    Opens an ephemeral matrix-nio client, sends one message and closes.
    ``thread_id`` / ``media_files`` / ``force_document`` are accepted for
    signature parity only (text delivery is the cron surface).
    """
    extra = getattr(pconfig, "extra", {}) or {}
    homeserver = str(_env_or_extra(extra, ENV_SERVER, "homeserver") or "").rstrip("/")
    user_id = str(_env_or_extra(extra, ENV_USER_ID, "user_id") or "")
    token = str(_env_or_extra(extra, ENV_TOKEN, "access_token") or "")
    if not (homeserver and user_id and token):
        return send_error(
            f"agentteams-matrix: {ENV_SERVER}, {ENV_USER_ID} and {ENV_TOKEN} "
            "must be configured"
        )
    target = _resolve_cron_room(pconfig, chat_id)
    if not target:
        return send_error("agentteams-matrix: chat_id is required")
    encryption = _extra_bool(extra, ENV_ENCRYPTION, "encryption", False)
    if encryption:
        encryption = crypto_available()
    state_dir = _resolve_state_dir(extra)
    state_dir.mkdir(parents=True, exist_ok=True)
    config = TransportConfig(
        homeserver=homeserver,
        user_id=user_id,
        access_token=token,
        device_id=str(_env_or_extra(extra, ENV_DEVICE_ID, "device_id") or ""),
        store_path=StateLayout(state_dir).e2ee if encryption else None,
        encryption=encryption,
        sync_timeout_ms=DEFAULT_SYNC_TIMEOUT_MS,
    )
    try:
        client = build_client(config)
        try:
            resp = await client.room_send(
                target,
                "m.room.message",
                {"msgtype": "m.text", "body": message},
                tx_id=str(uuid.uuid4()),
                ignore_unverified_devices=True,
            )
        finally:
            await client.close()
    except asyncio.CancelledError:
        raise
    except Exception as e:
        logger.debug("agentteams-matrix standalone send raised", exc_info=True)
        return send_error(f"agentteams-matrix standalone send failed: {e}")
    if not isinstance(resp, RoomSendResponse):
        return send_error(f"agentteams-matrix: room_send failed: {type(resp).__name__}")
    return {"success": True, "message_id": resp.event_id}


def register(ctx):
    """Plugin entry point: called by the Hermes plugin system."""
    ctx.register_platform(
        name=PLATFORM_NAME,
        label=PLATFORM_LABEL,
        adapter_factory=AgentTeamsMatrixAdapter,
        check_fn=check_requirements,
        validate_config=validate_config,
        is_connected=is_connected,
        required_env=[ENV_SERVER, ENV_USER_ID, ENV_TOKEN],
        install_hint="pip install 'matrix-nio[e2e]'  # plain matrix-nio without E2EE",
        env_enablement_fn=_env_enablement,
        cron_deliver_env_var=ENV_HOME_ROOM,
        standalone_sender_fn=_standalone_send,
        allowed_users_env=ENV_ALLOWED_USERS,
        allow_all_env=ENV_ALLOW_ALL_USERS,
        max_message_length=MAX_MESSAGE_LENGTH,
        emoji="💬",
        pii_safe=True,  # Matrix MXIDs are pseudonymous handles, not phone numbers
        allow_update_command=True,
        platform_hint=(
            "You are chatting via Matrix in AgentTeams rooms. Markdown is "
            "rendered (formatted body) and images/files/voice messages are "
            "supported. In group rooms users address you by @-mentioning "
            "your Matrix user ID; unmentioned group messages are buffered "
            "as context and delivered together with the next mention. "
            "Direct messages and each room are separate conversations. "
            "Keep responses concise and conversational."
        ),
    )
