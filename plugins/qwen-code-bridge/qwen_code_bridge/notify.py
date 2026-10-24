# -*- coding: utf-8 -*-
"""Human-facing notifications for dispatch runs.

The dispatch worker runs inside the host (QwenPaw), so "tell the human"
must use the host platform's own surface instead of injecting a chat
turn:

* **Standalone QwenPaw + QwenCode** — the console inbox
  (``qwenpaw.app.inbox_store.append_event``; the UI reads
  ``/console/inbox/events``).
* **AgentTeams** — the task/project room, via the same
  ``POST /api/messages/send`` path the bridge already uses for the
  terminal room bell (Matrix room + @human).

Routing: ``QWEN_CODE_BRIDGE_NOTIFY=auto|inbox|room`` (default ``auto`` =
room when the dispatch origin channel is Matrix, inbox otherwise).
Every path is best-effort: a failed notification must never block the
approval/control flow it belongs to.
"""

from __future__ import annotations

import json
import os
import urllib.request
from typing import Any

from .endpoints import app_base

NOTIFY_ENV = "QWEN_CODE_BRIDGE_NOTIFY"

TARGET_INBOX = "inbox"
TARGET_ROOM = "room"


def notify_target(origin: dict[str, Any] | None = None) -> str:
    """Resolve the notification surface for this dispatch."""
    forced = str(os.environ.get(NOTIFY_ENV, "") or "").strip().lower()
    if forced in (TARGET_INBOX, TARGET_ROOM):
        return forced
    channel = str((origin or {}).get("channel") or "").strip().lower()
    return TARGET_ROOM if channel == "matrix" else TARGET_INBOX


async def notify_inbox(
    *,
    agent_id: str,
    task_id: str,
    title: str,
    body: str,
    event_type: str = "needs_input",
    severity: str = "warning",
    payload: dict[str, Any] | None = None,
) -> bool:
    """Append one event to the QwenPaw console inbox (best-effort)."""
    try:
        from qwenpaw.app.inbox_store import append_event

        await append_event(
            agent_id=str(agent_id or "") or "default",
            source_type="qwen-code-bridge",
            source_id=str(task_id or ""),
            event_type=event_type,
            status="pending",
            title=title,
            body=body,
            severity=severity,
            payload=dict(payload or {}),
        )
        return True
    except Exception:
        return False


def notify_room(
    *,
    base_url: str,
    agent_id: str,
    channel: str,
    session_id: str,
    user_id: str,
    text: str,
) -> bool:
    """Post one message to the dispatch origin room (best-effort)."""
    try:
        root = app_base(base_url)
        body = {
            "channel": str(channel or "console"),
            "target_user": str(user_id or "default"),
            "target_session": str(session_id or ""),
            "text": text,
        }
        req = urllib.request.Request(
            root + "/api/messages/send",
            data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
            headers={
                "Content-Type": "application/json",
                "X-Agent-Id": agent_id or "AgentTeamGuide",
            },
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=10) as resp:
            resp.read(64)
        return True
    except Exception:
        return False


async def notify_human(
    *,
    task: dict[str, Any],
    title: str,
    body: str,
    payload: dict[str, Any] | None = None,
    event_type: str = "needs_input",
) -> str:
    """Notify the human on the right surface; returns the surface used."""
    origin = task.get("origin") or {}
    wake = task.get("wake") or {}
    agent_id = str(wake.get("agent_id") or "")
    channel = str(origin.get("channel") or wake.get("channel") or "console")
    session_id = str(origin.get("session_id") or wake.get("session_id") or "")
    user_id = str(origin.get("user_id") or wake.get("user_id") or "default")
    base_url = str(wake.get("base_url") or "")
    task_id = str(task.get("task_id") or "")

    target = notify_target(origin)
    if target == TARGET_ROOM:
        ok = notify_room(
            base_url=base_url,
            agent_id=agent_id,
            channel=channel,
            session_id=session_id,
            user_id=user_id,
            text=f"{title}\n{body}",
        )
        return TARGET_ROOM if ok else "none"
    if await notify_inbox(
        agent_id=agent_id,
        task_id=task_id,
        title=title,
        body=body,
        event_type=event_type,
        payload=payload,
    ):
        return TARGET_INBOX
    # Inbox unavailable (e.g. host module missing in a bare test env):
    # fall back to the room if we have a target, else give up quietly.
    if session_id and base_url:
        ok = notify_room(
            base_url=base_url,
            agent_id=agent_id,
            channel=channel,
            session_id=session_id,
            user_id=user_id,
            text=f"{title}\n{body}",
        )
        return TARGET_ROOM if ok else "none"
    return "none"
