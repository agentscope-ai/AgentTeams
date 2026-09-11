"""Durable Matrix delivery and room-session state."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
import time
from typing import Any


def atomic_write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(text, encoding="utf-8")
    temporary.replace(path)


class BridgeState:
    """Durable Matrix cursor and one DSH session identity per room."""

    def __init__(self, path: Path, data: dict[str, Any]) -> None:
        self.path = path
        self.data = data

    @classmethod
    def load(cls, path: Path) -> "BridgeState":
        if not path.exists():
            return cls(path, {"version": 1, "next_batch": "", "rooms": {}, "events": {}})
        decoded = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(decoded, dict) or decoded.get("version") != 1:
            raise RuntimeError(f"unsupported Matrix bridge state in {path}")
        decoded.setdefault("next_batch", "")
        decoded.setdefault("rooms", {})
        decoded.setdefault("events", {})
        if not isinstance(decoded["rooms"], dict) or not isinstance(decoded["events"], dict):
            raise RuntimeError(f"invalid Matrix bridge state in {path}")
        return cls(path, decoded)

    @property
    def next_batch(self) -> str | None:
        value = str(self.data.get("next_batch") or "").strip()
        return value or None

    @next_batch.setter
    def next_batch(self, value: str | None) -> None:
        self.data["next_batch"] = value or ""

    def session_for(self, room_id: str) -> tuple[str, bool]:
        rooms = self.data["rooms"]
        room = rooms.get(room_id)
        if not isinstance(room, dict):
            digest = hashlib.sha256(room_id.encode("utf-8")).hexdigest()[:32]
            room = {"session_id": f"session-agentteams-{digest}", "ready": False}
            rooms[room_id] = room
        session_id = str(room.get("session_id") or "")
        if not session_id:
            raise RuntimeError(f"Matrix room {room_id} has no DSH session id")
        return session_id, room.get("ready") is True

    def mark_session_ready(self, room_id: str) -> None:
        self.session_for(room_id)
        self.data["rooms"][room_id]["ready"] = True

    def begin_event(self, event_id: str, room_id: str) -> int:
        record = self.data["events"].get(event_id)
        if not isinstance(record, dict):
            record = {"room_id": room_id, "attempts": 1}
            self.data["events"][event_id] = record
        else:
            attempts = max(1, int(record.get("attempts") or 0))
            if record.get("status") == "failed":
                attempts += 1
            record["attempts"] = attempts
        record["status"] = "processing"
        record["updated_at"] = int(time.time())
        return record["attempts"]

    def mark_answer(self, event_id: str, answer: str, outputs: list[str]) -> None:
        record = self.data["events"].setdefault(event_id, {})
        record.update(
            {
                "status": "answer_ready",
                "answer": answer,
                "outputs": list(outputs),
                "last_error": "",
                "updated_at": int(time.time()),
            }
        )
        record.pop("outbox_before", None)

    def mark_outbox_before(self, event_id: str, snapshot: dict[str, str]) -> None:
        record = self.data["events"].setdefault(event_id, {})
        record["outbox_before"] = dict(snapshot)
        record["updated_at"] = int(time.time())

    def outbox_before_for(self, event_id: str) -> dict[str, str] | None:
        record = self.data["events"].get(event_id)
        if not isinstance(record, dict) or not isinstance(record.get("outbox_before"), dict):
            return None
        return {str(key): str(value) for key, value in record["outbox_before"].items()}

    def answer_for(self, event_id: str) -> tuple[str, list[str]] | None:
        record = self.data["events"].get(event_id)
        if not isinstance(record, dict) or not isinstance(record.get("answer"), str):
            return None
        outputs = record.get("outputs")
        return record["answer"], [str(value) for value in outputs] if isinstance(outputs, list) else []

    def mark_failure(self, event_id: str, error: str) -> None:
        record = self.data["events"].setdefault(event_id, {"attempts": 1})
        record.update({"status": "failed", "last_error": error, "updated_at": int(time.time())})

    def should_retry(self, event_id: str, max_attempts: int) -> bool:
        record = self.data["events"].get(event_id)
        attempts = int(record.get("attempts") or 0) if isinstance(record, dict) else 0
        return attempts < max_attempts

    def mark_completed(self, event_id: str, reply_event_id: str) -> None:
        record = self.data["events"].setdefault(event_id, {})
        record.update(
            {
                "status": "completed",
                "reply_event_id": reply_event_id,
                "updated_at": int(time.time()),
            }
        )
        completed = sorted(
            (
                (key, int(value.get("updated_at") or 0))
                for key, value in self.data["events"].items()
                if isinstance(value, dict) and value.get("status") == "completed"
            ),
            key=lambda item: item[1],
            reverse=True,
        )
        for old_event_id, _updated_at in completed[2000:]:
            self.data["events"].pop(old_event_id, None)

    def is_completed(self, event_id: str) -> bool:
        record = self.data["events"].get(event_id)
        return isinstance(record, dict) and record.get("status") == "completed"

    def save(self) -> None:
        atomic_write(self.path, json.dumps(self.data, ensure_ascii=False, sort_keys=True) + "\n")
