"""Pure Matrix policy helpers shared across AgentTeams worker runtimes.

These helpers intentionally avoid importing any Matrix SDK. They model the
policy layer AgentTeams adds on top of native Matrix adapters: outbound mention
enrichment, dual allow-lists, and copaw-style history buffering.
"""
from __future__ import annotations

import os
import re
from dataclasses import dataclass, field
from typing import Any, Dict, List, Mapping, MutableMapping, Optional, Set

DEFAULT_HISTORY_LIMIT = 50
HISTORY_CONTEXT_MARKER = "[Chat messages since your last reply - for context]"
CURRENT_MESSAGE_MARKER = "[Current message - respond to this]"

_MATRIX_USER_ID_RE = re.compile(
    r"@[a-zA-Z0-9._=+/\-]+:[a-zA-Z0-9.\-]+(?::\d+)?",
)


def normalize_user_id(uid: str) -> str:
    """Lowercase MXIDs and ensure a leading ``@`` for set membership."""
    normalized = (uid or "").strip().lower()
    if normalized and not normalized.startswith("@"):
        normalized = "@" + normalized
    return normalized


def _csv_set(value: Optional[str]) -> Set[str]:
    if not value:
        return set()
    return {part.strip() for part in value.split(",") if part.strip()}


def _policy_mode(value: Optional[str], default: str = "allowlist") -> str:
    mode = (value or default).strip().lower()
    if mode in {"open", "allowlist", "disabled"}:
        return mode
    return default


def extract_mentions_from_text(
    text: str,
    self_user_id: str | None = None,
) -> List[str]:
    """Return ordered, de-duplicated MXIDs discovered in ``text``."""
    if not text:
        return []

    seen: Set[str] = set()
    self_uid = (self_user_id or "").strip().lower()
    mentions: List[str] = []
    for mxid in _MATRIX_USER_ID_RE.findall(text):
        key = mxid.lower()
        if key in seen:
            continue
        if self_uid and key == self_uid:
            continue
        seen.add(key)
        mentions.append(mxid)
    return mentions


def _merge_mentions_block(
    message: MutableMapping[str, Any],
    new_user_ids: List[str],
) -> None:
    current = message.get("m.mentions")
    merged: Dict[str, Any] = dict(current) if isinstance(current, Mapping) else {}
    existing_user_ids = merged.get("user_ids")

    seen: Set[str] = set()
    combined: List[str] = []
    for raw in list(existing_user_ids or []) + list(new_user_ids):
        if not isinstance(raw, str) or not raw:
            continue
        key = raw.lower()
        if key in seen:
            continue
        seen.add(key)
        combined.append(raw)

    if combined:
        merged["user_ids"] = combined
    else:
        merged.pop("user_ids", None)

    if merged:
        message["m.mentions"] = merged


def should_suppress_outbound(
    content: Mapping[str, Any],
    *,
    filter_tool: bool = False,
    filter_thinking: bool = False,
) -> bool:
    """Return True if an outbound Matrix event should be dropped (quiet rooms)."""
    if not filter_tool and not filter_thinking:
        return False

    if not isinstance(content, Mapping):
        return False

    relates_to = content.get("m.relates_to")
    if isinstance(relates_to, Mapping) and relates_to.get("rel_type") == "m.replace":
        return False

    event_kind = content.get("hermes.event_kind")
    if event_kind in ("start", "finish", "heartbeat"):
        return False

    if event_kind == "tool" and filter_tool:
        return True
    if event_kind == "thinking" and filter_thinking:
        return True

    return False


def apply_outbound_mentions(
    content: MutableMapping[str, Any],
    self_user_id: str | None = None,
) -> None:
    """Populate ``m.mentions.user_ids`` from body text."""
    mentioned = extract_mentions_from_text(
        content.get("body", "") if isinstance(content.get("body"), str) else "",
        self_user_id=self_user_id,
    )

    new_content = content.get("m.new_content")
    if isinstance(new_content, dict):
        for mxid in extract_mentions_from_text(
            new_content.get("body", "")
            if isinstance(new_content.get("body"), str)
            else "",
            self_user_id=self_user_id,
        ):
            if mxid.lower() not in {item.lower() for item in mentioned}:
                mentioned.append(mxid)

    _merge_mentions_block(content, mentioned)
    if isinstance(new_content, dict):
        _merge_mentions_block(new_content, mentioned)


@dataclass(frozen=True)
class DualAllowList:
    """Allow-list policy split by DM and group contexts."""

    dm_policy: str = "allowlist"
    group_policy: str = "allowlist"
    dm_allow: frozenset[str] = field(default_factory=frozenset)
    group_allow: frozenset[str] = field(default_factory=frozenset)

    @classmethod
    def from_env(cls) -> "DualAllowList":
        return cls(
            dm_policy=_policy_mode(os.getenv("MATRIX_DM_POLICY"), "allowlist"),
            group_policy=_policy_mode(
                os.getenv("MATRIX_GROUP_POLICY"),
                "allowlist",
            ),
            dm_allow=frozenset(
                normalize_user_id(value)
                for value in _csv_set(os.getenv("MATRIX_ALLOWED_USERS"))
            ),
            group_allow=frozenset(
                normalize_user_id(value)
                for value in _csv_set(os.getenv("MATRIX_GROUP_ALLOW_FROM"))
            ),
        )

    @property
    def group_combined_allow(self) -> frozenset[str]:
        return self.dm_allow | self.group_allow

    def permits(self, sender: str, is_dm: bool) -> bool:
        normalized = normalize_user_id(sender)
        if not normalized:
            return False

        if is_dm:
            if self.dm_policy == "disabled":
                return False
            if self.dm_policy == "open":
                return True
            return normalized in self.dm_allow

        if self.group_policy == "disabled":
            return False
        if self.group_policy == "open":
            return True
        return normalized in self.group_combined_allow


@dataclass
class _HistoryEntry:
    sender: str
    body: str


@dataclass
class HistoryBuffer:
    """Per-room copaw-style history buffer for unmentioned group chatter."""

    limit: int = DEFAULT_HISTORY_LIMIT
    _entries: Dict[str, List[_HistoryEntry]] = field(default_factory=dict)

    @classmethod
    def from_env(cls) -> "HistoryBuffer":
        raw = os.getenv("MATRIX_HISTORY_LIMIT", "")
        try:
            limit = int(raw) if raw else DEFAULT_HISTORY_LIMIT
        except ValueError:
            limit = DEFAULT_HISTORY_LIMIT
        return cls(limit=max(limit, 0))

    def record(self, room_id: str, sender: str, body: str) -> None:
        if self.limit <= 0 or not room_id:
            return
        text = (body or "").strip()
        if not text:
            return
        bucket = self._entries.setdefault(room_id, [])
        bucket.append(_HistoryEntry(sender=sender or "unknown", body=text))
        while len(bucket) > self.limit:
            bucket.pop(0)

    def drain(self, room_id: str) -> str:
        entries = self._entries.pop(room_id, None)
        if not entries:
            return ""

        lines = [f"{entry.sender}: {entry.body}" for entry in entries]
        return (
            f"{HISTORY_CONTEXT_MARKER}\n"
            f"{chr(10).join(lines)}\n\n"
            f"{CURRENT_MESSAGE_MARKER}\n"
        )

    def clear(self, room_id: str) -> None:
        self._entries.pop(room_id, None)
