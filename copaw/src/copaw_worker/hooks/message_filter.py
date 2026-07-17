"""Outgoing Matrix message filtering for AgentTeams CoPaw agents."""

from __future__ import annotations

from dataclasses import dataclass
import os
from pathlib import Path
import re

from matrix.outbound_policy import (
    is_team_leader_internal_preamble_text,
    runtime_config_field,
)

NO_REPLY_TOKEN = "NO_REPLY"

_MATRIX_USER_ID_RE = re.compile(
    r"@[a-zA-Z0-9._=+/\-]+:[a-zA-Z0-9.\-]+(?::\d+)?",
)

_LOW_INFORMATION_ACKS = {
    "ack",
    "acknowledged",
    "done",
    "ok",
    "okay",
    "received",
    "收到",
    "好的",
    "好",
    "完成",
    "已完成",
}


@dataclass(frozen=True)
class MessageFilterResult:
    """Result of applying outgoing message filters."""

    text: str
    suppress_reason: str | None = None
    rewritten: bool = False
    rewrite_reason: str | None = None

    @property
    def suppressed(self) -> bool:
        return self.suppress_reason is not None


def extract_matrix_mentions(text: str) -> list[str]:
    """Extract visible Matrix user IDs from message text."""
    return list(dict.fromkeys(_MATRIX_USER_ID_RE.findall(text or "")))


def _low_information_key(text: str) -> str:
    """Normalize short ACK text by dropping punctuation, emoji, and spacing."""
    return "".join(
        re.findall(r"[0-9A-Za-z\u4e00-\u9fff]+", text or ""),
    ).lower()


def strip_no_reply_contamination(text: str) -> str:
    """Remove leaked NO_REPLY protocol tokens from otherwise substantive text."""
    if not text or NO_REPLY_TOKEN not in text:
        return text or ""

    # The token can be emitted adjacent to mentions, for example:
    # "@leadNO_REPLY@lead:domain ...". Remove it even when not word-delimited.
    cleaned = text.replace(NO_REPLY_TOKEN, "")
    cleaned = re.sub(
        r"@[a-zA-Z0-9._=+/\-]+(?=@[a-zA-Z0-9._=+/\-]+:[a-zA-Z0-9.\-]+(?::\d+)?)",
        "",
        cleaned,
    )
    cleaned = re.sub(r"[ \t]{2,}", " ", cleaned)
    cleaned = re.sub(r" *\n *", "\n", cleaned)
    return cleaned.strip()


def _runtime_config_field(section: str, key: str) -> str:
    return runtime_config_field(section, key)


def get_team_leader_dm_internal_preamble_reason(
    text: str,
    *,
    room_id: str | None = None,
) -> str | None:
    """Return a suppress reason for visible Team Leader internal DM preambles."""
    if not room_id:
        return None
    if _runtime_config_field("member", "role") != "team_leader":
        return None

    leader_dm_room_id = _runtime_config_field("team", "leaderDmRoomId")
    if not leader_dm_room_id or room_id != leader_dm_room_id:
        return None

    if not is_team_leader_internal_preamble_text(text):
        return None
    return "message suppressed: Team Leader internal preamble in Leader DM"


def get_pingpong_block_reason(
    text: str,
    mentions: list[str] | None = None,
    *,
    fallback_user_id: str | None = None,
) -> str | None:
    """Return a block reason when a reply would only wake another agent."""
    visible_mentions = mentions
    if visible_mentions is None:
        visible_mentions = extract_matrix_mentions(text)

    mention_targets = [m for m in visible_mentions if m]
    if fallback_user_id:
        mention_targets.append(fallback_user_id)

    if not mention_targets:
        return None

    without_mentions = _MATRIX_USER_ID_RE.sub("", text or "").strip()
    without_mentions = strip_no_reply_contamination(without_mentions)
    compact = re.sub(r"\s+", " ", without_mentions).strip()
    if not compact:
        return "message blocked: mention-only messages can create ping-pong loops"

    compact_key = _low_information_key(compact)
    if compact_key in _LOW_INFORMATION_ACKS:
        return (
            "message blocked: low-information mention acknowledgements can "
            "create ping-pong loops"
        )

    if len(compact) <= 8 and not re.search(r"[\w\u4e00-\u9fff]", compact):
        return "message blocked: mention plus status symbol can create ping-pong loops"

    return None


def filter_outgoing_matrix_message(
    text: str,
    mentions: list[str] | None = None,
    *,
    fallback_user_id: str | None = None,
    room_id: str | None = None,
) -> MessageFilterResult:
    """Apply all outgoing Matrix message filters in a stable order."""
    raw_text = text or ""
    stripped = raw_text.strip()

    if stripped == NO_REPLY_TOKEN:
        return MessageFilterResult(
            text=NO_REPLY_TOKEN,
            suppress_reason="message suppressed: NO_REPLY",
        )

    cleaned_text = strip_no_reply_contamination(raw_text)
    rewritten = cleaned_text != raw_text
    reason = get_team_leader_dm_internal_preamble_reason(
        cleaned_text,
        room_id=room_id,
    )
    if reason:
        return MessageFilterResult(
            text=cleaned_text,
            suppress_reason=reason,
            rewritten=rewritten,
            rewrite_reason=(
                "removed embedded NO_REPLY protocol token" if rewritten else None
            ),
        )

    reason = get_pingpong_block_reason(
        cleaned_text,
        mentions,
        fallback_user_id=fallback_user_id,
    )
    if reason:
        return MessageFilterResult(
            text=cleaned_text,
            suppress_reason=reason,
            rewritten=rewritten,
            rewrite_reason=(
                "removed embedded NO_REPLY protocol token" if rewritten else None
            ),
        )

    return MessageFilterResult(
        text=cleaned_text,
        rewritten=rewritten,
        rewrite_reason="removed embedded NO_REPLY protocol token" if rewritten else None,
    )
