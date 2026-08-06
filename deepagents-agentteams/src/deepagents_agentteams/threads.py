"""Stable identifiers for Matrix-backed DeepAgents threads."""

from __future__ import annotations

import hashlib

_THREAD_ID_NAMESPACE = b"agentteams-deepagents-v1"


def checkpoint_thread_id(
    *,
    worker_uid: str,
    room_id: str,
    thread_root_event_id: str | None,
) -> str:
    """Return a stable LangGraph checkpoint thread identifier."""
    effective_root = thread_root_event_id or room_id
    digest = hashlib.sha256(
        b"\x00".join(
            (
                _THREAD_ID_NAMESPACE,
                worker_uid.encode(),
                room_id.encode(),
                effective_root.encode(),
            )
        )
    ).hexdigest()
    return f"atd-{digest}"
