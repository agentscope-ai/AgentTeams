# roomflow room create/list/archive and room meta bindings (W1-a2: real module)
from __future__ import annotations

import hashlib

import json

import time

from pathlib import Path

from typing import Any

import urllib.error

import urllib.parse

import urllib.request

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.common import refs
from mcp.common.config import _load_runtime_config, _runtime_team_admin_user_id, _section
from mcp.common.identity import _canonical_room_id
from mcp.common.io import _read_json
from mcp.common.payload import _payload, _safe_id, _string_list
from mcp.common.paths import _optional_workspace_dir
from mcp.roomflow_tool import RoomDescribeDeps, describe_room as _describe_room_impl
from .matrix import _ensure_matrix_room_members, _matrix_env, _matrix_user_id
from .mention import _matrix_target
from mcp.transport.route import _reply_route_from_requester

# Monkeypatch-sensitive (15-name set): resolve at call time.
_write_json = refs.proxy("_write_json")


def _roomflow_room_meta() -> dict[str, Any]:
    config = _load_runtime_config()
    team = _section(config, "team")
    meta: dict[str, Any] = {
        "schemaVersion": 1,
        "roomKind": "task_room",
        "lifecycle": "ephemeral",
        "createdBy": "teamharness",
    }
    team_name = str(team.get("name") or "").strip()
    if team_name:
        meta["teamName"] = team_name

    admin = _section(team, "admin")
    admin_user_id = str(admin.get("matrixUserId") or admin.get("matrix_user_id") or "").strip()
    if admin_user_id:
        admin_meta: dict[str, Any] = {"userId": admin_user_id}
        admin_name = str(admin.get("name") or admin.get("runtimeName") or admin.get("runtime_name") or "").strip()
        if admin_name:
            admin_meta["name"] = admin_name
        meta["teamAdmin"] = admin_meta

    members = team.get("members")
    if isinstance(members, list):
        worker_members: list[dict[str, Any]] = []
        human_members: list[dict[str, Any]] = []
        for member in members:
            if not isinstance(member, dict):
                continue
            user_id = str(member.get("matrixUserId") or member.get("matrix_user_id") or "").strip()
            if not user_id:
                continue
            role = str(member.get("role") or "").strip().lower().replace("_", "-")
            display_name = str(member.get("name") or member.get("runtimeName") or member.get("runtime_name") or "").strip()
            if role in {"team-leader", "teamleader", "leader"}:
                if "leaderWorker" not in meta:
                    leader_meta: dict[str, Any] = {"userId": user_id}
                    if display_name:
                        leader_meta["workerName"] = display_name
                    meta["leaderWorker"] = leader_meta
                continue
            if role == "worker":
                worker_meta: dict[str, Any] = {"userId": user_id}
                if display_name:
                    worker_meta["workerName"] = display_name
                worker_members.append(worker_meta)
                continue
            human_meta: dict[str, Any] = {"userId": user_id}
            if display_name:
                human_meta["name"] = display_name
            human_members.append(human_meta)
        if worker_members:
            meta["workerMembers"] = worker_members
        if human_members:
            meta["humanMembers"] = human_members
    return meta


def _write_matrix_room_meta(room_id: str, content: dict[str, Any]) -> None:
    homeserver, token = _matrix_env("roomflow")
    encoded_room = urllib.parse.quote(room_id, safe="")
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/state/room.meta/",
        data=json.dumps(content).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        response.read()


def _roomflow(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "create_task_room")
    payload = _payload(arguments)
    if action == "create_task_room":
        return _create_task_room(arguments, payload)
    if action == "list_rooms":
        return _list_rooms(arguments)
    if action == "describe_room":
        return _describe_room_impl(
            arguments,
            payload,
            RoomDescribeDeps(
                matrix_env=_matrix_env,
                matrix_user_id=_matrix_user_id,
                canonical_room_id=_canonical_room_id,
            ),
        )
    if action == "archive_room":
        return _archive_room(arguments, payload)
    return {"ok": False, "tool": "roomflow", "action": action, "error": f"unsupported action: {action}"}


def _task_room_name(value: Any) -> str:
    name = str(value or "").strip()
    lowered = name.lower()
    for prefix in ("task:", "task\uff1a"):
        if lowered.startswith(prefix):
            name = name[len(prefix) :].strip()
            break
    return f"TASK\uff1a{name}" if name else ""


