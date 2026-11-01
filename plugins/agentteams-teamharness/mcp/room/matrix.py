# matrix env/user/membership helpers (W1-a2: real module)
from __future__ import annotations

import json

import os

from typing import Any

import urllib.parse

import urllib.request

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.common.config import _load_runtime_config, _section


def _matrix_env(tool: str) -> tuple[str, str]:
    homeserver = os.getenv("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
    token = os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "")
    if not homeserver or not token:
        raise ValueError("AGENTTEAMS_MATRIX_URL and AGENTTEAMS_WORKER_MATRIX_TOKEN are required")
    return homeserver, token


def _matrix_user_id() -> str:
    explicit = os.getenv("AGENTTEAMS_MATRIX_USER_ID", "").strip()
    if explicit:
        return explicit
    member = _section(_load_runtime_config(), "member")
    matrix_user_id = str(member.get("matrixUserId") or member.get("matrix_user_id") or "").strip()
    if matrix_user_id:
        return matrix_user_id
    name = os.getenv("AGENTTEAMS_WORKER_NAME", "").strip()
    domain = os.getenv("AGENTTEAMS_MATRIX_DOMAIN", "").strip()
    if name and domain:
        return f"@{name}:{domain}"
    return ""


def _ensure_matrix_room_members(room_id: str, invite: list[str]) -> None:
    current = set(_matrix_room_member_user_ids(room_id))
    for user_id in invite:
        if user_id and user_id not in current:
            _matrix_invite_to_room(room_id, user_id)


def _matrix_invite_to_room(room_id: str, user_id: str) -> None:
    homeserver, token = _matrix_env("roomflow")
    encoded_room = urllib.parse.quote(room_id, safe="")
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/invite",
        data=json.dumps({"user_id": user_id}).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        response.read()


def _matrix_room_member_user_ids(room_id: str, memberships: set[str] | None = None) -> list[str]:
    homeserver, token = _matrix_env("roomflow")
    encoded_room = urllib.parse.quote(room_id, safe="")
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/members",
        headers={"Authorization": f"Bearer {token}"},
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        data = json.loads(response.read().decode("utf-8") or "{}")
    allowed = memberships or {"join", "invite"}
    members: list[str] = []
    for event in data.get("chunk", []):
        if not isinstance(event, dict):
            continue
        user_id = str(event.get("state_key") or "").strip()
        content = event.get("content") if isinstance(event.get("content"), dict) else {}
        membership = str(content.get("membership") or "").strip()
        if user_id and membership in allowed:
            members.append(user_id)
    return members


def _team_leader_matrix_id() -> str:
    """Resolve the team leader's Matrix user ID from the runtime config.

    The controller projects the full team roster (with roles and Matrix
    user IDs) into the worker's runtime config. Returns an empty string
    when no leader entry exists (standalone runs) so callers can skip
    the notification instead of failing.
    """
    config = _load_runtime_config()
    team = _section(config, "team")
    members = team.get("members")
    if not isinstance(members, list):
        return ""
    for member in members:
        if not isinstance(member, dict):
            continue
        role = str(member.get("role") or "").strip().lower().replace("_", "-")
        if role in {"team-leader", "teamleader", "leader"}:
            return str(member.get("matrixUserId") or member.get("matrix_user_id") or "").strip()
    return ""


def _team_human_matrix_ids() -> list[str]:
    """Resolve Matrix IDs of human (non-leader, non-worker) team members.

    Same runtime-config source as ``_team_leader_matrix_id``: the
    controller projects the full roster (including humans and the team
    admin) into ``team.members`` with their roles. Humans are every
    member whose role is neither leader nor worker, plus the
    ``team.admin`` entry. Returns an empty list on standalone runs so
    callers skip the extra mentions instead of failing.
    """
    config = _load_runtime_config()
    team = _section(config, "team")
    ids: list[str] = []

    def _add(user_id: Any) -> None:
        user_id = str(user_id or "").strip()
        if user_id.startswith("@") and user_id not in ids:
            ids.append(user_id)

    admin = _section(team, "admin")
    _add(admin.get("matrixUserId") or admin.get("matrix_user_id"))
    members = team.get("members")
    if not isinstance(members, list):
        return ids
    for member in members:
        if not isinstance(member, dict):
            continue
        role = str(member.get("role") or "").strip().lower().replace("_", "-")
        if role in {"team-leader", "teamleader", "leader", "worker"}:
            continue
        _add(member.get("matrixUserId") or member.get("matrix_user_id"))
    return ids


