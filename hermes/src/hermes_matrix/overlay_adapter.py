"""AgentTeams policy overlay on hermes-agent's native Matrix adapter.

This module no longer reimplements Matrix transport with ``matrix-nio``.
Instead, it subclasses hermes-agent's stock mautrix-based adapter and only
injects the AgentTeams-specific policy layer that differs from upstream:

  * outbound ``m.mentions`` enrichment for full-MXID pings
  * separate DM / group allow-lists
  * copaw-style history buffering for unmentioned group chatter
  * image downgrade when the active model has no vision capability

All transport-heavy capabilities (media upload/download, streaming edits,
typing, reactions, threads, E2EE, read receipts) stay in the upstream adapter.
"""
from __future__ import annotations

import logging
import os
import types
from typing import Any, Dict, Optional

from gateway.config import PlatformConfig
from gateway.platforms._matrix_native import MatrixAdapter as _NativeMatrixAdapter

from hermes_matrix.policies import (
    DualAllowList,
    HistoryBuffer,
    apply_outbound_mentions,
)

logger = logging.getLogger(__name__)
_IMAGE_FILENAME_EXTENSIONS = frozenset({
    ".png",
    ".jpg",
    ".jpeg",
    ".gif",
    ".webp",
    ".bmp",
    ".tiff",
    ".tif",
    ".heic",
    ".heif",
})


def _truthy_env(name: str, default: bool = False) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    return raw.strip().lower() in ("true", "1", "yes", "on")


def _is_commandish(text: str) -> bool:
    return (text or "").startswith(("/", "!"))


def _normalize_image_body(body: str) -> str:
    """Drop Matrix transport filenames so image-only messages stay image-first.

    Matrix image events commonly populate ``body`` with the uploaded filename
    even when the user did not provide any textual caption. Feeding that raw
    filename to Hermes causes the downstream agent to treat the turn like a
    file lookup (for example ``search_files("foo.png")``) instead of relying
    on the already-cached image plus automatic vision enrichment.
    """
    stripped = (body or "").strip()
    if not stripped:
        return ""
    if "\n" in stripped or "\r" in stripped:
        return stripped

    leaf = stripped.rsplit("/", 1)[-1].rsplit("\\", 1)[-1]
    stem, ext = os.path.splitext(leaf)
    if stem and ext.lower() in _IMAGE_FILENAME_EXTENSIONS:
        return ""
    return stripped


def _describe_dropped_media(source_content: dict, fallback_body: str) -> str:
    """Return a copaw-style textual summary for a media event."""
    body = (fallback_body or "").strip()
    msgtype = source_content.get("msgtype", "")
    if msgtype == "m.image":
        return f"[sent an image: {body}]" if body else "[sent an image]"
    if msgtype == "m.file":
        return f"[sent a file: {body}]" if body else "[sent a file]"
    if msgtype == "m.audio":
        return f"[sent audio: {body}]" if body else "[sent audio]"
    if msgtype == "m.video":
        return f"[sent a video: {body}]" if body else "[sent a video]"
    return body


class MatrixAdapter(_NativeMatrixAdapter):
    """Thin subclass that overlays AgentTeams policy on hermes native Matrix."""

    def __init__(self, config: PlatformConfig):
        super().__init__(config)
        self._dual_allow = DualAllowList.from_env()
        self._history = HistoryBuffer.from_env()
        self._vision_enabled = _truthy_env("MATRIX_VISION_ENABLED", default=False)

    async def connect(self) -> bool:
        ok = await super().connect()
        if ok and self._client is not None:
            self._wrap_send_message_event()
        return ok

    def _wrap_send_message_event(self) -> None:
        """Inject MSC3952 ``m.mentions`` into every outgoing Matrix event."""
        if self._client is None:
            return
        if getattr(self._client, "_hiclaw_mentions_wrapped", False):
            return

        original = self._client.send_message_event

        async def wrapped(
            client: Any,
            room_id: Any,
            event_type: Any,
            content: Any,
            *args: Any,
            **kwargs: Any,
        ) -> Any:
            if isinstance(content, dict):
                apply_outbound_mentions(content, self_user_id=self._user_id)
            return await original(room_id, event_type, content, *args, **kwargs)

        self._client.send_message_event = types.MethodType(wrapped, self._client)
        self._client._hiclaw_mentions_wrapped = True  # type: ignore[attr-defined]
        logger.debug("Matrix: wrapped send_message_event for outbound mentions")

    async def _resolve_message_context(
        self,
        room_id: str,
        sender: str,
        event_id: str,
        body: str,
        source_content: dict,
        relates_to: dict,
    ) -> Optional[tuple]:
        """Apply AgentTeams allow/history policy around native mention gating."""
        is_dm = await self._is_dm_room(room_id)
        if not self._dual_allow.permits(sender, is_dm=is_dm):
            return None

        ctx = await super()._resolve_message_context(
            room_id,
            sender,
            event_id,
            body,
            source_content,
            relates_to,
        )
        if ctx is None:
            if not is_dm:
                display = await self._get_display_name(room_id, sender)
                history_body = _describe_dropped_media(source_content, body)
                self._history.record(room_id, display, history_body)
            return None

        body, is_dm, chat_type, thread_id, display_name, source = ctx
        if is_dm:
            return body, is_dm, chat_type, thread_id, display_name, source

        if _is_commandish(body):
            # Match copaw: slash commands do not receive a history prefix and
            # should clear any accumulated unmentioned context.
            self._history.clear(room_id)
            return body, is_dm, chat_type, thread_id, display_name, source

        body = f"{display_name}: {body}"
        history_prefix = self._history.drain(room_id)
        if history_prefix:
            body = history_prefix + body
        return body, is_dm, chat_type, thread_id, display_name, source

    async def _handle_media_message(
        self,
        room_id: str,
        sender: str,
        event_id: str,
        event_ts: float,
        source_content: Dict[str, Any],
        relates_to: Dict[str, Any],
        msgtype: str,
    ) -> None:
        """Downgrade images to text when the active model lacks vision."""
        if msgtype == "m.image" and not self._vision_enabled:
            body = source_content.get("body", "") or ""
            display_body = body or "image"
            text_content = dict(source_content)
            text_content["msgtype"] = "m.text"
            text_content["body"] = (
                "[User sent an image (current model does not support image "
                f"input): {display_body}]"
            )
            await self._handle_text_message(
                room_id,
                sender,
                event_id,
                event_ts,
                text_content,
                relates_to,
            )
            return

        if msgtype == "m.image":
            normalized_content = dict(source_content)
            normalized_body = _normalize_image_body(
                source_content.get("body", "") or ""
            )
            if normalized_body != (source_content.get("body", "") or ""):
                logger.debug(
                    "Matrix: stripping transport filename from inbound image body %r",
                    source_content.get("body", ""),
                )
                normalized_content["body"] = normalized_body
            await super()._handle_media_message(
                room_id,
                sender,
                event_id,
                event_ts,
                normalized_content,
                relates_to,
                msgtype,
            )
            return

        await super()._handle_media_message(
            room_id,
            sender,
            event_id,
            event_ts,
            source_content,
            relates_to,
            msgtype,
        )