def _create_task_room(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    try:
        raw_project_id = payload.get("projectId") or payload.get("project_id") or payload.get("taskId")
        project_id = _safe_id(raw_project_id, "projectId")
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": str(exc)}
    task_id = project_id
    name = _task_room_name(project_id)
    if not name:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": "projectId is required"}
    source = str(payload.get("source") or "").strip()
    topic = str(payload.get("topic") or "").strip()
    if not topic:
        suffix = f" [source: {source}]" if source else ""
        topic = f"Task room for {task_id}{suffix}"
    invite = _string_list(payload.get("invite") if "invite" in payload else arguments.get("invite"))
    admin = str(payload.get("admin") or payload.get("adminUser") or payload.get("admin_user") or _runtime_team_admin_user_id()).strip()
    if admin and admin not in invite:
        invite.append(admin)

    creator = _matrix_user_id()
    power_users: dict[str, int] = {}
    if creator:
        power_users[creator] = 100
    if admin:
        power_users[admin] = 100

    body: dict[str, Any] = {
        "name": name,
        "topic": topic,
        "invite": invite,
        "preset": "trusted_private_chat",
    }
    if power_users:
        body["power_level_content_override"] = {"users": power_users}
    binding = _roomflow_project_room_binding(arguments, payload, project_id)
    if binding.get("error"):
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": binding["error"]}
    room_meta = _roomflow_room_meta()

    base: dict[str, Any] = {
        "ok": True,
        "tool": "roomflow",
        "action": "create_task_room",
        "projectId": project_id,
        "taskId": task_id,
        "name": name,
        "source": source,
        "topic": topic,
        "invite": invite,
        "content": body,
    }
    if binding.get("projectRoomKey"):
        base["projectRoomKey"] = binding["projectRoomKey"]
    if binding.get("sourceRoomId"):
        base["sourceRoomId"] = binding["sourceRoomId"]
    if binding.get("sender"):
        base["sender"] = binding["sender"]
    existing_room_id = _bound_room_id(binding)
    if existing_room_id:
        base["roomId"] = existing_room_id
        base["target"] = f"room:{existing_room_id}"
        base["reused"] = True
        if arguments.get("dryRun"):
            base["dryRun"] = True
            return base
        try:
            _ensure_matrix_room_members(existing_room_id, invite)
            _write_matrix_room_meta(existing_room_id, room_meta)
        except ValueError as exc:
            return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": str(exc)}
        except urllib.error.HTTPError as exc:
            error = exc.read().decode("utf-8", errors="replace")[:200]
            return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: {exc}"}
        return base

    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base

    try:
        homeserver, token = _matrix_env("roomflow")
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": str(exc)}
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/createRoom",
        data=json.dumps(body).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as exc:
        error = exc.read().decode("utf-8", errors="replace")[:200]
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: {exc}"}
    room_id = str(data.get("room_id") or "")
    if not room_id:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": "Matrix createRoom response missing room_id", "response": data}
    base["roomId"] = room_id
    base["target"] = f"room:{room_id}"
    try:
        _write_matrix_room_meta(room_id, room_meta)
    except urllib.error.HTTPError as exc:
        error = exc.read().decode("utf-8", errors="replace")[:200]
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: {exc}"}
    _write_roomflow_project_room_binding(binding, room_id, base)
    return base


def _roomflow_project_room_binding(arguments: dict[str, Any], payload: dict[str, Any], project_id: str) -> dict[str, Any]:
    source, source_room_id = _external_source_room_ref(payload)
    sender = _external_sender_ref(payload)
    project_room_key = f"project:{project_id}"
    binding: dict[str, Any] = {
        "projectId": project_id,
        "source": source,
        "sourceRoomId": source_room_id,
        "projectRoomKey": project_room_key,
    }
    if sender:
        binding["sender"] = sender
    workspace_dir = _optional_workspace_dir(arguments)
    if not workspace_dir:
        return binding
    digest = hashlib.sha256(project_room_key.encode("utf-8")).hexdigest()[:16]
    path = workspace_dir / "shared" / "roomflow" / "project-rooms" / f"{project_id}-{digest}.json"
    record = _read_json(path)
    binding["path"] = path
    binding["record"] = record
    return binding


def _external_source_room_ref(payload: dict[str, Any]) -> tuple[str, str]:
    source = str(payload.get("source") or "").strip().lower()
    if not source or source == "matrix":
        return "", ""
    return source, str(payload.get("sourceRoomId") or payload.get("source_room_id") or "").strip()


def _external_sender_ref(payload: dict[str, Any]) -> str:
    for key in (
        "sender",
        "senderId",
        "sender_id",
        "senderUserId",
        "sender_user_id",
        "sourceUserId",
        "source_user_id",
        "targetUser",
        "target_user",
    ):
        value = str(payload.get(key) or "").strip()
        if value:
            return value
    route = payload.get("replyRoute") or payload.get("reply_route")
    if isinstance(route, dict):
        for key in ("targetUser", "target_user", "sender", "senderId", "sender_id"):
            value = str(route.get(key) or "").strip()
            if value:
                return value
    requester_route = _reply_route_from_requester(payload.get("requester"))
    return str(requester_route.get("target_user") or "").strip()


def _bound_room_id(binding: dict[str, Any]) -> str:
    record = binding.get("record")
    if not isinstance(record, dict):
        return ""
    return str(record.get("roomId") or record.get("room_id") or "").strip()


def _write_roomflow_project_room_binding(binding: dict[str, Any], room_id: str, base: dict[str, Any]) -> None:
    path = binding.get("path")
    if not isinstance(path, Path):
        return
    record = dict(binding.get("record") if isinstance(binding.get("record"), dict) else {})
    record.update(
        {
            "projectId": binding.get("projectId"),
            "source": binding.get("source"),
            "sourceRoomId": binding.get("sourceRoomId"),
            "projectRoomKey": binding.get("projectRoomKey"),
            "roomId": room_id,
            "target": f"room:{room_id}",
            "updatedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "taskId": base.get("taskId"),
            "name": base.get("name"),
        }
    )
    if binding.get("sender"):
        record["sender"] = binding.get("sender")
    if "createdAt" not in record:
        record["createdAt"] = record["updatedAt"]
    _write_json(path, record)


def _list_rooms(arguments: dict[str, Any]) -> dict[str, Any]:
    if arguments.get("dryRun"):
        return {"ok": True, "tool": "roomflow", "action": "list_rooms", "dryRun": True}
    try:
        homeserver, token = _matrix_env("roomflow")
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "list_rooms", "error": str(exc)}
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/joined_rooms",
        headers={"Authorization": f"Bearer {token}"},
        method="GET",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as exc:
        error = exc.read().decode("utf-8", errors="replace")[:200]
        return {"ok": False, "tool": "roomflow", "action": "list_rooms", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "roomflow", "action": "list_rooms", "error": f"Matrix API error: {exc}"}
    rooms = data.get("joined_rooms") if isinstance(data.get("joined_rooms"), list) else []
    return {"ok": True, "tool": "roomflow", "action": "list_rooms", "rooms": rooms, "count": len(rooms)}


