"""Guard low-information replies that can create agent ping-pong loops."""

from __future__ import annotations

import re

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


def extract_matrix_mentions(text: str) -> list[str]:
    """Extract visible Matrix user IDs from message text."""
    return list(dict.fromkeys(_MATRIX_USER_ID_RE.findall(text or "")))


def _low_information_key(text: str) -> str:
    """Normalize short ACK text by dropping punctuation, emoji, and spacing."""
    return "".join(
        re.findall(r"[0-9A-Za-z\u4e00-\u9fff]+", text or ""),
    ).lower()


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
