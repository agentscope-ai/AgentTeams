"""
MatrixChannel: CoPaw BaseChannel implementation for Matrix (via matrix-nio).

This file is installed into ~/.copaw/custom_channels/ at worker startup
so CoPaw's channel registry picks it up automatically.
"""
from __future__ import annotations

import asyncio
import html
import io
import logging
import mimetypes
import os
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any, AsyncIterator, Callable, Dict, List, Optional

from nio import (
    AsyncClient,
    AsyncClientConfig,
    LoginResponse,
    MatrixRoom,
    MegolmEvent,
    RoomEncryptedAudio,
    RoomEncryptedFile,
    RoomEncryptedImage,
    RoomEncryptedVideo,
    RoomMessageAudio,
    RoomMessageFile,
    RoomMessageImage,
    RoomMessageText,
    RoomMessageVideo,
    SyncResponse,
    UploadResponse,
)
from nio.responses import WhoamiResponse

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Lazy import of CoPaw base types so this file can be syntax-checked without
# copaw installed (it's only executed inside a copaw environment).
# ---------------------------------------------------------------------------
try:
    from copaw.app.channels.base import BaseChannel
    from copaw.app.channels.schema import ChannelType
    from agentscope_runtime.engine.schemas.agent_schemas import (
        AudioContent,
        ContentType,
        FileContent,
        ImageContent,
        TextContent,
        VideoContent,
    )
    _COPAW_AVAILABLE = True
except ImportError:  # pragma: no cover
    _COPAW_AVAILABLE = False
    BaseChannel = object  # type: ignore[assignment,misc]
    ChannelType = str  # type: ignore[assignment]


CHANNEL_KEY = "matrix"

# Known CoPaw slash commands — used to decide whether to strip @mention prefix
_SLASH_COMMANDS = frozenset({
    "message", "history", "compact_str", "compact", "new", "clear", "reset",
})

# Aliases: map alternative command names to their canonical form.
_SLASH_ALIASES: dict[str, str] = {
    "reset": "clear",
}


def _md_to_html(text: str) -> str:
    """Convert Markdown text to HTML for Matrix ``formatted_body``.

    Uses ``markdown-it-py`` (the Python port of markdown-it) with the same
    configuration as OpenClaw's Matrix extension so rendering is consistent
    across both runtimes:

    - html disabled (raw HTML is escaped)
    - linkify enabled (bare URLs become clickable links)
    - breaks enabled (single newlines become ``<br>``)
    - strikethrough enabled (``~~text~~``)

    Falls back to simple HTML-escape + ``<br>`` if the library is missing.
    """
    try:
        from markdown_it import MarkdownIt

        md = MarkdownIt(
            "commonmark",
            {"html": False, "linkify": True, "breaks": True, "typographer": False},
        )
        md.enable("strikethrough")
        md.enable("table")

        # linkify support requires linkify-it-py
        try:
            from linkify_it import LinkifyIt
            md.linkify = LinkifyIt()
        except ImportError:
            pass

        return md.render(text).rstrip("\n")
    except ImportError:
        logger.warning(
            "markdown-it-py not installed; formatted_body will be plain text"
        )
        return html.escape(text).replace("\n", "<br>\n")

# Markers that separate accumulated history from the triggering message,
# matching the convention used by OpenClaw so agents can parse uniformly.
HISTORY_CONTEXT_MARKER = "[Chat messages since your last reply - for context]"
CURRENT_MESSAGE_MARKER = "[Current message - respond to this]"
DEFAULT_HISTORY_LIMIT = 50


@dataclass
class HistoryEntry:
    """A buffered room message that didn't mention the bot."""

    sender: str
    body: str
    timestamp: Optional[int] = None
    message_id: Optional[str] = None
    # Optional structured media parts (e.g. downloaded images for vision models)
    # to be included alongside the text history when the mention arrives.
    media_parts: Optional[List[Dict[str, Any]]] = None


class MatrixChannelConfig:
    """Parsed config for MatrixChannel (read from config.json channels.matrix)."""

    def __init__(self, raw: dict[str, Any]) -> None:
        self.enabled: bool = raw.get("enabled", True)
        self.homeserver: str = raw.get("homeserver", "")
        self.access_token: str = raw.get("access_token", "")
        # username/password fallback (rarely used in hiclaw)
        self.username: str = raw.get("username", "")
        self.password: str = raw.get("password", "")
        self.device_name: str = raw.get("device_name", "copaw-worker")
        # E2EE: when True, enable end-to-end encryption via matrix-nio + libolm
        self.encryption: bool = raw.get("encryption", False)

        # Allowlist / policy
        self.dm_policy: str = raw.get("dm_policy", "allowlist")
        self.allow_from: list[str] = [
            _normalize_user_id(u) for u in raw.get("allow_from", [])
        ]
        self.group_policy: str = raw.get("group_policy", "allowlist")
        self.group_allow_from: list[str] = [
            _normalize_user_id(u) for u in raw.get("group_allow_from", [])
        ]
        # Per-room overrides: {"*": {"requireMention": true}, ...}
        self.groups: dict[str, Any] = raw.get("groups", {})

        self.bot_prefix: str = raw.get("bot_prefix", "")
        self.filter_tool_messages: bool = raw.get("filter_tool_messages", False)
        self.filter_thinking: bool = raw.get("filter_thinking", False)
        # Whether the active model supports image inputs. Set by bridge.py.
        # Defaults to False so images are never sent to a non-vision model.
        self.vision_enabled: bool = raw.get("vision_enabled", False)
        # Max non-mentioned messages to buffer per room (0 = disabled).
        self.history_limit: int = max(0, raw.get("history_limit", DEFAULT_HISTORY_LIMIT))


def _normalize_user_id(uid: str) -> str:
    uid = uid.strip().lower()
    if not uid.startswith("@"):
        uid = "@" + uid
    return uid


