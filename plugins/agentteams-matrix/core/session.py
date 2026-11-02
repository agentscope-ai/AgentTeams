"""Session-id derivation and handle conversion (P9, PR1 M2).

Runtime-neutral port of the QwenPaw channel's session keying (extracted
from ``adapters/qwenpaw/agentteams_matrix/channel.py``).  Matrix keys
sessions on the room: DMs, group rooms and threads all resolve to
``matrix:<room_id>`` (a thread stays in its parent room's session; the
thread root travels separately in the channel meta, not in the session
key).  The channel switches to these helpers in M3; until then the
parity tests (``plugins/tests/agentteams-matrix/test_core_session.py``)
assert identical results on the same inputs.
"""

from __future__ import annotations

from typing import Any, Mapping, Optional

__all__ = [
    "resolve_session_id",
    "to_handle_from_target",
]


def resolve_session_id(
    sender_id: str, channel_meta: Optional[Mapping[str, Any]] = None
) -> str:
    """Derive the session key for an inbound Matrix message.

    DMs, group rooms and thread replies all key on ``meta["room_id"]``;
    when the meta carries no room id, *sender_id* is the fallback key
    (upstream BaseChannel contract).
    """
    room_id = (channel_meta or {}).get("room_id", sender_id)
    return f"matrix:{room_id}"


def to_handle_from_target(*, user_id: str, session_id: str) -> str:
    """For Matrix, return room_id (session_id), not user_id.

    Matrix requires room_id to send messages, not user_id.  The
    session_id carries a ``matrix:`` prefix added by
    :func:`resolve_session_id`; strip it so the value is a raw Matrix
    room_id that can be passed directly to ``room_send``.
    """
    if session_id.startswith("matrix:"):
        return session_id[len("matrix:") :]
    return session_id
