"""Per-room message history buffer (P9, PR1 M2).

Runtime-neutral port of the QwenPaw channel's group-history buffer
(extracted from ``adapters/qwenpaw/agentteams_matrix/channel.py``):
buffered room messages that didn't mention the bot, capped per room,
plus the per-room generation/record counters and retained event log the
send alignment gate snapshots (#1244).  Behaviour is identical to the
channel implementation (parity tests in
``plugins/tests/agentteams-matrix/test_core_history.py``).

The content-part handling in :meth:`RoomHistory.apply_history_to_parts`
is runtime-neutral: the caller injects an ``is_text_part`` predicate and
a ``make_text_part`` factory for its own content-part type.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Callable, List, Optional, Tuple

__all__ = [
    "CURRENT_MESSAGE_MARKER",
    "DEFAULT_HISTORY_LIMIT",
    "HISTORY_CONTEXT_MARKER",
    "HistoryEntry",
    "RoomHistory",
]

# Group history buffer: how many recent un-mentioned messages are carried
# into the next turn's context when a mention arrives.
DEFAULT_HISTORY_LIMIT = 50

# Markers delimiting the injected history block and the current message
# (consumed by the agent's prompt and by the send-gate nudge).
HISTORY_CONTEXT_MARKER = "[Chat messages since your last reply - for context]"
CURRENT_MESSAGE_MARKER = "[Current message - respond to this]"

IsTextPart = Callable[[Any], bool]
MakeTextPart = Callable[[str], Any]


@dataclass
class HistoryEntry:
    """A buffered room message that didn't mention the bot."""

    sender: str
    body: str
    timestamp: Optional[int] = None
    message_id: Optional[str] = None
    # Optional structured media parts (e.g. downloaded images for vision
    # models) to be included alongside the text history when the mention
    # arrives.
    media_parts: Optional[List[Any]] = None
    # Canonical Matrix user id (full MXID) of the sender. ``sender`` holds a
    # human-readable display name (best-effort, may fall back to the
    # localpart), which is NOT a stable identity: the send alignment gate
    # compares senders against the turn's trigger-sender MXID, so the
    # comparison must use this canonical id, not the display name (#1386 P1).
    sender_id: Optional[str] = None


