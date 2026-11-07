"""OutboundQueue: the persistent outbound delivery queue (P9, MX-1).

Store-before-send: every outbound message is persisted *before* it is
handed to the transport, so a crash can never lose an in-flight message
and a restart replays it exactly once (Matrix deduplicates sends per
``tx_id``, and the worker derives the ``tx_id`` deterministically from
the row identity).

Row model::

    (kind, key, payload, seq, generation, attempts, due)   PK=(kind, key)

Rules
-----
- **Store before send**: :meth:`OutboundQueue.enqueue` persists the row
  before any delivery attempt.
- **Same-key re-entry** replaces the payload, bumps ``generation``,
  resets ``attempts`` and keeps the queue position (``seq``).
- **Later delay** (a future ``due``) never counts as an attempt; a row
  is claimable only once ``due`` has passed.
- **Completion** matches the ``(kind, key, seq, generation)`` quadruple;
  a stale completion (superseded generation or seq) never removes a row.
- **Per-kind concurrency cap** + exponential backoff: default schedule
  ``[2s, 60s]``, long-running task kinds ``[5s, 300s]`` —
  ``delay = min(cap, initial * 2 ** (attempts - 1))``.
- **Dead letter**: ``DEAD_LETTER_ATTEMPTS`` (3) consecutive failed
  attempts make the row terminal; the core reports the health error.
- **Dropped / gone** rows are permanent archive records and are never
  re-queued.
- **Ordering**: at most one in-flight delivery per room — a second row
  for the same room waits until the first one completes or fails.

Vocabulary (shared with the TeamHarness ``notification_log``, TH-2 /
MX-4): ``sent`` / ``delivered`` (room_accepted) / ``accepted``
(channel_accepted) / ``dropped{reason}`` / ``gone``.

Persistence is atomic JSON (``json`` + ``fsync`` + ``os.replace`` via
:func:`core.state._atomic_write`, the same primitive the rest of the
state layer uses): the single-writer lock already guarantees one core
process per agent, and an agent's outbox holds dozens to hundreds of
rows, so a full-file atomic rewrite is the right trade — no sqlite
dependency, no partial writes.  All timestamps are unix seconds.
"""

from __future__ import annotations

import hashlib
import json
import os
import time
from collections import Counter
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Union

from .interfaces import MediaRef, OutboundMessage
from .state import _atomic_write

#: Delivery vocabulary shared with the TeamHarness notification_log.
OUTBOX_STATUSES = ("sent", "delivered", "accepted", "dropped", "gone")

#: Row lifecycle statuses.  ``pending`` / ``in_flight`` are live states;
#: ``dead_letter`` / ``dropped`` / ``gone`` are terminal (archived).
ROW_STATUSES = ("pending", "in_flight", "dead_letter", "dropped", "gone")

#: Three consecutive failed send attempts -> dead letter (terminal).
DEAD_LETTER_ATTEMPTS = 3

#: Default retry schedule (seconds): [2s, 60s].
DEFAULT_BACKOFF_INITIAL = 2.0
DEFAULT_BACKOFF_MAX = 60.0

#: Long-running task kinds retry schedule (seconds): [5s, 300s].
LONG_BACKOFF_INITIAL = 5.0
LONG_BACKOFF_MAX = 300.0

#: Default per-kind in-flight cap.
DEFAULT_CONCURRENCY = 4

_FILE_NAME = "queue.json"
_FORMAT_VERSION = 1


