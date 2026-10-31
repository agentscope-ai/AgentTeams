# -*- coding: utf-8 -*-
"""Native QwenPaw chat-list integration (sidebar exposure).

Every dispatch run is surfaced in the console's left sidebar as a chat
inside a pinned custom group (default name ``QwenCode``). The plugin
uses the app's own chats API — no frontend injection:

    GET  /api/chats/groups            list groups
    POST /api/chats/groups            create a custom group
    PUT  /api/chats/groups/{id}       rename / pin a group
    POST /api/chats                   create a chat
    PUT  /api/chats/{chat_id}         rename / pin / move a chat

All calls are best-effort: a chat-sync failure is recorded on the task
but never breaks the dispatch itself.
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from typing import Any

from .endpoints import app_base

DEFAULT_GROUP_NAME = "QwenCode"
CHAT_CHANNEL = "qwen-code-bridge"
_AGENT_ENV = "QWEN_CODE_BRIDGE_AGENT_ID"
_DEFAULT_URL = "http://127.0.0.1:8088"


def resolve_root(base_url: str | None = None) -> str:
    return app_base(base_url)


def resolve_agent(agent_id: str | None = None) -> str:
    return (
        str(agent_id or "").strip()
        or os.environ.get(_AGENT_ENV, "").strip()
        or "default"
    )


def _request(
    method: str,
    url: str,
    body: dict | None,
    agent_id: str,
    timeout: float = 5.0,
) -> Any:
    data = (
        json.dumps(body, ensure_ascii=False).encode("utf-8")
        if body is not None
        else None
    )
    req = urllib.request.Request(
        url,
        data=data,
        headers={
            "Content-Type": "application/json",
            "X-Agent-Id": agent_id,
        },
        method=method,
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        raw = resp.read()
    if not raw:
        return None
    try:
        return json.loads(raw.decode("utf-8"))
    except ValueError:
        return None


def ensure_group(
    root: str,
    agent_id: str,
    name: str = DEFAULT_GROUP_NAME,
) -> str:
    """Return the pinned custom group id, creating/pinning it if needed."""
    groups = _request("GET", root + "/api/chats/groups", None, agent_id) or []
    for group in groups:
        if isinstance(group, dict) and str(group.get("name") or "") == name:
            gid = str(group.get("id") or "")
            if gid and not group.get("pinned"):
                _request(
                    "PUT",
                    f"{root}/api/chats/groups/{gid}",
                    {"pinned": True},
                    agent_id,
                )
            return gid
    created = _request(
        "POST",
        root + "/api/chats/groups",
        {"name": name},
        agent_id,
    ) or {}
    gid = str(created.get("id") or "")
    if gid:
        _request(
            "PUT",
            f"{root}/api/chats/groups/{gid}",
            {"pinned": True},
            agent_id,
        )
    return gid


def create_run_chat(
    root: str,
    agent_id: str,
    *,
    name: str,
    session_id: str,
    user_id: str,
    group_id: str,
    meta: dict | None = None,
) -> str:
    """Create the sidebar chat for one dispatch run; returns chat id."""
    payload = {
        "name": name,
        "session_id": session_id,
        "user_id": user_id,
        "channel": CHAT_CHANNEL,
        "source": "chat",
        "group_id": group_id or None,
        "meta": meta or {},
    }
    created = _request("POST", root + "/api/chats", payload, agent_id) or {}
    return str(created.get("id") or "")


DIRECT_SESSION_ID = "qwen-code-bridge:direct"
DIRECT_CHAT_NAME = "QwenCode · 直接对话"


def ensure_direct_chat(
    root: str,
    agent_id: str,
    group_id: str,
    user_id: str = "default",
) -> str:
    """Return the group's direct-chat id, creating it once if needed."""
    existing = _request("GET", root + "/api/chats", None, agent_id) or []
    for chat in existing:
        if not isinstance(chat, dict):
            continue
        if (
            str(chat.get("channel") or "") == CHAT_CHANNEL
            and str(chat.get("session_id") or "") == DIRECT_SESSION_ID
        ):
            return str(chat.get("id") or "")
    return create_run_chat(
        root,
        agent_id,
        name=DIRECT_CHAT_NAME,
        session_id=DIRECT_SESSION_ID,
        user_id=user_id,
        group_id=group_id,
        meta={"kind": "direct", "product": "qwen-code-bridge"},
    )


def rename_chat(
    root: str,
    agent_id: str,
    chat_id: str,
    name: str,
) -> bool:
    if not chat_id:
        return False
    try:
        _request(
            "PUT",
            f"{root}/api/chats/{chat_id}",
            {"name": name},
            agent_id,
        )
        return True
    except Exception:
        return False


def run_chat_name(task: str, cli: str, state: str) -> str:
    return f"{task} · {cli} · {state}"
