"""Mention parsing, prefix stripping, and worker text matching (P9, PR1 M2).

Runtime-neutral port of the QwenPaw channel's mention logic (extracted
from ``adapters/qwenpaw/agentteams_matrix/channel.py``).  Behaviour is
identical to the channel implementation, including the edge cases: empty
``m.mentions`` / missing ``user_ids``, duplicate mentions, case-
insensitivity, punctuation and multi-space separators after the mention
prefix, and the localpart token boundary rules.

The channel switches to these helpers in M3; until then the parity tests
(``plugins/tests/agentteams-matrix/test_core_mentions.py``) assert that
core and channel return identical results on the same inputs.
"""

from __future__ import annotations

import re
import urllib.parse
from typing import Any

__all__ = [
    "extract_mentions_from_text",
    "strip_mention_prefix",
    "text_mentions_worker",
    "was_mentioned",
]

# Regex to match Matrix user IDs: @localpart:domain (with optional port)
_MATRIX_USER_ID_RE = re.compile(
    r"@[a-zA-Z0-9._=+/\-]+:[a-zA-Z0-9.\-]+(?::\d+)?",
)


def was_mentioned(user_id: str, event: Any, text: str) -> bool:
    """Whether *event* mentions *user_id* (structured or textual form).

    *event* is any object exposing a ``source`` mapping with the raw
    Matrix event content (nio events satisfy this; a plain
    ``{"content": {...}}`` holder works too).
    """
    if not user_id:
        return False
    # 1. Check m.mentions (structured mention from Matrix spec)
    content = event.source.get("content", {})
    mentions = content.get("m.mentions", {})
    if user_id in mentions.get("user_ids", []):
        return True
    if mentions.get("room"):
        return True
    # 2. formatted_body: matrix.to mention links (Element HTML format)
    formatted_body = content.get("formatted_body", "")
    if formatted_body and user_id:
        escaped_uid = re.escape(user_id)
        if re.search(
            rf'href=["\']https://matrix\.to/#/{escaped_uid}["\']',
            formatted_body,
            re.IGNORECASE,
        ):
            return True
        encoded_uid = re.escape(urllib.parse.quote(user_id))
        if re.search(
            rf'href=["\']https://matrix\.to/#/{encoded_uid}["\']',
            formatted_body,
            re.IGNORECASE,
        ):
            return True
    # 3. Fallback: match full MXID in plain text
    if user_id and re.search(
        re.escape(user_id),
        text,
        re.IGNORECASE,
    ):
        return True
    # 4. matrix.to localpart-only link (domain omitted by some clients)
    localpart = user_id.lstrip("@").split(":", 1)[0]
    if localpart and formatted_body and re.search(
        rf'href=["\']https://matrix\.to/#/{re.escape("@" + localpart)}["\']',
        formatted_body,
        re.IGNORECASE,
    ):
        return True
    # 5. bare @localpart in plain text.
    #    A Matrix localpart may contain [A-Za-z0-9._=/+-] (spec user
    #    identifiers), so the mention token in text extends over those
    #    characters. `@alice-dev`, `@alice+dev`, `@alice/dev` and
    #    `@alice2` are OTHER users (longer localparts) and
    #    `@alice:other.test` is a full MXID on ANOTHER domain — none
    #    of them is our user in bare form. Require the token to end
    #    exactly where our localpart ends: the next character must
    #    not be a localpart character or ':' (which would start an
    #    MXID domain we do not own).
    if localpart:
        token = re.compile(
            rf"(?<![\w@])@{re.escape(localpart)}(?![A-Za-z0-9._=:/+-])"
        )
        if token.search(text):
            return True
    return False


def strip_mention_prefix(
    user_id: str, text: str, display_name: str | None = None
) -> str:
    """Strip leading @mention prefix so slash commands can be detected.

    Handles MXID format (@user:server), room display name, and localpart.
    E.g. ``"@worker:hs.example /new"`` → ``"/new"``
         ``"math 💕: /clear"`` → ``"/clear"``.

    *display_name* is the resolved display name of *user_id* in the room
    (the channel resolves it via the nio room/client; it may differ from
    the localpart, e.g. ``"math 💕"`` for localpart ``"math"``).  Pass
    ``None`` when there is no room context.
    """
    if not user_id:
        return text
    # 1. Strip MXID (@user:server) at start
    escaped = re.escape(user_id)
    result = re.sub(rf"^{escaped}\s*:?\s*", "", text, flags=re.IGNORECASE)
    if result != text:
        return result.strip()
    # 2. Strip room display name (e.g. "math 💕") at start — try before
    #    localpart so that "math 💕: /clear" is not partially matched by
    #    the shorter localpart "math".
    if display_name and display_name != user_id:
        result = re.sub(
            rf"^{re.escape(display_name)}\s*:?\s*",
            "",
            text,
            flags=re.IGNORECASE,
        )
        if result != text:
            # Clean leftover decoration (e.g. emoji suffix) between
            # the display name and the actual message content.
            result = re.sub(r"^[^\w/]+", "", result)
            return result.strip()
    # 3. Strip localpart (e.g. "math") at start — only if display name
    #    didn't match.
    localpart = user_id.split(":")[0].lstrip("@")
    if localpart:
        result = re.sub(
            rf"^@?{re.escape(localpart)}\s*:?\s*",
            "",
            text,
            flags=re.IGNORECASE,
        )
        if result != text:
            # After stripping localpart, there may be leftover decoration
            # from the display name (e.g. emoji suffix "💕: " from
            # "math 💕: /clear").  Strip non-alphanumeric prefix so the
            # slash command is exposed.
            result = re.sub(r"^[^\w/]+", "", result)
            return result.strip()
    return text


def extract_mentions_from_text(text: str) -> list[str]:
    """Extract all @user:domain Matrix IDs from message text."""
    matches = _MATRIX_USER_ID_RE.findall(text)
    return list(dict.fromkeys(matches))  # dedupe, preserve order


def text_mentions_worker(
    user_id: str | None, text: str, worker_localpart: str
) -> bool:
    """Text-only mention check for gate relevance (#1386 P1).

    Mirrors the plain-text arms of :func:`was_mentioned` (full MXID +
    bare ``@localpart`` with localpart boundary rules); retained history
    entries carry plain body text only, no event content.
    """
    if not text or not worker_localpart:
        return False
    if user_id and re.search(
        re.escape(user_id), text, re.IGNORECASE
    ):
        return True
    token = re.compile(
        rf"(?<![\w@])@{re.escape(worker_localpart)}(?![A-Za-z0-9._=:/+-])"
    )
    return bool(token.search(text))