class MatrixChannel(BaseChannel):
    """CoPaw channel that connects to a Matrix homeserver via matrix-nio."""

    channel = CHANNEL_KEY  # type: ignore[assignment]
    uses_manager_queue: bool = True

    def __init__(
        self,
        process: Callable,
        config: MatrixChannelConfig,
        on_reply_sent: Optional[Callable] = None,
        show_tool_details: bool = True,
        filter_tool_messages: bool = False,
        filter_thinking: bool = False,
    ) -> None:
        super().__init__(
            process=process,
            on_reply_sent=on_reply_sent,
            show_tool_details=show_tool_details,
            filter_tool_messages=filter_tool_messages,
            filter_thinking=filter_thinking,
        )
        self._cfg = config
        self._client: Optional[AsyncClient] = None
        self._user_id: Optional[str] = None
        self._sync_task: Optional[asyncio.Task] = None
        self._typing_tasks: Dict[str, asyncio.Task] = {}  # room_id -> renewal task
        self._room_histories: Dict[str, List[HistoryEntry]] = {}  # per-room history buffer

    # ------------------------------------------------------------------
    # Debounce key — use room_id so same-room messages from different
    # senders are serialised (not processed concurrently), preventing
    # race conditions on the shared session state.
    # ------------------------------------------------------------------

    def get_debounce_key(self, payload: Any) -> str:
        if isinstance(payload, dict):
            meta = payload.get("meta") or {}
            room_id = meta.get("room_id")
            if room_id:
                return f"matrix:{room_id}"
            return payload.get("sender_id") or ""
        return getattr(payload, "session_id", "") or ""

    # ------------------------------------------------------------------
    # Factory
    # ------------------------------------------------------------------

    @classmethod
    def from_config(
        cls,
        process: Callable,
        config: Any,
        on_reply_sent: Optional[Callable] = None,
        show_tool_details: bool = True,
        filter_tool_messages: bool = False,
        filter_thinking: bool = False,
    ) -> "MatrixChannel":
        if isinstance(config, dict):
            cfg = MatrixChannelConfig(config)
        elif isinstance(config, MatrixChannelConfig):
            cfg = config
        else:
            # SimpleNamespace or other object — convert to dict via __dict__
            cfg = MatrixChannelConfig(vars(config))
        return cls(
            process=process,
            config=cfg,
            on_reply_sent=on_reply_sent,
            show_tool_details=show_tool_details,
            filter_tool_messages=filter_tool_messages or cfg.filter_tool_messages,
            filter_thinking=filter_thinking or cfg.filter_thinking,
        )

    @classmethod
    def from_env(cls, process: Callable, on_reply_sent=None) -> "MatrixChannel":
        import os
        cfg = MatrixChannelConfig({
            "homeserver": os.environ.get("HICLAW_MATRIX_SERVER", ""),
            "access_token": os.environ.get("HICLAW_MATRIX_TOKEN", ""),
        })
        return cls(process=process, config=cfg, on_reply_sent=on_reply_sent)

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def start(self) -> None:
        if not self._cfg.homeserver:
            logger.warning("MatrixChannel: homeserver not configured, skipping")
            return

        # E2EE: when encryption is enabled, provide store_path so matrix-nio
        # persists Olm/Megolm keys, and set config to auto-trust all devices
        # (appropriate for bot use cases where interactive verification is
        # impractical).
        if self._cfg.encryption:
            store_path = self._e2ee_store_path()
            store_path.mkdir(parents=True, exist_ok=True)
            client_config = AsyncClientConfig(
                store_sync_tokens=False,
                encryption_enabled=True,
            )
            self._client = AsyncClient(
                self._cfg.homeserver,
                user="",
                store_path=str(store_path),
                config=client_config,
            )
        else:
            self._client = AsyncClient(self._cfg.homeserver, user="")

        # Login
        if self._cfg.access_token:
            self._client.access_token = self._cfg.access_token
            whoami = await self._client.whoami()
            if isinstance(whoami, WhoamiResponse):
                self._user_id = whoami.user_id
                self._client.user_id = whoami.user_id
                self._client.user = whoami.user_id
                # E2EE requires device_id to associate Olm keys with this device
                if whoami.device_id:
                    self._client.device_id = whoami.device_id
                logger.info("MatrixChannel: logged in as %s (token, device=%s)", self._user_id, whoami.device_id)
                # Load crypto store after user_id and device_id are set
                if self._cfg.encryption and self._client.store_path:
                    if self._client.device_id:
                        self._client.load_store()
                        logger.info("MatrixChannel: crypto store loaded from %s", self._client.store_path)
                    else:
                        logger.error("MatrixChannel: E2EE enabled but whoami returned no device_id — encryption disabled"
                                     " (token may lack device scope)")
                        self._cfg.encryption = False
            else:
                logger.error("MatrixChannel: token login failed: %s", whoami)
                return
        elif self._cfg.username and self._cfg.password:
            resp = await self._client.login(
                self._cfg.username,
                self._cfg.password,
                device_name=self._cfg.device_name,
            )
            if isinstance(resp, LoginResponse):
                self._user_id = resp.user_id
                logger.info("MatrixChannel: logged in as %s (password)", self._user_id)
            else:
                logger.error("MatrixChannel: password login failed: %s", resp)
                return
        else:
            logger.error("MatrixChannel: no credentials configured")
            return

        # Register event callbacks and start sync loop
        self._client.add_event_callback(self._on_room_event, (RoomMessageText,))
        self._client.add_event_callback(
            self._on_room_media_event,
            (RoomMessageImage, RoomMessageFile, RoomMessageAudio, RoomMessageVideo),
        )

        # E2EE: upload device keys and register encrypted event callbacks
        if self._cfg.encryption:
            if self._client.should_upload_keys:
                await self._client.keys_upload()
                logger.info("MatrixChannel: E2E keys uploaded")
            # Encrypted media events (decrypted by nio, delivered as RoomEncrypted* types)
            self._client.add_event_callback(
                self._on_room_encrypted_media_event,
                (RoomEncryptedImage, RoomEncryptedAudio, RoomEncryptedVideo, RoomEncryptedFile),
            )
            # Undecryptable events (missing session key)
            self._client.add_event_callback(self._on_megolm_event, (MegolmEvent,))
            logger.info("MatrixChannel: E2EE enabled, encrypted event handlers registered")

        self._sync_task = asyncio.create_task(self._sync_loop())
        logger.info("MatrixChannel: sync loop started")

    async def stop(self) -> None:
        if self._sync_task:
            self._sync_task.cancel()
            try:
                await self._sync_task
            except asyncio.CancelledError:
                pass
        if self._client:
            await self._client.close()
        logger.info("MatrixChannel: stopped")

    # ------------------------------------------------------------------
    # Sync loop
    # ------------------------------------------------------------------

    # ------------------------------------------------------------------
    # Sync token persistence
    # ------------------------------------------------------------------

    @staticmethod
    def _sync_token_path() -> Optional[Path]:
        """Return the file path for persisting the Matrix sync token."""
        wd = os.environ.get("COPAW_WORKING_DIR")
        if wd:
            return Path(wd) / "matrix_sync_token"
        return None

    def _load_sync_token(self) -> Optional[str]:
        """Load persisted next_batch token from disk, or None.

        The token file is pulled from MinIO by FileSync.pull_all() during
        startup, so it's already on disk when this runs — even on a fresh
        container after destroy/recreate.
        """
        path = self._sync_token_path()
        if path and path.exists():
            try:
                token = path.read_text().strip()
                if token:
                    logger.info("MatrixChannel: restored sync token from %s", path)
                    return token
            except Exception as exc:
                logger.warning("MatrixChannel: failed to read sync token: %s", exc)
        return None

    def _save_sync_token(self, token: str) -> None:
        """Persist next_batch token to disk (push_loop uploads it to MinIO)."""
        path = self._sync_token_path()
        if path:
            try:
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(token)
            except Exception as exc:
                logger.warning("MatrixChannel: failed to save sync token: %s", exc)

    async def _e2ee_maintenance(self) -> None:
        """Perform E2EE key maintenance tasks after each sync.

        Mirrors what nio's sync_forever() does between syncs:
        - Upload device keys when needed
        - Query device keys for new/changed users
        - Claim one-time keys to establish Olm sessions
        - Send outgoing to-device messages (key shares, key requests)
        """
        if not self._cfg.encryption or not self._client or not self._client.olm:
            return
        try:
            if self._client.should_upload_keys:
                await self._client.keys_upload()
            if self._client.should_query_keys:
                await self._client.keys_query()
            if self._client.should_claim_keys:
                await self._client.keys_claim(
                    self._client.get_users_for_key_claiming()
                )
            await self._client.send_to_device_messages()
        except Exception as exc:
            logger.warning("MatrixChannel: E2EE maintenance error: %s", exc)

    async def _sync_loop(self) -> None:
        next_batch: Optional[str] = self._load_sync_token()

        # When no persisted token exists (old version upgrade or first deploy),
        # do an initial sync with callbacks suppressed — only capture next_batch
        # so subsequent syncs are incremental.  This prevents replaying old
        # messages when the token file doesn't exist yet.
        #
        # To truly suppress callbacks, temporarily remove event callbacks
        # before the sync and restore them after, because nio's sync()
        # internally calls receive_response() which fires callbacks.
        if next_batch is None:
            logger.info("MatrixChannel: no sync token found, performing catch-up sync (messages suppressed)")
            try:
                saved_cbs = self._client.event_callbacks[:]
                self._client.event_callbacks.clear()
                try:
                    resp = await self._client.sync(timeout=30000, full_state=True)
                finally:
                    self._client.event_callbacks.extend(saved_cbs)
                if isinstance(resp, SyncResponse):
                    next_batch = resp.next_batch
                    self._save_sync_token(next_batch)
                    # Still auto-join invited rooms during catch-up
                    for room_id in resp.rooms.invite:
                        logger.info("MatrixChannel: auto-joining %s", room_id)
                        await self._client.join(room_id)
                    await self._e2ee_maintenance()
                    logger.info("MatrixChannel: catch-up sync done, will process messages from next sync")
                else:
                    logger.warning("MatrixChannel: catch-up sync error: %s", resp)
            except Exception as exc:
                logger.exception("MatrixChannel: catch-up sync exception: %s", exc)
        else:
            # Restored from token — do a full_state sync to populate room
            # member display names (nio needs full state for user_name()).
            # Event callbacks are already registered so any messages received
            # during the offline window will be processed normally.
            logger.info("MatrixChannel: restored token, performing full-state sync to load room state")
            try:
                resp = await self._client.sync(
                    timeout=30000, since=next_batch, full_state=True,
                )
                if isinstance(resp, SyncResponse):
                    next_batch = resp.next_batch
                    self._save_sync_token(next_batch)
                    for room_id in resp.rooms.invite:
                        logger.info("MatrixChannel: auto-joining %s", room_id)
                        await self._client.join(room_id)
                    await self._e2ee_maintenance()
                else:
                    logger.warning("MatrixChannel: full-state sync error: %s", resp)
            except Exception as exc:
                logger.exception("MatrixChannel: full-state sync exception: %s", exc)

        while True:
            try:
                resp = await self._client.sync(
                    timeout=30000,
                    since=next_batch,
                    full_state=False,
                )
                if isinstance(resp, SyncResponse):
                    next_batch = resp.next_batch
                    self._save_sync_token(next_batch)
                    # Auto-join invited rooms
                    for room_id in resp.rooms.invite:
                        logger.info("MatrixChannel: auto-joining %s", room_id)
                        await self._client.join(room_id)
                    # E2EE: full key maintenance (upload, query, claim, to-device)
                    await self._e2ee_maintenance()
                else:
                    logger.warning("MatrixChannel: sync error: %s", resp)
                    await asyncio.sleep(5)
            except asyncio.CancelledError:
                break
            except Exception as exc:
                logger.exception("MatrixChannel: sync exception: %s", exc)
                await asyncio.sleep(5)

    # ------------------------------------------------------------------
    # Allowlist helpers
    # ------------------------------------------------------------------

    def _check_allowed(self, sender_id: str, room_id: str, is_dm: bool) -> bool:
        """Return True if the sender is allowed to interact in this context."""
        normalized = _normalize_user_id(sender_id)
        if is_dm:
            if self._cfg.dm_policy == "disabled":
                return False
            if self._cfg.dm_policy == "allowlist":
                if normalized not in self._cfg.allow_from:
                    logger.debug("MatrixChannel: DM blocked from %s", sender_id)
                    return False
        else:
            if self._cfg.group_policy == "disabled":
                return False
            if self._cfg.group_policy == "allowlist":
                if normalized not in self._cfg.group_allow_from:
                    logger.debug("MatrixChannel: group msg blocked from %s", sender_id)
                    return False
        return True

    def _require_mention(self, room_id: str) -> bool:
        """Check per-room config, fall back to global default (require mention)."""
        room_cfg = self._cfg.groups.get(room_id) or self._cfg.groups.get("*")
        if room_cfg:
            if room_cfg.get("autoReply") is True:
                return False
            if "requireMention" in room_cfg:
                return bool(room_cfg["requireMention"])
        return True  # default: require mention in group rooms

    def _was_mentioned(self, event: Any, text: str) -> bool:
        if not self._user_id:
            return False
        # 1. Check m.mentions (structured mention from Matrix spec)
        content = event.source.get("content", {})
        mentions = content.get("m.mentions", {})
        if self._user_id in mentions.get("user_ids", []):
            return True
        if mentions.get("room"):
            return True
        # 2. Check formatted_body for matrix.to mention links (Element HTML format)
        formatted_body = content.get("formatted_body", "")
        if formatted_body and self._user_id:
            import urllib.parse
            escaped_uid = re.escape(self._user_id)
            if re.search(rf'href=["\']https://matrix\.to/#/{escaped_uid}["\']', formatted_body, re.IGNORECASE):
                return True
            encoded_uid = re.escape(urllib.parse.quote(self._user_id))
            if re.search(rf'href=["\']https://matrix\.to/#/{encoded_uid}["\']', formatted_body, re.IGNORECASE):
                return True
        # 3. Fallback: match full MXID in plain text
        if self._user_id and re.search(re.escape(self._user_id), text, re.IGNORECASE):
            return True
        return False

    def _strip_mention_prefix(self, text: str, room: Any = None) -> str:
        """Strip leading @mention prefix so slash commands can be detected.

        Handles MXID format (@user:server), room display name, and localpart.
        E.g. ``"@worker:hs.example /new"`` → ``"/new"``
             ``"math 💕: /clear"`` → ``"/clear"``.
        """
        if not self._user_id:
            return text
        # 1. Strip MXID (@user:server) at start
        escaped = re.escape(self._user_id)
        result = re.sub(rf"^{escaped}\s*:?\s*", "", text, flags=re.IGNORECASE)
        if result != text:
            return result.strip()
        # 2. Strip room display name (e.g. "math 💕") at start — try before
        #    localpart so that "math 💕: /clear" is not partially matched by
        #    the shorter localpart "math".
        if room and self._user_id:
            display_name = self._get_display_name(room, self._user_id)
            logger.debug(
                "strip_mention_prefix: user_id=%s display_name=%r room_users=%d",
                self._user_id, display_name, len(getattr(room, 'users', {})),
            )
            if display_name and display_name != self._user_id:
                result = re.sub(
                    rf"^{re.escape(display_name)}\s*:?\s*", "", text, flags=re.IGNORECASE,
                )
                if result != text:
                    # Clean leftover decoration (e.g. emoji suffix) between
                    # the display name and the actual message content.
                    result = re.sub(r"^[^\w/]+", "", result)
                    return result.strip()
        # 3. Strip localpart (e.g. "math") at start — only if display name
        #    didn't match.
        localpart = self._user_id.split(":")[0].lstrip("@")
        if localpart:
            result = re.sub(
                rf"^{re.escape(localpart)}\s*:?\s*", "", text, flags=re.IGNORECASE,
            )
            if result != text:
                # After stripping localpart, there may be leftover decoration
                # from the display name (e.g. emoji suffix "💕: " from
                # "math 💕: /clear").  Strip non-alphanumeric prefix so the
                # slash command is exposed.
                result = re.sub(r"^[^\w/]+", "", result)
                return result.strip()
        return text

    # ------------------------------------------------------------------
    # History accumulation (requireMention + context buffering)
    # ------------------------------------------------------------------

    def _get_display_name(self, room: Any, user_id: str) -> str:
        """Best-effort human-readable name for a Matrix user in *room*.

        Tries the room object passed by nio first, then falls back to
        looking up the room in the nio client's rooms dict (which is
        populated by full_state sync at startup).
        """
        # 1. Try the room object directly (passed by nio callback)
        try:
            name = room.user_name(user_id)
            if name:
                return name
        except Exception:
            pass
        # 2. Fallback: look up from nio client's rooms dict
        if self._client:
            room_id = getattr(room, "room_id", None)
            if room_id:
                client_room = self._client.rooms.get(room_id)
                if client_room and client_room is not room:
                    try:
                        name = client_room.user_name(user_id)
                        if name:
                            logger.debug(
                                "display_name resolved via client.rooms fallback: %s -> %r",
                                user_id, name,
                            )
                            return name
                    except Exception:
                        pass
        # 3. Fallback: localpart of MXID (e.g. "@alice:hs" → "alice")
        logger.debug(
            "display_name fallback to localpart for %s (room.users=%d, client_rooms=%d)",
            user_id,
            len(getattr(room, "users", {})),
            len(self._client.rooms) if self._client else 0,
        )
        return user_id.split(":")[0].lstrip("@") or user_id

    def _record_history(self, room_id: str, entry: HistoryEntry) -> None:
        """Append *entry* to the per-room history buffer, respecting the limit."""
        limit = self._cfg.history_limit
        if limit <= 0:
            return
        history = self._room_histories.setdefault(room_id, [])
        history.append(entry)
        while len(history) > limit:
            history.pop(0)

    def _build_history_prefix(self, room_id: str) -> str:
        """Format buffered history entries as a multi-line text block."""
        entries = self._room_histories.get(room_id, [])
        if not entries:
            return ""
        lines: list[str] = []
        for e in entries:
            line = f"{e.sender}: {e.body}"
            if e.message_id:
                line += f" [id:{e.message_id}]"
            lines.append(line)
        return "\n".join(lines)

    def _apply_history_to_parts(
        self, room_id: str, content_parts: list[dict[str, Any]]
    ) -> list[dict[str, Any]]:
        """Prepend accumulated history context to *content_parts*.

        If the first part is text, the history block is merged into it;
        otherwise a new text part is prepended.  Any media parts stored
        in history entries (e.g. downloaded images) are inserted between
        the history text block and the current message parts so that
        vision models can see them.

        Returns a (possibly new) list — the original is not mutated.
        """
        if self._cfg.history_limit <= 0:
            return content_parts
        history_text = self._build_history_prefix(room_id)
        if not history_text:
            return content_parts

        # Collect media content parts carried by history entries
        history_media: list[dict[str, Any]] = []
        for entry in self._room_histories.get(room_id, []):
            if entry.media_parts:
                history_media.extend(entry.media_parts)

        # Merge into the leading text part when possible
        if content_parts and content_parts[0].get("type") == "text":
            current_text = content_parts[0]["text"]
            combined = (
                f"{HISTORY_CONTEXT_MARKER}\n{history_text}\n\n"
                f"{CURRENT_MESSAGE_MARKER}\n{current_text}"
            )
            return [{"type": "text", "text": combined}] + history_media + content_parts[1:]
        # No leading text part (e.g. pure media) — prepend a dedicated block
        prefix_part = {
            "type": "text",
            "text": (
                f"{HISTORY_CONTEXT_MARKER}\n{history_text}\n\n"
                f"{CURRENT_MESSAGE_MARKER}"
            ),
        }
        return [prefix_part] + history_media + content_parts

    def _clear_history(self, room_id: str) -> None:
        """Drop the buffered history for *room_id*."""
        self._room_histories.pop(room_id, None)

    async def _record_media_history(
        self, room: Any, event: Any, sender_id: str, room_id: str
    ) -> None:
        """Record a non-mentioned media message as a history entry.

        Produces a typed text description (e.g. ``[sent an image: photo.jpg]``)
        and, for images when vision is enabled, downloads the actual file so it
        can be included as an image content part later.
        """
        body = event.body or ""
        media_parts: list[dict[str, Any]] = []

        if isinstance(event, RoomMessageImage):
            body_desc = f"[sent an image: {body}]" if body else "[sent an image]"
            if self._cfg.vision_enabled:
                mxc_url: str = getattr(event, "url", "") or ""
                if mxc_url:
                    eid = event.event_id[:8].lstrip("$")
                    filename = body or f"matrix_media_{eid}"
                    filename = f"{eid}_{filename}"
                    local_path = await self._download_mxc(mxc_url, filename)
                    if local_path:
                        media_parts.append({
                            "type": "image",
                            "image_url": Path(local_path).as_uri(),
                        })
        elif isinstance(event, RoomMessageFile):
            body_desc = f"[sent a file: {body}]" if body else "[sent a file]"
        elif isinstance(event, RoomMessageAudio):
            body_desc = f"[sent audio: {body}]" if body else "[sent audio]"
        elif isinstance(event, RoomMessageVideo):
            body_desc = f"[sent a video: {body}]" if body else "[sent a video]"
        else:
            body_desc = body or "[media]"

        self._record_history(room_id, HistoryEntry(
            sender=self._get_display_name(room, sender_id),
            body=body_desc,
            timestamp=getattr(event, "server_timestamp", None),
            message_id=event.event_id,
            media_parts=media_parts or None,
        ))

    # ------------------------------------------------------------------
    # Media directory
    # ------------------------------------------------------------------

    def _media_dir(self) -> Path:
        """Return (and create) the local media storage directory."""
        try:
            from copaw.constant import WORKING_DIR
            d = WORKING_DIR / "media"
        except Exception:
            d = Path.home() / ".copaw" / "media"
        d.mkdir(parents=True, exist_ok=True)
        return d

    # ------------------------------------------------------------------
    # Media download (mxc:// → local file)
    # ------------------------------------------------------------------

    async def _download_mxc(self, mxc_url: str, filename: str) -> Optional[str]:
        """Download an mxc:// URI to a local file. Returns local path or None."""
        if not mxc_url.startswith("mxc://"):
            return None
        try:
            rest = mxc_url[6:]  # strip "mxc://"
            server, media_id = rest.split("/", 1)
            url = (
                f"{self._cfg.homeserver}/_matrix/media/v3/download"
                f"/{server}/{media_id}"
            )
            headers = {"Authorization": f"Bearer {self._cfg.access_token}"}
            import httpx
            async with httpx.AsyncClient(follow_redirects=True, timeout=60) as http:
                resp = await http.get(url, headers=headers)
                resp.raise_for_status()
            dest = self._media_dir() / filename
            dest.write_bytes(resp.content)
            logger.debug("MatrixChannel: downloaded %s → %s", mxc_url, dest)
            return str(dest)
        except Exception as exc:
            logger.warning(
                "MatrixChannel: failed to download %s: %s", mxc_url, exc
            )
            return None

    # ------------------------------------------------------------------
    # E2EE helpers
    # ------------------------------------------------------------------

    def _e2ee_store_path(self) -> Path:
        """Return the directory for persisting Olm/Megolm crypto state."""
        wd = os.environ.get("COPAW_WORKING_DIR")
        if wd:
            return Path(wd) / "matrix_crypto_store"
        return Path.home() / ".copaw" / "matrix_crypto_store"

    async def _download_encrypted_mxc(
        self, mxc_url: str, filename: str, key: dict, hashes: dict, iv: str
    ) -> Optional[str]:
        """Download an encrypted mxc:// URI, decrypt it, and save locally."""
        if not mxc_url.startswith("mxc://") or not self._client:
            return None
        try:
            rest = mxc_url[6:]
            server, media_id = rest.split("/", 1)
            url = (
                f"{self._cfg.homeserver}/_matrix/media/v3/download"
                f"/{server}/{media_id}"
            )
            headers = {"Authorization": f"Bearer {self._cfg.access_token}"}
            import httpx
            async with httpx.AsyncClient(follow_redirects=True, timeout=60) as http:
                resp = await http.get(url, headers=headers)
                resp.raise_for_status()

            from nio.crypto.attachments import decrypt_attachment
            jwk_key = key.get("k", "")
            sha256_hash = hashes.get("sha256", "")
            plaintext = decrypt_attachment(resp.content, jwk_key, sha256_hash, iv)

            dest = self._media_dir() / filename
            dest.write_bytes(plaintext)
            logger.debug("MatrixChannel: downloaded+decrypted %s → %s", mxc_url, dest)
            return str(dest)
        except Exception as exc:
            logger.warning(
                "MatrixChannel: failed to download encrypted %s: %s", mxc_url, exc
            )
            return None

    async def _on_megolm_event(self, room: MatrixRoom, event: MegolmEvent) -> None:
        """Handle undecryptable encrypted events (missing session key)."""
        logger.warning(
            "MatrixChannel: could not decrypt event %s in %s (session_id=%s)",
            event.event_id, room.room_id, getattr(event, "session_id", "?"),
        )

    async def _on_room_encrypted_media_event(self, room: MatrixRoom, event: Any) -> None:
        """Handle decrypted encrypted media events (RoomEncryptedImage, etc.).

        These events are delivered by matrix-nio after successful decryption of
        the Megolm payload. The file content itself is still encrypted with AES
        and must be downloaded + decrypted using the key/iv/hashes from the event.
        """
        if event.sender == self._user_id:
            return

        sender_id = event.sender
        room_id = room.room_id
        is_dm = len(room.users) == 2

        if not self._check_allowed(sender_id, room_id, is_dm):
            return

        if not is_dm:
            if self._require_mention(room_id) and not self._was_mentioned(event, ""):
                # Record as history (text description only)
                body = event.body or ""
                if isinstance(event, RoomEncryptedImage):
                    desc = f"[sent an encrypted image: {body}]" if body else "[sent an encrypted image]"
                elif isinstance(event, RoomEncryptedAudio):
                    desc = f"[sent encrypted audio: {body}]" if body else "[sent encrypted audio]"
                elif isinstance(event, RoomEncryptedVideo):
                    desc = f"[sent an encrypted video: {body}]" if body else "[sent an encrypted video]"
                else:
                    desc = f"[sent an encrypted file: {body}]" if body else "[sent an encrypted file]"
                self._record_history(room_id, HistoryEntry(
                    sender=self._get_display_name(room, sender_id),
                    body=desc,
                    timestamp=getattr(event, "server_timestamp", None),
                    message_id=event.event_id,
                ))
                return

        await self._send_read_receipt(room_id, event.event_id)
        await self._send_typing(room_id, True)

        body = event.body or ""
        mxc_url = getattr(event, "url", "") or ""
        key = getattr(event, "key", {}) or {}
        hashes = getattr(event, "hashes", {}) or {}
        iv = getattr(event, "iv", "") or ""

        content_parts: list[dict[str, Any]] = []
        if body:
            content_parts.append({"type": "text", "text": body})

        if mxc_url and key and iv:
            eid = event.event_id[:8].lstrip("$")
            filename = body or f"matrix_media_{eid}"
            filename = f"{eid}_{filename}"
            local_path = await self._download_encrypted_mxc(mxc_url, filename, key, hashes, iv)
            if local_path:
                file_uri = Path(local_path).as_uri()
                if isinstance(event, RoomEncryptedImage):
                    if self._cfg.vision_enabled:
                        content_parts.append({"type": "image", "image_url": file_uri})
                    else:
                        content_parts.append({
                            "type": "text",
                            "text": f"[User sent an image (current model does not support image input): {body or filename}]",
                        })
                elif isinstance(event, RoomEncryptedAudio):
                    content_parts.append({"type": "audio", "data": file_uri})
                elif isinstance(event, RoomEncryptedVideo):
                    content_parts.append({"type": "video", "video_url": file_uri})
                else:
                    content_parts.append({
                        "type": "file",
                        "file_url": file_uri,
                        "filename": body or filename,
                    })
            else:
                content_parts.append({"type": "text", "text": f"[Encrypted media unavailable: {body}]"})

        if not content_parts:
            return

        if not is_dm:
            content_parts = self._apply_history_to_parts(room_id, content_parts)

        worker_name = (self._user_id or "").split(":")[0].lstrip("@")
        payload = {
            "channel_id": CHANNEL_KEY,
            "sender_id": sender_id,
            "content_parts": content_parts,
            "meta": {
                "room_id": room_id,
                "is_dm": is_dm,
                "worker_name": worker_name,
                "event_id": event.event_id,
                "sender_id": sender_id,
            },
        }

        if self._enqueue:
            self._enqueue(payload)
            if not is_dm:
                self._clear_history(room_id)

    # ------------------------------------------------------------------
    # Media upload (local file → mxc://)
    # ------------------------------------------------------------------

    async def _upload_file(self, file_ref: str) -> Optional[str]:
        """Upload a local file to Matrix media repo. Returns mxc:// URI or None."""
        if not self._client:
            return None
        try:
            # file_ref may be a file:// URI or a plain path
            path = Path(file_ref.removeprefix("file://"))
            if not path.exists():
                logger.warning(
                    "MatrixChannel: upload source not found: %s", file_ref
                )
                return None
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
                    "MatrixChannel: uploaded %s → %s", path.name, resp.content_uri
                )
                return resp.content_uri
            logger.warning("MatrixChannel: upload failed: %s", resp)
            return None
        except Exception as exc:
            logger.warning(
                "MatrixChannel: upload error for %s: %s", file_ref, exc
            )
            return None

    # ------------------------------------------------------------------
    # Incoming message handling — text
    # ------------------------------------------------------------------

    async def _on_room_event(self, room: MatrixRoom, event: RoomMessageText) -> None:
        logger.debug(
            "_on_room_event: sender=%s body=%r room_users=%d",
            event.sender, (event.body or "")[:80], len(getattr(room, 'users', {})),
        )
        # Skip own messages
        if event.sender == self._user_id:
            return

        sender_id = event.sender
        room_id = room.room_id
        text = event.body or ""
        is_dm = len(room.users) == 2

        if not self._check_allowed(sender_id, room_id, is_dm):
            return

        # Mention check for group rooms
        if not is_dm:
            if self._require_mention(room_id) and not self._was_mentioned(event, text):
                self._record_history(room_id, HistoryEntry(
                    sender=self._get_display_name(room, sender_id),
                    body=text,
                    timestamp=getattr(event, "server_timestamp", None),
                    message_id=event.event_id,
                ))
                return

        # Mark as read + start typing immediately so the sender sees feedback
        await self._send_read_receipt(room_id, event.event_id)
        await self._send_typing(room_id, True)

        # Strip leading @mention so slash commands are detected regardless of
        # room type (group or DM).  Only strip for known CoPaw commands to
        # avoid altering normal messages.
        command_text = text
        stripped = self._strip_mention_prefix(text, room)
        cmd = stripped.lstrip("/").split()[0] if stripped.startswith("/") else ""
        if cmd in _SLASH_COMMANDS:
            command_text = stripped
            # Apply alias (e.g. /reset -> /clear)
            if cmd in _SLASH_ALIASES:
                canonical = _SLASH_ALIASES[cmd]
                command_text = command_text.replace(f"/{cmd}", f"/{canonical}", 1)
            if stripped != text:
                logger.info("Stripped mention prefix for slash command: %r -> %r", text, command_text)

        # Build content parts, prepending accumulated history for group rooms.
        # Skip history prepend for slash commands — CoPaw's command parser
        # requires the message to start with "/" to recognise it.
        content_parts: list[dict[str, Any]] = [{"type": "text", "text": command_text}]
        is_slash_cmd = command_text.startswith("/")
        if not is_dm and not is_slash_cmd:
            content_parts = self._apply_history_to_parts(room_id, content_parts)

        worker_name = (self._user_id or "").split(":")[0].lstrip("@")
        payload = {
            "channel_id": CHANNEL_KEY,
            "sender_id": sender_id,
            "content_parts": content_parts,
            "meta": {
                "room_id": room_id,
                "is_dm": is_dm,
                "worker_name": worker_name,
                "event_id": event.event_id,
                "sender_id": sender_id,
            },
        }

        if self._enqueue:
            self._enqueue(payload)
            if not is_dm:
                self._clear_history(room_id)

    # ------------------------------------------------------------------
    # Incoming message handling — media (image / file / audio / video)
    # ------------------------------------------------------------------

    async def _on_room_media_event(self, room: MatrixRoom, event: Any) -> None:
        """Handle incoming media messages (image, file, audio, video)."""
        if event.sender == self._user_id:
            return

        sender_id = event.sender
        room_id = room.room_id
        is_dm = len(room.users) == 2

        if not self._check_allowed(sender_id, room_id, is_dm):
            return

        # For group rooms, apply the same mention policy as text messages.
        # Media body (filename) rarely contains a mention, but respect
        # m.mentions if the client sends it.
        if not is_dm:
            if self._require_mention(room_id) and not self._was_mentioned(event, ""):
                await self._record_media_history(room, event, sender_id, room_id)
                return

        await self._send_read_receipt(room_id, event.event_id)
        await self._send_typing(room_id, True)

        mxc_url: str = getattr(event, "url", "") or ""
        body: str = event.body or ""  # filename or caption

        content_parts: list[dict[str, Any]] = []

        # Include the filename/caption as a text hint so the LLM understands context
        if body:
            content_parts.append({"type": "text", "text": body})

        if mxc_url:
            # Use the body as filename, fall back to a safe default.
            # Strip leading '$' from Matrix event IDs to avoid URI encoding
            # issues ($→%24 breaks agentscope's image extension check).
            eid = event.event_id[:8].lstrip("$")
            filename = body or f"matrix_media_{eid}"
            filename = f"{eid}_{filename}"
            local_path = await self._download_mxc(mxc_url, filename)
            if local_path:
                file_uri = Path(local_path).as_uri()
                if isinstance(event, RoomMessageImage):
                    if self._cfg.vision_enabled:
                        content_parts.append({
                            "type": "image",
                            "image_url": file_uri,
                        })
                    else:
                        # Model does not support image input — downgrade to text
                        content_parts.append({
                            "type": "text",
                            "text": f"[User sent an image (current model does not support image input): {body or filename}]",
                        })
                elif isinstance(event, RoomMessageAudio):
                    content_parts.append({
                        "type": "audio",
                        "data": file_uri,
                    })
                elif isinstance(event, RoomMessageVideo):
                    content_parts.append({
                        "type": "video",
                        "video_url": file_uri,
                    })
                else:  # RoomMessageFile
                    content_parts.append({
                        "type": "file",
                        "file_url": file_uri,
                        "filename": body or filename,
                    })
            else:
                content_parts.append({
                    "type": "text",
                    "text": f"[Media unavailable: {body}]",
                })

        if not content_parts:
            return

        # Prepend accumulated history for group rooms
        if not is_dm:
            content_parts = self._apply_history_to_parts(room_id, content_parts)

        worker_name = (self._user_id or "").split(":")[0].lstrip("@")
        payload = {
            "channel_id": CHANNEL_KEY,
            "sender_id": sender_id,
            "content_parts": content_parts,
            "meta": {
                "room_id": room_id,
                "is_dm": is_dm,
                "worker_name": worker_name,
                "event_id": event.event_id,
                "sender_id": sender_id,
            },
        }

        if self._enqueue:
            self._enqueue(payload)
            if not is_dm:
                self._clear_history(room_id)

    # ------------------------------------------------------------------
    # Read receipt & typing indicator
    # ------------------------------------------------------------------

    async def _send_read_receipt(self, room_id: str, event_id: str) -> None:
        """Mark a message as read (sends both read receipt and read marker)."""
        if not self._client or not event_id:
            return
        try:
            await self._client.room_read_markers(
                room_id, fully_read_event=event_id, read_event=event_id
            )
        except Exception as exc:
            logger.debug(
                "MatrixChannel: read receipt failed for %s: %s", event_id, exc
            )

    async def _send_typing(
        self, room_id: str, typing: bool, timeout: int = 30000
    ) -> None:
        """Set typing indicator on/off for a room.

        When turning on, starts a background renewal task that re-sends the
        typing indicator every 25s (before the 30s server timeout expires),
        up to a 2-minute hard cap. When turning off, cancels the renewal task.
        """
        if not self._client:
            return
        # Cancel any existing renewal task for this room
        existing = self._typing_tasks.pop(room_id, None)
        if existing and not existing.done():
            existing.cancel()
        try:
            await self._client.room_typing(
                room_id, typing_state=typing, timeout=timeout
            )
        except Exception as exc:
            logger.debug(
                "MatrixChannel: typing indicator failed for %s: %s", room_id, exc
            )
        # Start renewal loop if turning on
        if typing:
            self._typing_tasks[room_id] = asyncio.create_task(
                self._typing_renewal_loop(room_id, timeout)
            )

    async def _typing_renewal_loop(
        self, room_id: str, timeout: int = 30000
    ) -> None:
        """Re-send typing=true every 25s, hard-capped at 2 minutes."""
        max_duration = 120  # seconds
        renewal_interval = 25  # seconds (renew before 30s server timeout)
        elapsed = 0
        try:
            while elapsed < max_duration:
                await asyncio.sleep(renewal_interval)
                elapsed += renewal_interval
                if not self._client:
                    break
                await self._client.room_typing(
                    room_id, typing_state=True, timeout=timeout
                )
        except asyncio.CancelledError:
            pass
        except Exception as exc:
            logger.debug(
                "MatrixChannel: typing renewal failed for %s: %s", room_id, exc
            )
        finally:
            # If we hit the 2-min cap, explicitly stop typing
            if elapsed >= max_duration and self._client:
                try:
                    await self._client.room_typing(room_id, typing_state=False)
                except Exception:
                    pass
            self._typing_tasks.pop(room_id, None)

    # ------------------------------------------------------------------
    # build_agent_request_from_native (BaseChannel protocol)
    # ------------------------------------------------------------------

    def _build_content_part(self, p: dict[str, Any]) -> Any:
        """Convert a native content-part dict to a CoPaw Content object."""
        t = p.get("type")
        if t == "text" and p.get("text"):
            return TextContent(type=ContentType.TEXT, text=p["text"])
        if t == "image" and p.get("image_url"):
            if not self._cfg.vision_enabled:
                # Downgrade silently; _on_room_media_event should have already
                # converted this, but guard here for any code path that builds
                # content_parts directly.
                return TextContent(
                    type=ContentType.TEXT,
                    text="[Image omitted: current model does not support image input]",
                )
            return ImageContent(type=ContentType.IMAGE, image_url=p["image_url"])
        if t == "file":
            return FileContent(
                type=ContentType.FILE,
                file_url=p.get("file_url", ""),
            )
        if t == "audio" and p.get("data"):
            return AudioContent(type=ContentType.AUDIO, data=p["data"])
        if t == "video" and p.get("video_url"):
            return VideoContent(type=ContentType.VIDEO, video_url=p["video_url"])
        return None

    def build_agent_request_from_native(self, native_payload: Any) -> Any:
        parts = native_payload.get("content_parts", [])
        meta = native_payload.get("meta", {})
        sender_id = native_payload.get("sender_id", "")
        room_id = meta.get("room_id", sender_id)
        session_id = f"matrix:{room_id}"

        content = [
            obj for p in parts
            if (obj := self._build_content_part(p)) is not None
        ]
        if not content:
            content = [TextContent(type=ContentType.TEXT, text="")]

        # Use room_id as the AgentRequest user_id so that all participants
        # in the same room share one session (CoPaw keys session state on
        # both session_id AND user_id).  The real sender is preserved in
        # meta["sender_id"] for reply mentions.
        req = self.build_agent_request_from_user_content(
            channel_id=CHANNEL_KEY,
            sender_id=room_id,
            session_id=session_id,
            content_parts=content,
            channel_meta=meta,
        )
        req.channel_meta = meta  # type: ignore[attr-defined]
        return req

    def resolve_session_id(self, sender_id: str, channel_meta=None) -> str:
        room_id = (channel_meta or {}).get("room_id", sender_id)
        return f"matrix:{room_id}"

    def get_to_handle_from_request(self, request: Any) -> str:
        meta = getattr(request, "channel_meta", {}) or {}
        return meta.get("room_id", getattr(request, "user_id", ""))

    # ------------------------------------------------------------------
    # Mention helper — build spec-compliant Matrix mention
    # ------------------------------------------------------------------

    def _apply_mention(
        self, content: dict[str, Any], user_id: str, room_id: str
    ) -> None:
        """Add a full Matrix mention to an outgoing event content dict.

        Sets ``m.mentions`` (for push notifications) and adds a
        ``formatted_body`` with an HTML pill so clients render the
        mention visually.
        """
        content["m.mentions"] = {"user_ids": [user_id]}

        display_name = self._resolve_display_name(user_id, room_id)
        pill = (
            f'<a href="https://matrix.to/#/{user_id}">'
            f"{display_name}</a>"
        )

        body = content.get("body", "")
        # Reuse already-converted formatted_body (set by send()) or convert now
        html_body = content.get("formatted_body") or _md_to_html(body)
        content["format"] = "org.matrix.custom.html"
        content["formatted_body"] = f"{pill} {html_body}" if html_body else pill
        # Prepend plain-text fallback so non-HTML clients also see the mention
        content["body"] = f"{display_name} {body}" if body else display_name

    def _resolve_display_name(self, user_id: str, room_id: str) -> str:
        """Best-effort display name for *user_id* in *room_id*."""
        if self._client:
            room = self._client.rooms.get(room_id)
            if room:
                try:
                    name = room.user_name(user_id)
                    if name:
                        return name
                except Exception:
                    pass
        return user_id.split(":")[0].lstrip("@") or user_id

    # ------------------------------------------------------------------
    # Outgoing send — text
    # ------------------------------------------------------------------

    async def send(
        self,
        to_handle: str,
        text: str,
        meta: Optional[Dict[str, Any]] = None,
    ) -> None:
        if not self._client:
            logger.error("MatrixChannel: send called but client not ready")
            return

        room_id = to_handle
        content: dict[str, Any] = {
            "msgtype": "m.text",
            "body": text,
            "format": "org.matrix.custom.html",
            "formatted_body": _md_to_html(text),
        }

        sender_id = (meta or {}).get("sender_id") or (meta or {}).get("user_id")
        if sender_id:
            self._apply_mention(content, sender_id, room_id)

        try:
            await self._client.room_send(
                room_id, "m.room.message", content,
                ignore_unverified_devices=True,
            )
        except Exception as exc:
            logger.exception(
                "MatrixChannel: send failed to %s: %s", room_id, exc
            )
        finally:
            await self._send_typing(room_id, False)

    # ------------------------------------------------------------------
    # Outgoing send — media
    # ------------------------------------------------------------------

    async def send_media(
        self,
        to_handle: str,
        part: Any,
        meta: Optional[Dict[str, Any]] = None,
    ) -> None:
        """Upload a local file to Matrix and send as m.image / m.file / etc."""
        if not self._client:
            return

        room_id = to_handle
        t = getattr(part, "type", None)

        # Extract the local file reference from the content part
        if t == ContentType.IMAGE:
            file_ref = getattr(part, "image_url", "")
            matrix_msgtype = "m.image"
        elif t == ContentType.VIDEO:
            file_ref = getattr(part, "video_url", "")
            matrix_msgtype = "m.video"
        elif t == ContentType.AUDIO:
            file_ref = getattr(part, "data", "")
            matrix_msgtype = "m.audio"
        elif t == ContentType.FILE:
            file_ref = getattr(part, "file_url", "") or getattr(part, "file_id", "")
            matrix_msgtype = "m.file"
        else:
            return

        if not file_ref:
            return

        # Upload to Matrix media repository
        mxc_uri = await self._upload_file(file_ref)
        if not mxc_uri:
            logger.warning(
                "MatrixChannel: send_media upload failed for %s", file_ref
            )
            return

        # Build and send the Matrix room event
        try:
            path_str = file_ref.removeprefix("file://")
            filename = os.path.basename(path_str) or "file"
            mime_type, _ = mimetypes.guess_type(path_str)
            mime_type = mime_type or "application/octet-stream"
            try:
                file_size = os.path.getsize(path_str)
            except OSError:
                file_size = 0

            event_content: dict[str, Any] = {
                "msgtype": matrix_msgtype,
                "body": filename,
                "url": mxc_uri,
                "info": {
                    "mimetype": mime_type,
                    "size": file_size,
                },
            }
            sender_id = (meta or {}).get("sender_id") or (meta or {}).get("user_id")
            if sender_id:
                self._apply_mention(event_content, sender_id, room_id)

            await self._client.room_send(
                room_id, "m.room.message", event_content,
                ignore_unverified_devices=True,
            )
            logger.debug(
                "MatrixChannel: sent %s %s to %s", matrix_msgtype, filename, room_id
            )
        except Exception as exc:
            logger.exception(
                "MatrixChannel: send_media failed for %s: %s", room_id, exc
            )
