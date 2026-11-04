# runtime config loading and storage-root defaults (W1-a2: real module)
from __future__ import annotations

import json
import os
import re
from pathlib import Path
import urllib.error
from typing import Any


def _load_runtime_config() -> dict[str, Any]:
    runtime_config = os.getenv("TEAMHARNESS_RUNTIME_CONFIG", "").strip()
    if not runtime_config:
        return {}
    path = Path(runtime_config)
    if not path.exists():
        return {}
    text = path.read_text(encoding="utf-8")
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        data = None
    if isinstance(data, dict):
        return data
    try:
        import yaml

        data = yaml.safe_load(text) or {}
    except Exception:
        data = _simple_yaml_sections(text)
    return data if isinstance(data, dict) else {}


def _simple_yaml_sections(text: str) -> dict[str, Any]:
    data: dict[str, Any] = {}
    section: str | None = None
    nested_section: str | None = None
    for line in text.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        top = re.match(r"^([A-Za-z0-9_]+):\s*(.*)$", line)
        if top:
            key, value = top.group(1), top.group(2).strip()
            if value:
                data[key] = _yaml_scalar(value)
                section = None
                nested_section = None
            else:
                data[key] = {}
                section = key
                nested_section = None
            continue
        nested = re.match(r"^\s{2}([A-Za-z0-9_]+):\s*(.*)$", line)
        if nested and section and isinstance(data.get(section), dict):
            key, value = nested.group(1), nested.group(2).strip()
            if value:
                data[section][key] = _yaml_scalar(value)
                nested_section = None
            else:
                data[section][key] = {}
                nested_section = key
            continue
        deep = re.match(r"^\s{4}([A-Za-z0-9_]+):\s*(.*)$", line)
        if deep and section and nested_section and isinstance(data.get(section), dict):
            parent = data[section].get(nested_section)
            if isinstance(parent, dict):
                parent[deep.group(1)] = _yaml_scalar(deep.group(2).strip())
    return data


def _yaml_scalar(value: str) -> Any:
    if value in {"", "null", "Null", "NULL", "~"}:
        return ""
    if value in {"true", "True", "TRUE"}:
        return True
    if value in {"false", "False", "FALSE"}:
        return False
    if (value.startswith("'") and value.endswith("'")) or (value.startswith('"') and value.endswith('"')):
        return value[1:-1]
    return value


def _section(data: dict[str, Any], name: str) -> dict[str, Any]:
    value = data.get(name)
    return value if isinstance(value, dict) else {}


def _protocol_enforce_enabled() -> bool:
    """W2 protocol guard switch (issue #1451 W2).

    The ``protocol.enforce`` key of the TEAMHARNESS_RUNTIME_CONFIG (JSON or
    YAML) decides; the TEAMHARNESS_PROTOCOL_ENFORCE environment variable
    overrides when set to a non-empty value. Default off: with the switch
    off every W2 guard is skipped and the behaviour is byte-for-byte the
    pre-W2 behaviour.
    """
    override = os.getenv("TEAMHARNESS_PROTOCOL_ENFORCE", "").strip().lower()
    if override:
        return override in {"1", "true", "yes", "on"}
    return bool(_section(_load_runtime_config(), "protocol").get("enforce"))


def _runtime_team_name() -> str:
    """Return the runtime team name from TEAMHARNESS_RUNTIME_CONFIG, if any.

    Used to stamp ``team_id`` onto project metadata so the Controller can map
    a project back to its owning team (``teams/{team}/shared/projects/`` vs the
    global ``shared/projects/`` prefix). Empty string when no team is configured.
    """
    team = _section(_load_runtime_config(), "team")
    return str(team.get("name") or "").strip()


def _runtime_team_admin_user_id() -> str:
    config = _load_runtime_config()
    team = _section(config, "team")
    admin = _section(team, "admin")
    matrix_user_id = str(admin.get("matrixUserId") or admin.get("matrix_user_id") or "").strip()
    if matrix_user_id:
        return matrix_user_id
    return _runtime_leader_dm_admin_user_id(config)


def _runtime_leader_dm_admin_user_id(config: dict[str, Any]) -> str:
    from ..room.matrix import _matrix_room_member_user_ids, _matrix_user_id

    team = _section(config, "team")
    room_id = str(team.get("leaderDmRoomId") or team.get("leader_dm_room_id") or "").strip()
    if not room_id:
        return ""
    leader_id = str(_section(config, "member").get("matrixUserId") or _matrix_user_id()).strip()
    try:
        members = _matrix_room_member_user_ids(room_id)
    except (ValueError, urllib.error.HTTPError, urllib.error.URLError, TimeoutError, OSError, json.JSONDecodeError):
        return ""
    for user_id in members:
        if user_id and user_id != leader_id:
            return user_id
    return ""