class RoomHistory:
    """Per-room history buffer: limit, gate counters, and event log."""

    def __init__(self, limit: int = DEFAULT_HISTORY_LIMIT) -> None:
        # Clamp like the channel constructor (``max(0, history_limit)``) —
        # 0 disables the buffer (and the send-gate staleness tracking
        # that rides on the counters below).
        self.limit: int = max(0, limit)
        self._room_histories: dict[str, list[HistoryEntry]] = {}
        # Per-room history generation + cumulative record counters. The
        # send alignment gate (#1244) snapshots the generation into each
        # turn's meta, so a different mention's clear_history can no
        # longer consume the turn's staleness marker (the shared buffer
        # is consumed at the next enqueue).
        self._room_history_gen: dict[str, int] = {}
        self._room_history_records: dict[str, int] = {}
        # Retained per-room event log with stable record indexes: when the
        # send gate retriggers an in-flight turn, the events that made it
        # stale are recovered from here (the shared buffer above is already
        # consumed by the next enqueue and would no longer hold them).
        # Same cap as the buffer — a turn in flight longer than the cap
        # loses the head of its window (tail through the newest event is
        # always retained).
        self._room_event_log: dict[str, list[tuple[int, HistoryEntry]]] = {}

    # -- recording ---------------------------------------------------------

    def record_history(self, room_id: str, entry: HistoryEntry) -> None:
        """Append *entry* to the per-room history buffer (respect limit).

        Also advances the per-room history generation and record counters
        used by the send alignment gate snapshot (#1244): every new room
        event is a potential staleness signal for turns still in flight.
        """
        limit = self.limit
        if limit <= 0:
            return
        history = self._room_histories.setdefault(room_id, [])
        history.append(entry)
        while len(history) > limit:
            history.pop(0)
        self._room_history_gen[room_id] = self._room_history_gen.get(room_id, 0) + 1
        self._room_history_records[room_id] = (
            self._room_history_records.get(room_id, 0) + 1
        )
        log = self._room_event_log.setdefault(room_id, [])
        log.append((self._room_history_records[room_id], entry))
        while len(log) > limit:
            log.pop(0)

    def clear_history(self, room_id: str) -> None:
        """Drop the buffered history for *room_id*."""
        self._room_histories.pop(room_id, None)

    def generation(self, room_id: str) -> int:
        """Current history generation for *room_id* (send-gate snapshot)."""
        return self._room_history_gen.get(room_id, 0)

    def record_count(self, room_id: str) -> int:
        """Cumulative record count for *room_id* (send-gate snapshot)."""
        return self._room_history_records.get(room_id, 0)

    # -- formatting ---------------------------------------------------------

    def build_history_prefix(self, room_id: str) -> str:
        """Format buffered history entries as a multi-line text block."""
        entries = self._room_histories.get(room_id, [])
        if not entries:
            return ""
        lines: list[str] = []
        for e in entries:
            line = f"{e.sender}: {e.body}"
            if e.message_id:
                line += f" [id:{e.message_id}]"
            lines.append(line)
        return "\n".join(lines)

    def build_missed_history(
        self, room_id: str, since_records: int
    ) -> Tuple[str, list]:
        """(text, media) of retained events recorded after *since_records*.

        The exact window that made a turn stale: the room events its
        context snapshot did not cover.  Sourced from the retained
        per-room event log rather than the shared buffer, which the
        mention that made the turn stale has already consumed at its own
        enqueue.  The log is capped like the buffer: a turn in flight
        for longer than ``limit`` room events loses the head of
        its window (the tail through the newest event is always
        retained).  Media parts carried by the entries ride along.
        """
        lines: list[str] = []
        media: list[Any] = []
        for record_idx, e in self._room_event_log.get(room_id, []):
            if record_idx <= since_records:
                continue
            line = f"{e.sender}: {e.body}"
            if e.message_id:
                line += f" [id:{e.message_id}]"
            lines.append(line)
            if e.media_parts:
                media.extend(e.media_parts)
        return "\n".join(lines), media

    def apply_history_to_parts(
        self,
        room_id: str,
        content_parts: list,
        is_text_part: IsTextPart,
        make_text_part: MakeTextPart,
    ) -> list:
        """Prepend accumulated history context to *content_parts*.

        If the first part is a text part (per *is_text_part*), the
        history block is merged into it; otherwise a new text part is
        prepended.  Any media parts stored in history entries (e.g.
        downloaded images) are inserted between the history text block
        and the current message parts so that vision models can see them.

        Returns a (possibly new) list — the original is not mutated.
        """
        if self.limit <= 0:
            return content_parts
        history_text = self.build_history_prefix(room_id)
        if not history_text:
            return content_parts

        # Collect media content parts carried by history entries
        history_media: list[Any] = []
        for entry in self._room_histories.get(room_id, []):
            if entry.media_parts:
                history_media.extend(entry.media_parts)

        # Merge into the leading text part when possible
        first = content_parts[0] if content_parts else None
        if first and is_text_part(first):
            current_text = first.text or ""
            combined = (
                f"{HISTORY_CONTEXT_MARKER}\n{history_text}\n\n"
                f"{CURRENT_MESSAGE_MARKER}\n{current_text}"
            )
            return (
                [make_text_part(combined)]
                + history_media
                + content_parts[1:]
            )
        # No leading text part (e.g. pure media) — prepend a dedicated block
        prefix_part = make_text_part(
            f"{HISTORY_CONTEXT_MARKER}\n{history_text}\n\n"
            f"{CURRENT_MESSAGE_MARKER}"
        )
        return [prefix_part] + history_media + content_parts
