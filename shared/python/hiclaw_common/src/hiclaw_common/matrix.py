"""Mautrix-based Matrix client for HiClaw worker runtimes.

Provides MautrixRelay: a composable (not subclassed) async Matrix client that
wraps mautrix.client.Client and applies HiClaw policy (DualAllowList,
HistoryBuffer, MSC3952 mentions) around each inbound message.

Each inbound Matrix message calls the provided ``on_invoke`` coroutine, which
is expected to return (reply_text, new_session_id). The relay stays stateless
with respect to session IDs — the caller (worker.py) owns that state.

Also provides ``matrix_relogin``: a helper that re-authenticates via the
Matrix password login API to obtain a fresh access token.  This is needed
because E2EE session keys are tied to a specific device credential chain —
reusing a stale access_token after a pod restart prevents key decryption.

Requires: mautrix[encryption]>=0.20 (see hiclaw-common[matrix] optional extra).
"""
from __future__ import annotations

import asyncio
import json
import logging
import re
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Awaitable, Callable, Optional

from mautrix.client import Client
from mautrix.types import (
    EventType,
    Membership,
    RoomID,
    StateEvent,
    UserID,
)
from mautrix.types.event.message import MessageEvent

from hiclaw_common.policies import DualAllowList, HistoryBuffer, apply_outbound_mentions

logger = logging.getLogger(__name__)

_STARTUP_GRACE_MS = 10_000

try:
    from markdown_it import MarkdownIt as _MarkdownIt
    _md = _MarkdownIt()
    _HAVE_MD = True
except ImportError:
    _HAVE_MD = False

_THINK_RE = re.compile(r"<think>(.*?)</think>", re.DOTALL)


def _to_html(text: str) -> str:
    """Convert reply text (markdown + <think> tags) to Matrix-compatible HTML.

    <think>...</think> → <blockquote><em>💭 ...</em></blockquote>
    Markdown bold/blockquote/code rendered via markdown-it-py when available.
    """
    def _render_think(m: re.Match) -> str:
        inner = m.group(1).strip()
        if _HAVE_MD:
            inner_html = _md.render(inner).strip()
        else:
            inner_html = inner.replace("\n", "<br/>")
        return f"<blockquote>💭 {inner_html}</blockquote>"

    html = _THINK_RE.sub(_render_think, text)

    if _HAVE_MD:
        return _md.render(html)

    # Minimal regex fallback: bold + inline code only
    html = re.sub(r"\*\*(.+?)\*\*", r"<strong>\1</strong>", html)
    html = re.sub(r"`([^`]+)`", r"<code>\1</code>", html)
    return html
_TYPING_KEEPALIVE_INTERVAL = 30  # seconds between keepalive renewals
_TYPING_TIMEOUT_MS = 40_000       # mautrix typing timeout per renewal