@dataclass
class OutboxRow:
    """One queue row (PK ``kind``+``key``).

    ``seq`` fixes the queue position (assigned at first enqueue, kept on
    same-key re-entry); ``generation`` counts payload replacements
    (bumped on re-entry, which resets ``attempts``); ``attempts`` counts
    failed *send* attempts only — waiting on a future ``due`` never
    counts; ``due`` is the unix timestamp before which the row must not
    be claimed.
    """

    kind: str
    key: str
    payload: Dict[str, Any]
    seq: int
    generation: int
    attempts: int
    due: float
    status: str = "pending"

    @property
    def room_id(self) -> Optional[str]:
        return self.payload.get("room_id")

    def as_message(self) -> OutboundMessage:
        return payload_to_message(self.payload)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "kind": self.kind,
            "key": self.key,
            "payload": self.payload,
            "seq": self.seq,
            "generation": self.generation,
            "attempts": self.attempts,
            "due": self.due,
            "status": self.status,
        }

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "OutboxRow":
        return cls(
            kind=data["kind"],
            key=data["key"],
            payload=data["payload"],
            seq=data["seq"],
            generation=data["generation"],
            attempts=data["attempts"],
            due=data["due"],
            status=data.get("status", "pending"),
        )


def message_to_payload(msg: OutboundMessage) -> Dict[str, Any]:
    """Serialise an :class:`~core.interfaces.OutboundMessage` for a row."""
    return {
        "room_id": msg.room_id,
        "text": msg.text,
        "reply_to": msg.reply_to,
        "kind": msg.kind,
        "media": [m.to_dict() for m in msg.media],
        "tx_id": msg.tx_id,
    }


def payload_to_message(payload: Dict[str, Any]) -> OutboundMessage:
    """Parse a row payload back into an :class:`OutboundMessage`.

    Round-trips exactly with :func:`message_to_payload` (media stays a
    tuple, matching the dataclass default).
    """
    return OutboundMessage(
        room_id=payload["room_id"],
        text=payload["text"],
        reply_to=payload.get("reply_to"),
        kind=payload.get("kind", "final"),
        media=tuple(MediaRef.from_dict(m) for m in payload.get("media", ())),
        tx_id=payload.get("tx_id"),
    )


def outbox_marker(kind: str, key: str, seq: int, generation: int) -> str:
    """Idempotency marker embedded at the end of the message body."""
    return f"agentteams-outbox:{kind}:{key}:{seq}:{generation}"


def with_marker(text: str, marker: str) -> str:
    """Append the idempotency marker as a trailing HTML comment."""
    if not text:
        return f"<!-- {marker} -->"
    if text.endswith("\n"):
        return f"{text}\n<!-- {marker} -->"
    return f"{text} <!-- {marker} -->"


def deterministic_tx_id(kind: str, key: str, seq: int, generation: int) -> str:
    """Deterministic Matrix ``tx_id`` for one row identity.

    Matrix deduplicates sends per (room, tx_id), so replaying the same
    row after a restart reuses the same tx_id and produces no second
    event.
    """
    marker = outbox_marker(kind, key, seq, generation)
    return hashlib.sha256(marker.encode("utf-8")).hexdigest()[:32]


