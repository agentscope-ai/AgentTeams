"""Minimal Matrix event source and current-room reply transport."""

from __future__ import annotations

from dataclasses import dataclass
import json
import re
import time
from typing import Any
import urllib.error
import urllib.parse
import urllib.request

from .security import Redactor


TASK_ASSIGNED_RE = re.compile(r"\bTASK_ASSIGNED\s*:\s*([A-Za-z0-9._-]+)")
MATRIX_MENTION_RE = re.compile(r"@[A-Za-z0-9._=+/\-]+:[A-Za-z0-9.\-]+(?::\d+)?")


class MatrixError(RuntimeError):
    """Raised for Matrix transport or response errors."""


@dataclass(frozen=True)
class AssignedTask:
    event_id: str
    room_id: str
    sender: str
    task_id: str
    body: str


class MatrixClient:
    def __init__(self, homeserver: str, token: str, user_id: str, *, timeout: float = 35.0) -> None:
        self.homeserver = homeserver.rstrip("/")
        self.token = token
        self.user_id = user_id
        self.timeout = timeout
        self.redactor = Redactor([token])
        if not self.homeserver or not self.token or not self.user_id:
            raise MatrixError("Matrix homeserver, token, and user id are required")

    def _request(
        self,
        method: str,
        path: str,
        *,
        query: dict[str, str] | None = None,
        payload: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        url = self.homeserver + path
        if query:
            url += "?" + urllib.parse.urlencode(query)
        data = None if payload is None else json.dumps(payload).encode("utf-8")
        request = urllib.request.Request(
            url,
            data=data,
            headers={
                "Authorization": f"Bearer {self.token}",
                "Content-Type": "application/json",
            },
            method=method,
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                result = json.loads(response.read().decode("utf-8") or "{}")
        except urllib.error.HTTPError as exc:
            try:
                detail = self.redactor.redact(
                    exc.read().decode("utf-8", errors="replace")[:300]
                )
            finally:
                exc.close()
            raise MatrixError(f"Matrix API HTTP {exc.code}: {detail}") from exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise MatrixError(
                f"Matrix API request failed: {self.redactor.redact(exc)}"
            ) from exc
        if not isinstance(result, dict):
            raise MatrixError("Matrix API returned a non-object response")
        return result

    def whoami(self) -> str:
        result = self._request("GET", "/_matrix/client/v3/account/whoami")
        return str(result.get("user_id") or "")

    def sync(self, since: str = "", *, timeout_ms: int = 30000) -> dict[str, Any]:
        query = {"timeout": str(max(0, timeout_ms))}
        if since:
            query["since"] = since
        return self._request("GET", "/_matrix/client/v3/sync", query=query)

    def assigned_tasks(self, sync_response: dict[str, Any]) -> list[AssignedTask]:
        rooms = sync_response.get("rooms") if isinstance(sync_response.get("rooms"), dict) else {}
        joined = rooms.get("join") if isinstance(rooms.get("join"), dict) else {}
        tasks: list[AssignedTask] = []
        for room_id, room in joined.items():
            if not isinstance(room, dict):
                continue
            timeline = room.get("timeline") if isinstance(room.get("timeline"), dict) else {}
            events = timeline.get("events") if isinstance(timeline.get("events"), list) else []
            for event in events:
                if not isinstance(event, dict) or event.get("type") != "m.room.message":
                    continue
                if str(event.get("sender") or "") == self.user_id:
                    continue
                content = event.get("content") if isinstance(event.get("content"), dict) else {}
                body = str(content.get("body") or "")
                match = TASK_ASSIGNED_RE.search(body)
                if not match:
                    continue
                mentions = content.get("m.mentions") if isinstance(content.get("m.mentions"), dict) else {}
                mentioned_ids = mentions.get("user_ids") if isinstance(mentions.get("user_ids"), list) else []
                if self.user_id not in mentioned_ids and self.user_id not in body:
                    continue
                event_id = str(event.get("event_id") or "")
                if not event_id:
                    continue
                tasks.append(
                    AssignedTask(
                        event_id=event_id,
                        room_id=str(room_id),
                        sender=str(event.get("sender") or ""),
                        task_id=match.group(1),
                        body=body,
                    )
                )
        return tasks

    def send_text(self, room_id: str, text: str, *, transaction_id: str = "") -> str:
        txn_id = transaction_id or f"codex-worker-{int(time.time() * 1000)}"
        encoded_room = urllib.parse.quote(room_id, safe="")
        encoded_txn = urllib.parse.quote(txn_id, safe="")
        mentions = sorted(set(MATRIX_MENTION_RE.findall(text)))
        content: dict[str, Any] = {"msgtype": "m.text", "body": text}
        if mentions:
            content["m.mentions"] = {"user_ids": mentions}
        result = self._request(
            "PUT",
            f"/_matrix/client/v3/rooms/{encoded_room}/send/m.room.message/{encoded_txn}",
            payload=content,
        )
        return str(result.get("event_id") or "")
