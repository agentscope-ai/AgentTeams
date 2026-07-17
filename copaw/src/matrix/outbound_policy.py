"""Outbound Matrix send filtering for AgentTeams roles.

Extracted from the channel transport so Manager, Worker, and hook layers can
share the same Team Leader DM / NO_REPLY / assignment-reroute semantics without
importing ``copaw_worker``.
"""
from __future__ import annotations

import os
import re

from .paths import runtime_root

NO_REPLY_TOKEN = "NO_REPLY"

_MATRIX_USER_ID_RE = re.compile(
    r"@[a-zA-Z0-9._=+/\-]+:[a-zA-Z0-9.\-]+(?::\d+)?",
)
_TEAM_LEADER_DM_INTERNAL_PREAMBLE_RE = re.compile(
    r"(?i)\b("
    r"let me|"
    r"i['’]?ll coordinate|"
    r"i will coordinate|"
    r"i have (?:\d+|one|two|three|four|five|six|seven|eight|nine|ten) "
    r"workers? available|"
    r"now let me|"
    r"no active projects|"
    r"project created\. now|"
    r"good[,.]? i have|"
    r"solid understanding|"
    r"team coordination plan"
    r")\b",
)
_TEAM_LEADER_WORKER_ASSIGNMENT_RE = re.compile(
    r"(?i)\b("
    r"task\s+assigned|"
    r"assigned\s+task|"
    r"you\s+are\s+assigned|"
    r"please\s+(?:design|implement|write|test|build|handle|review|create|investigate|work)|"
    r"start\s+(?:by\s+)?(?:designing|implementing|writing|testing|building|handling|reviewing|creating|investigating)"
    r")\b",
)


def _strip_yaml_string(value: str) -> str:
    text = value.strip()
    if not text or text in {"null", "~"}:
        return ""
    if "#" in text:
        text = text.split("#", 1)[0].strip()
    if len(text) >= 2 and text[0] == text[-1] and text[0] in {"'", '"'}:
        return text[1:-1]
    return text


def runtime_config_field(section: str, key: str) -> str:
    """Read a scalar field from ``runtime/runtime.yaml`` under *section*."""
    path = runtime_root() / "runtime" / "runtime.yaml"
    if not path.exists():
        return ""

    in_section = False
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return ""
    for raw_line in lines:
        if not raw_line.strip() or raw_line.lstrip().startswith("#"):
            continue
        if not raw_line.startswith((" ", "\t")):
            in_section = raw_line.strip() == f"{section}:"
            continue
        if not in_section:
            continue
        stripped = raw_line.strip()
        if ":" not in stripped:
            continue
        field, value = stripped.split(":", 1)
        if field.strip() == key:
            return _strip_yaml_string(value)
    return ""


def extract_matrix_user_ids(text: str) -> list[str]:
    """Return de-duplicated Matrix user IDs found in *text*."""
    seen: set[str] = set()
    result: list[str] = []
    for match in _MATRIX_USER_ID_RE.finditer(text or ""):
        mxid = match.group(0)
        key = mxid.lower()
        if key in seen:
            continue
        seen.add(key)
        result.append(mxid)
    return result


def _matrix_localpart(user_id: str | None) -> str:
    text = (user_id or os.getenv("AGENTTEAMS_WORKER_NAME") or "").strip()
    if text.startswith("@"):
        return text[1:].split(":", 1)[0]
    return text.split(":", 1)[0]


def is_team_leader_identity(user_id: str | None) -> bool:
    localpart = _matrix_localpart(user_id)
    return localpart.endswith("-lead") or localpart.endswith("-leader")


def ends_with_no_reply_control(text: str) -> bool:
    """Return true when the final non-empty output line is NO_REPLY."""
    return bool(text) and text.rstrip().splitlines()[-1].strip() == NO_REPLY_TOKEN


def is_team_leader_internal_preamble_text(text: str) -> bool:
    """Return true for visible Team Leader internal planning/tool preambles."""
    stripped = (text or "").strip()
    if not stripped or "?" in stripped:
        return False

    # Keep concrete worker assignments so the channel can reroute them to the
    # Team Room. Roster/topology planning that happens to mention workers is
    # still an internal preamble and must stay out of Leader DM.
    if extract_matrix_user_ids(text) and (
        _TEAM_LEADER_WORKER_ASSIGNMENT_RE.search(stripped)
    ):
        return False

    return bool(_TEAM_LEADER_DM_INTERNAL_PREAMBLE_RE.search(stripped))


def is_team_leader_dm_internal_preamble(current_room_id: str, text: str) -> bool:
    """Suppress visible Team Leader internal planning/tool preambles in Leader DM."""
    if runtime_config_field("member", "role") != "team_leader":
        return False

    leader_dm_room_id = runtime_config_field("team", "leaderDmRoomId")
    if not leader_dm_room_id or current_room_id != leader_dm_room_id:
        return False

    return is_team_leader_internal_preamble_text(text)


class OutboundFilterPolicy:
    """Role-aware outbound send filtering configured from runtime.yaml."""

    def __init__(self, user_id: str | None = None) -> None:
        self._user_id = user_id

    @property
    def user_id(self) -> str | None:
        return self._user_id

    @user_id.setter
    def user_id(self, value: str | None) -> None:
        self._user_id = value

    def should_suppress_no_reply(self, text: str) -> bool:
        return ends_with_no_reply_control(text)

    def should_suppress_team_leader_preamble(
        self,
        room_id: str,
        text: str,
    ) -> bool:
        if is_team_leader_dm_internal_preamble(room_id, text):
            return True
        if not is_team_leader_identity(self._user_id):
            return False
        return is_team_leader_internal_preamble_text(text)

    def resolve_destination_room(self, current_room_id: str, text: str) -> str:
        """Reroute Team Leader worker assignments from Leader DM to Team Room."""
        if runtime_config_field("member", "role") != "team_leader":
            return current_room_id

        team_room_id = runtime_config_field("team", "teamRoomId")
        leader_dm_room_id = runtime_config_field("team", "leaderDmRoomId")
        team_name = runtime_config_field("team", "name")
        if not team_room_id or not leader_dm_room_id:
            return current_room_id
        if current_room_id != leader_dm_room_id:
            return current_room_id

        for mxid in extract_matrix_user_ids(text):
            localpart = mxid.removeprefix("@").split(":", 1)[0]
            if localpart.endswith("-lead"):
                continue
            if not team_name or localpart.startswith(f"{team_name}-"):
                return team_room_id
        return current_room_id