class OutboundQueue:
    """Persistent outbound queue (store-before-send, idempotent replay).

    The queue file lives at ``<state_dir>/outbox/queue.json`` (0600).
    Rows are delivered by the core's worker: :meth:`claim_due` marks the
    claimable rows ``in_flight`` (persisted), the worker sends each one,
    then :meth:`complete` (success) or :meth:`fail` (error: backoff or
    dead letter) settles it on the quadruple.  Terminal rows move to the
    append-only ``archive`` (permanent records); completed rows are
    removed.  Delivery receipts (the shared vocabulary) are append-only
    too and stay attributable after a row is removed.

    Concurrency note: the single-writer lock already guarantees one core
    process per agent, and the worker runs on the core's single asyncio
    loop, so no internal locking is needed.
    """

    def __init__(
        self,
        path: Union[str, Path],
        *,
        concurrency: int = DEFAULT_CONCURRENCY,
        backoff_initial: float = DEFAULT_BACKOFF_INITIAL,
        backoff_max: float = DEFAULT_BACKOFF_MAX,
        long_running_kinds: Iterable[str] = (),
        backoff_long_initial: float = LONG_BACKOFF_INITIAL,
        backoff_long_max: float = LONG_BACKOFF_MAX,
    ) -> None:
        self._dir = Path(path)
        self._file = self._dir / _FILE_NAME
        self._concurrency = int(concurrency)
        self._initial = float(backoff_initial)
        self._max = float(backoff_max)
        self._long_kinds = frozenset(long_running_kinds)
        self._long_initial = float(backoff_long_initial)
        self._long_max = float(backoff_long_max)
        if self._concurrency < 1:
            raise ValueError("OutboundQueue concurrency must be >= 1")
        if self._initial <= 0:
            raise ValueError("backoff_initial must be > 0")
        if self._max < self._initial:
            raise ValueError("backoff_max must be >= backoff_initial")
        if self._long_initial <= 0:
            raise ValueError("backoff_long_initial must be > 0")
        if self._long_max < self._long_initial:
            raise ValueError("backoff_long_max must be >= backoff_long_initial")
        self._data: Optional[Dict[str, Any]] = None

    # -- properties ---------------------------------------------------------

    @property
    def file_path(self) -> Path:
        return self._file

    # -- persistence --------------------------------------------------------

    def _empty(self) -> Dict[str, Any]:
        return {
            "version": _FORMAT_VERSION,
            "seq": 0,
            "rows": [],
            "archive": [],
            "receipts": [],
        }

    def _load(self) -> Dict[str, Any]:
        if self._data is not None:
            return self._data
        try:
            text = self._file.read_text(encoding="utf-8")
        except FileNotFoundError:
            self._data = self._empty()
            return self._data
        data = json.loads(text)
        if not isinstance(data, dict) or data.get("version") != _FORMAT_VERSION:
            raise ValueError(
                f"outbox file {self._file} is malformed "
                f"(expected an object with version {_FORMAT_VERSION})"
            )
        for list_key in ("rows", "archive", "receipts"):
            if not isinstance(data.get(list_key), list):
                raise ValueError(
                    f"outbox file {self._file} is malformed "
                    f"({list_key!r} must be a list)"
                )
        if not isinstance(data.get("seq"), int) or isinstance(data.get("seq"), bool):
            raise ValueError(
                f"outbox file {self._file} is malformed (seq must be an int)"
            )
        self._data = data
        return data

    def _save(self) -> None:
        data = self._load()
        self._ensure_dir()
        _atomic_write(
            self._file, json.dumps(data, indent=2, ensure_ascii=False) + "\n"
        )

    def _ensure_dir(self) -> None:
        self._dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        # Holds message payloads: keep the dir 0700 regardless of umask
        # (owner-only but traversable — non-root needs the exec bit to
        # enter it) — queue.json itself stays 0600.
        os.chmod(self._dir, 0o700)

    # -- lookup -------------------------------------------------------------

    def _live_rows(self) -> List[Dict[str, Any]]:
        return self._load()["rows"]

    def _find(self, kind: str, key: str, seq: int, generation: int) -> Optional[Dict[str, Any]]:
        for row in self._live_rows():
            if (
                row["kind"] == kind
                and row["key"] == key
                and row["seq"] == seq
                and row["generation"] == generation
            ):
                return row
        return None

    def _find_by_pk(self, kind: str, key: str) -> Optional[Dict[str, Any]]:
        for row in self._live_rows():
            if row["kind"] == kind and row["key"] == key:
                return row
        return None

    def _archive_row(self, row: Dict[str, Any], status: str) -> None:
        row["status"] = status
        self._live_rows().remove(row)
        self._load()["archive"].append(row)

    # -- lifecycle ----------------------------------------------------------

    def restore(self) -> List[OutboxRow]:
        """Load persisted state for delivery (call once at startup).

        Rows claimed before a crash (``in_flight``) are reset to
        ``pending`` and become claimable immediately — they are replayed
        with the same deterministic ``tx_id``.  Returns the replayed
        rows.
        """
        data = self._load()
        now = time.time()
        replayed: List[OutboxRow] = []
        for raw in data["rows"]:
            if raw.get("status") == "in_flight":
                raw["status"] = "pending"
                raw["due"] = min(float(raw["due"]), now)
                replayed.append(OutboxRow.from_dict(raw))
        if replayed:
            self._save()
        return replayed

    def enqueue(
        self,
        kind: str,
        key: str,
        payload: Union[OutboundMessage, Dict[str, Any]],
        *,
        delay: float = 0.0,
        now: Optional[float] = None,
    ) -> OutboxRow:
        """Persist a row before any delivery attempt (store before send).

        Re-enqueuing a live ``(kind, key)`` replaces the payload:
        generation+1, attempts reset, queue position (``seq``) kept.
        Terminal rows (dead letter / dropped / gone) stay in the archive
        and are never re-queued; a fresh enqueue under the same key
        starts a new row at the tail with a new ``seq``.
        """
        if now is None:
            now = time.time()
        if delay < 0:
            raise ValueError("enqueue delay must be >= 0")
        data = self._load()
        payload_dict = (
            message_to_payload(payload)
            if isinstance(payload, OutboundMessage)
            else dict(payload)
        )
        existing = self._find_by_pk(kind, key)
        if existing is not None:
            existing["payload"] = payload_dict
            existing["generation"] += 1
            existing["attempts"] = 0
            existing["due"] = now + delay
            existing["status"] = "pending"
            row = existing
        else:
            data["seq"] += 1
            row = {
                "kind": kind,
                "key": key,
                "payload": payload_dict,
                "seq": data["seq"],
                "generation": 1,
                "attempts": 0,
                "due": now + delay,
                "status": "pending",
            }
            data["rows"].append(row)
        self._save()
        return OutboxRow.from_dict(row)

    def claim_due(self, now: Optional[float] = None) -> List[OutboxRow]:
        """Claim all claimable rows (marks them ``in_flight``, persisted).

        Claimable = ``pending`` and ``due`` passed, in ``seq`` order,
        subject to the per-kind in-flight cap and the one-in-flight-per-
        room ordering rule.
        """
        if now is None:
            now = time.time()
        rows = self._live_rows()
        in_flight = [r for r in rows if r["status"] == "in_flight"]
        busy_rooms = {
            r["payload"].get("room_id")
            for r in in_flight
            if r["payload"].get("room_id")
        }
        busy_kinds = Counter(r["kind"] for r in in_flight)
        due_rows = sorted(
            (r for r in rows if r["status"] == "pending" and r["due"] <= now),
            key=lambda r: r["seq"],
        )
        claimed: List[OutboxRow] = []
        for row in due_rows:
            room = row["payload"].get("room_id")
            if room is not None and room in busy_rooms:
                continue
            if busy_kinds[row["kind"]] >= self._concurrency:
                continue
            row["status"] = "in_flight"
            if room is not None:
                busy_rooms.add(room)
            busy_kinds[row["kind"]] += 1
            claimed.append(OutboxRow.from_dict(row))
        if claimed:
            self._save()
        return claimed

    def complete(self, kind: str, key: str, seq: int, generation: int) -> bool:
        """Remove a row on the ``(kind, key, seq, generation)`` match.

        A stale quadruple (superseded generation or seq) never removes a
        row and returns False.
        """
        row = self._find(kind, key, seq, generation)
        if row is None or row["status"] not in ("pending", "in_flight"):
            return False
        self._live_rows().remove(row)
        self._save()
        return True

    def fail(
        self, kind: str, key: str, seq: int, generation: int, *, now: Optional[float] = None
    ) -> Union[str, bool]:
        """Record one failed send attempt for a claimed row.

        Returns ``"dead_letter"`` when the row is terminally dead
        (:data:`DEAD_LETTER_ATTEMPTS` consecutive failures),
        ``"pending"`` when a backoff retry is scheduled, or False when
        the quadruple is stale (row replaced or already removed).
        """
        if now is None:
            now = time.time()
        row = self._find(kind, key, seq, generation)
        if row is None or row["status"] != "in_flight":
            return False
        row["attempts"] += 1
        if row["attempts"] >= DEAD_LETTER_ATTEMPTS:
            self._archive_row(row, "dead_letter")
            self._save()
            return "dead_letter"
        row["status"] = "pending"
        row["due"] = now + self.backoff_delay(row["kind"], row["attempts"])
        self._save()
        return "pending"

    def dead_letter(self, kind: str, key: str, seq: int, generation: int) -> bool:
        """Terminate a live row as dead letter (archived permanently)."""
        row = self._find(kind, key, seq, generation)
        if row is None or row["status"] not in ("pending", "in_flight"):
            return False
        self._archive_row(row, "dead_letter")
        self._save()
        return True

    # -- receipts (shared vocabulary) ----------------------------------------

    def record(
        self,
        kind: str,
        key: str,
        seq: int,
        generation: int,
        status: str,
        *,
        reason: Optional[str] = None,
        event_id: Optional[str] = None,
        tx_id: Optional[str] = None,
        now: Optional[float] = None,
    ) -> bool:
        """Append a delivery receipt (vocabulary: :data:`OUTBOX_STATUSES`).

        ``dropped`` / ``gone`` additionally archive the live row
        (permanent, never re-queued); the other statuses are pure
        receipts — the row may already have been removed by
        :meth:`complete`, and the quadruple in the receipt keeps it
        attributable.
        """
        if status not in OUTBOX_STATUSES:
            raise ValueError(f"status must be one of {OUTBOX_STATUSES}, got {status!r}")
        if now is None:
            now = time.time()
        data = self._load()
        data["receipts"].append(
            {
                "kind": kind,
                "key": key,
                "seq": seq,
                "generation": generation,
                "status": status,
                "reason": reason,
                "event_id": event_id,
                "tx_id": tx_id,
                "ts": now,
            }
        )
        if status in ("dropped", "gone"):
            row = self._find(kind, key, seq, generation)
            if row is not None and row["status"] in ("pending", "in_flight"):
                self._archive_row(row, status)
        self._save()
        return True

    def drop(
        self,
        kind: str,
        key: str,
        seq: int,
        generation: int,
        *,
        reason: Optional[str] = None,
        now: Optional[float] = None,
    ) -> bool:
        """Permanently drop a live row: receipt ``dropped{reason}`` + archive.

        A dropped row is never re-queued (not claimable, not replayed).
        """
        row = self._find(kind, key, seq, generation)
        if row is None or row["status"] not in ("pending", "in_flight"):
            return False
        return self.record(kind, key, seq, generation, "dropped", reason=reason, now=now)

    # -- scheduling -----------------------------------------------------------

    def backoff_delay(self, kind: str, attempts: int) -> float:
        """Exponential backoff after *attempts* failed attempts (>= 1).

        Default schedule ``[2s, 60s]``; a kind listed in
        ``long_running_kinds`` uses ``[5s, 300s]``:
        ``min(cap, initial * 2 ** (attempts - 1))``.
        """
        if attempts < 1:
            raise ValueError("attempts must be >= 1")
        if kind in self._long_kinds:
            initial, cap = self._long_initial, self._long_max
        else:
            initial, cap = self._initial, self._max
        return min(cap, initial * (2 ** (attempts - 1)))

    # -- accessors --------------------------------------------------------------

    def get(self, kind: str, key: str) -> Optional[OutboxRow]:
        """The live row for ``(kind, key)``, or None."""
        row = self._find_by_pk(kind, key)
        return OutboxRow.from_dict(row) if row is not None else None

    def rows(self) -> List[OutboxRow]:
        return [OutboxRow.from_dict(r) for r in self._live_rows()]

    def archive(self) -> List[OutboxRow]:
        """Terminal rows (dead letter / dropped / gone), permanent."""
        return [OutboxRow.from_dict(r) for r in self._load()["archive"]]

    def receipts(self) -> List[Dict[str, Any]]:
        return list(self._load()["receipts"])

    def pending_count(self) -> int:
        return sum(1 for r in self._live_rows() if r["status"] == "pending")

    def in_flight_count(self) -> int:
        return sum(1 for r in self._live_rows() if r["status"] == "in_flight")

    def dead_letter_count(self) -> int:
        return sum(
            1 for r in self._load()["archive"] if r["status"] == "dead_letter"
        )
