# -*- coding: utf-8 -*-
"""Read QwenCode's live session status (the console status indicator).

The web shell derives its indicator from ``/workspaces/<ws>/sessions/live-state``:
per-session ``activeWorkState``, ``hasActivePrompt``,
``hasRunningBackgroundTasks``, ``isWaitingForPermission`` and
``isWaitingForUserQuestion``. The plugin has no UI, so this module reads the
same endpoint with the serve token and exposes a derived indicator.
"""

from __future__ import annotations

import urllib.parse
from typing import Any

import httpx


def derive_indicator(payload: dict[str, Any] | None, reachable: bool = True) -> str:
    if not reachable:
        return "offline"
    sessions = (payload or {}).get("sessions") or []
    if not isinstance(sessions, list):
        return "unknown"
    if any(s.get("isWaitingForPermission") for s in sessions if isinstance(s, dict)):
        return "waiting_permission"
    if any(s.get("isWaitingForUserQuestion") for s in sessions if isinstance(s, dict)):
        return "waiting_question"
    for session in sessions:
        if not isinstance(session, dict):
            continue
        if (
            session.get("hasActivePrompt")
            or session.get("hasRunningBackgroundTasks")
            or str(session.get("activeWorkState") or "idle") != "idle"
        ):
            return "busy"
    return "idle"


def is_busy(payload: dict[str, Any] | None, reachable: bool = True) -> bool:
    return derive_indicator(payload, reachable) in {
        "busy",
        "waiting_permission",
        "waiting_question",
    }


async def fetch_live_state(workspace: str) -> dict[str, Any]:
    # Lazy imports keep this module usable in unit tests without the app.
    from .web import serve_token, serve_url

    url = serve_url()
    token = serve_token()
    if not url:
        return {"reachable": False, "indicator": "offline", "workspace": workspace}
    encoded = urllib.parse.quote(str(workspace), safe="")
    headers = {"Authorization": f"Bearer {token}"} if token else {}
    try:
        async with httpx.AsyncClient(timeout=3.0) as client:
            resp = await client.get(
                f"{url}/workspaces/{encoded}/sessions/live-state",
                headers=headers,
            )
        if resp.status_code != 200:
            return {
                "reachable": False,
                "indicator": "offline",
                "workspace": workspace,
                "status_code": resp.status_code,
            }
        payload = resp.json()
        return {
            "reachable": True,
            "workspace": workspace,
            "indicator": derive_indicator(payload),
            "sessions": payload.get("sessions") or [],
            "catalog_version": payload.get("catalogVersion"),
        }
    except Exception as exc:  # offline must never raise into a caller
        return {
            "reachable": False,
            "indicator": "offline",
            "workspace": workspace,
            "error": str(exc)[:200],
        }


async def session_busy(workspace: str, session_id: str) -> bool | None:
    """Three-state liveness probe for one CLI session.

    ``True``  — the session reports active work (prompt/background/
                 permission/question/non-idle work state);
    ``False`` — the endpoint answered and the session is genuinely idle;
    ``None``  — the probe is unreachable/unknown. Callers must treat
                 *unknown* as "possibly alive", never as idle: a transient
                 serve hiccup must not kill a working session (10/23
                 incident: ``False`` here killed a live run).
    """
    state = await fetch_live_state(workspace)
    if not state.get("reachable"):
        return None
    for session in state.get("sessions") or []:
        if str(session.get("sessionId") or "") == str(session_id):
            return bool(
                session.get("hasActivePrompt")
                or session.get("hasRunningBackgroundTasks")
                or session.get("isWaitingForPermission")
                or session.get("isWaitingForUserQuestion")
                or str(session.get("activeWorkState") or "idle") != "idle"
            )
    # Reachable but the session is not registered (e.g. a resumed/foreign
    # session): the probe cannot speak for it — unknown, not idle.
    return None