class MautrixRelay:
    """Policy-enforcing Matrix relay backed by mautrix.client.Client."""

    def __init__(
        self,
        homeserver: str,
        user_id: str,
        access_token: str,
        device_id: str,
        policies: DualAllowList,
        history: HistoryBuffer,
        on_invoke: Callable[[str], Awaitable[tuple[str, Optional[str]]]],
        media_dir: Optional[Path] = None,
    ) -> None:
        self._user_id = user_id
        self._policies = policies
        self._history = history
        self._on_invoke = on_invoke
        self._media_dir = media_dir
        self._start_ms = int(time.time() * 1000) - _STARTUP_GRACE_MS
        self._dm_cache: dict[str, bool] = {}

        self._client = Client(
            mxid=UserID(user_id),
            base_url=homeserver,
            token=access_token,
            device_id=device_id,
        )
        self._client.add_event_handler(EventType.ROOM_MESSAGE, self._handle_message)
        self._client.add_event_handler(EventType.ROOM_MEMBER, self._handle_member)

    async def run(self) -> None:
        await self._client.start(None)

    async def stop(self) -> None:
        self._client.stop()

    async def _is_dm(self, room_id: RoomID) -> bool:
        rid = str(room_id)
        if rid in self._dm_cache:
            return self._dm_cache[rid]
        try:
            members = await self._client.get_joined_members(room_id)
            is_dm = len(members) <= 2
        except Exception:
            is_dm = False
        self._dm_cache[rid] = is_dm
        return is_dm

    async def _handle_member(self, event: StateEvent) -> None:
        if not isinstance(event.content, object):
            return
        membership = getattr(event.content, "membership", None)
        if membership == Membership.INVITE and str(event.state_key) == self._user_id:
            logger.info("Received invite to %s — joining", event.room_id)
            try:
                await self._client.join_room(event.room_id)
            except Exception as exc:
                logger.error("Failed to join room %s: %s", event.room_id, exc)
        elif membership in (Membership.JOIN, Membership.LEAVE, Membership.BAN):
            self._dm_cache.pop(str(event.room_id), None)

    async def _handle_message(self, event: MessageEvent) -> None:
        try:
            await self._process_message(event)
        except Exception as exc:
            logger.error("Error processing message in %s: %s", event.room_id, exc)

    async def _download_media(self, event: MessageEvent) -> str:
        """Download media attachment and return an augmented message prefix.

        Returns empty string when the event is not a media message or when
        ``media_dir`` was not provided.  On success returns a line like:
          [Image saved to: /workspace/image.png]
        so Claude can read the file by path.
        """
        if not self._media_dir:
            return ""

        msgtype = str(getattr(event.content, "msgtype", "") or "")
        _media_types = {"m.image": "Image", "m.file": "File", "m.video": "Video", "m.audio": "Audio"}
        if msgtype not in _media_types:
            return ""

        mxc_url = getattr(event.content, "url", None)
        if not mxc_url:
            return ""

        try:
            data: bytes = await self._client.download_media(mxc_url)
        except Exception as exc:
            logger.warning("Failed to download media %s: %s", mxc_url, exc)
            return ""

        filename = re.sub(r"[^\w.\-]", "_", getattr(event.content, "body", "") or "attachment")
        dest = self._media_dir / filename
        stem = dest.stem
        suffix = dest.suffix
        counter = 1
        while dest.exists():
            dest = self._media_dir / f"{stem}_{counter}{suffix}"
            counter += 1

        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_bytes(data)
        logger.info("Downloaded %s to %s (%d bytes)", msgtype, dest, len(data))
        return f"[{_media_types[msgtype]} saved to: {dest}]\n"

    async def _process_message(self, event: MessageEvent) -> None:
        if str(event.sender) == self._user_id:
            return

        if getattr(event, "timestamp", 0) < self._start_ms:
            return

        body = getattr(event.content, "body", "") or ""
        if not body:
            return

        room_id = event.room_id
        is_dm = await self._is_dm(room_id)
        sender = str(event.sender)

        if not self._policies.permits(sender, is_dm):
            if not is_dm:
                self._history.record(str(room_id), sender, body)
            return

        context = ""
        if not is_dm:
            context = self._history.drain(str(room_id))

        media_prefix = await self._download_media(event)
        full_message = context + media_prefix + body

        keepalive = asyncio.create_task(self._typing_keepalive(room_id))
        try:
            reply, _new_sid = await self._on_invoke(full_message)
        except Exception as exc:
            logger.error("invoke failed: %s", exc)
            reply = f"Sorry, an error occurred: {exc}"
        finally:
            keepalive.cancel()
            try:
                await self._client.set_typing(room_id, timeout=0)
            except Exception:
                pass

        content: dict = {
            "msgtype": "m.text",
            "body": reply,
            "format": "org.matrix.custom.html",
            "formatted_body": _to_html(reply),
        }
        apply_outbound_mentions(content, self_user_id=self._user_id)

        try:
            await self._client.send_message_event(room_id, EventType.ROOM_MESSAGE, content)
        except Exception as exc:
            logger.error("send_message_event failed: %s", exc)

    async def _typing_keepalive(self, room_id: RoomID) -> None:
        while True:
            try:
                await self._client.set_typing(room_id, timeout=_TYPING_TIMEOUT_MS)
            except Exception:
                pass
            await asyncio.sleep(_TYPING_KEEPALIVE_INTERVAL)


def matrix_relogin(
    homeserver: str,
    worker_name: str,
    password: str,
) -> Optional[tuple[str, str]]:
    """Re-authenticate via Matrix password login. Returns (access_token, device_id) or None."""
    login_url = f"{homeserver.rstrip('/')}/_matrix/client/v3/login"
    body = json.dumps({
        "type": "m.login.password",
        "identifier": {"type": "m.id.user", "user": worker_name},
        "password": password,
    }).encode()
    try:
        req = urllib.request.Request(
            login_url,
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = json.loads(resp.read())
    except (urllib.error.URLError, ValueError, TimeoutError) as exc:
        logger.warning("Matrix re-login failed: %s", exc)
        return None

    token = payload.get("access_token", "")
    device = payload.get("device_id", "")
    if not token:
        logger.warning("Matrix re-login returned no token")
        return None
    return token, device