def _archive_room(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    target = str(payload.get("roomId") or payload.get("room_id") or arguments.get("target") or "").strip()
    try:
        target_kind, room_id = _matrix_target(target)
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "archive_room", "error": str(exc)}
    if target_kind != "room":
        return {"ok": False, "tool": "roomflow", "action": "archive_room", "error": "archive_room requires a Matrix room target"}
    base = {"ok": True, "tool": "roomflow", "action": "archive_room", "roomId": room_id, "target": f"room:{room_id}"}
    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base
    try:
        homeserver, token = _matrix_env("roomflow")
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "archive_room", "error": str(exc)}
    encoded_room = urllib.parse.quote(room_id, safe="")
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/leave",
        data=b"{}",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10):
            pass
    except urllib.error.HTTPError as exc:
        error = exc.read().decode("utf-8", errors="replace")[:200]
        if "M_NOT_FOUND" in error:
            base["note"] = "already left"
            return base
        return {"ok": False, "tool": "roomflow", "action": "archive_room", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "roomflow", "action": "archive_room", "error": f"Matrix API error: {exc}"}
    base["archived"] = True
    return base


def _runtime_team_room_id() -> str:
    team = _section(_load_runtime_config(), "team")
    return str(team.get("teamRoomId") or team.get("team_room_id") or "").strip()


