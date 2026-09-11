"""Matrix transport and event parsing for the managed DeepSeek Harness worker."""

from __future__ import annotations

import hashlib
import json
import mimetypes
import os
import re
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


def matrix_events(
    payload: dict[str, Any],
    room_types: dict[str, str],
    own_user_id: str,
    agent_user_ids: set[str] | None = None,
) -> list[dict[str, Any]]:
    """Extract actionable text, image, and file events from watched joined rooms."""
    found: list[dict[str, Any]] = []
    joined = payload.get("rooms", {}).get("join", {})
    if not isinstance(joined, dict):
        return found
    for room_id, room in joined.items():
        room_type = room_types.get(room_id)
        if room_type not in {"personal", "team"} or not isinstance(room, dict):
            continue
        events = room.get("timeline", {}).get("events", [])
        if not isinstance(events, list):
            continue
        for event in events:
            if not isinstance(event, dict) or event.get("type") != "m.room.message":
                continue
            unsigned = event.get("unsigned")
            is_redacted = isinstance(unsigned, dict) and unsigned.get("redacted_because") is not None
            if event.get("sender") == own_user_id or is_redacted:
                continue
            content = event.get("content")
            if not isinstance(content, dict):
                continue
            sender = str(event.get("sender") or "").strip()
            mentions = content.get("m.mentions")
            mentioned_users = mentions.get("user_ids", []) if isinstance(mentions, dict) else []
            is_targeted = (
                isinstance(mentions, dict)
                and (mentions.get("room") is True or (isinstance(mentioned_users, list) and own_user_id in mentioned_users))
            )
            # Personal rooms remain conversational for humans. Team rooms are
            # dispatch surfaces, so every sender must explicitly target this
            # Worker. Agent messages require the same targeting in either room
            # to prevent reply loops while still allowing delegated replies.
            if (room_type == "team" or sender in (agent_user_ids or set())) and not is_targeted:
                continue
            msgtype = str(content.get("msgtype") or "")
            kind = {"m.text": "text", "m.image": "image", "m.file": "file"}.get(msgtype)
            if kind is None:
                continue
            body = str(content.get("body") or "").strip()
            event_id = str(event.get("event_id") or "").strip()
            if not body or not event_id or not sender:
                continue
            extracted = {
                "room_id": room_id,
                "event_id": event_id,
                "sender": sender,
                "body": body,
                "kind": kind,
            }
            if content.get("com.agentteams.auto_source_mention") is True:
                extracted["auto_source_mention"] = True
            if kind != "text":
                mxc_url = str(content.get("url") or "").strip()
                if not mxc_url.startswith("mxc://"):
                    continue
                info = content.get("info")
                mimetype = str(info.get("mimetype") or "") if isinstance(info, dict) else ""
                extracted.update(
                    {
                        "filename": str(content.get("filename") or body),
                        "mxc_url": mxc_url,
                        "mimetype": mimetype,
                    }
                )
            found.append(extracted)
    return found


def text_events(
    payload: dict[str, Any],
    room_types: dict[str, str],
    own_user_id: str,
    agent_user_ids: set[str] | None = None,
) -> list[dict[str, Any]]:
    """Return the plain-text compatibility view used by bridge callers."""
    return [
        {key: value for key, value in event.items() if key != "kind"}
        for event in matrix_events(payload, room_types, own_user_id, agent_user_ids)
        if event["kind"] == "text"
    ]


def visible_matrix_user_ids(text: str, allowed_user_ids: set[str]) -> list[str]:
    """Return known Matrix user IDs that appear as complete tokens in text."""
    found: list[str] = []
    for user_id in sorted(allowed_user_ids):
        trailing_boundary = r"(?![0-9A-Za-z_=/+@-]|\.(?=[0-9A-Za-z-])|:(?=[0-9]))"
        pattern = rf"(?<![0-9A-Za-z._=/+@-]){re.escape(user_id)}{trailing_boundary}"
        if re.search(pattern, text):
            found.append(user_id)
    return found


def random_transaction_key() -> str:
    return os.urandom(16).hex()


def matrix_transaction_id(key: str) -> str:
    return "dsh-" + hashlib.sha256(key.encode("utf-8")).hexdigest()[:48]


class MatrixClient:
    """Authenticated Matrix client with idempotent room-message delivery."""

    def __init__(self, homeserver: str, token: str) -> None:
        self.homeserver = homeserver.rstrip("/")
        self.token = token

    def request(self, method: str, path: str, body: dict[str, Any] | None = None, timeout: int = 45) -> dict[str, Any]:
        data = None if body is None else json.dumps(body, ensure_ascii=False).encode("utf-8")
        request = urllib.request.Request(
            f"{self.homeserver}{path}",
            data=data,
            method=method,
            headers={"Authorization": f"Bearer {self.token}", "Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return json.loads(response.read().decode("utf-8") or "{}")

    def sync(self, since: str | None, timeout_ms: int) -> dict[str, Any]:
        query: dict[str, str | int] = {"timeout": timeout_ms}
        if since:
            query["since"] = since
        return self.request("GET", f"/_matrix/client/v3/sync?{urllib.parse.urlencode(query)}", timeout=max(10, timeout_ms // 1000 + 10))

    def _download_path(self, mxc_url: str, prefix: str) -> str:
        parsed = urllib.parse.urlsplit(mxc_url)
        if parsed.scheme != "mxc" or not parsed.netloc or not parsed.path.strip("/"):
            raise RuntimeError(f"invalid Matrix media URL: {mxc_url}")
        server_name = urllib.parse.quote(parsed.netloc, safe="")
        media_id = urllib.parse.quote(parsed.path.strip("/"), safe="")
        return f"{prefix}/{server_name}/{media_id}"

    def download_media(self, mxc_url: str, max_bytes: int) -> tuple[bytes, str]:
        paths = [
            self._download_path(mxc_url, "/_matrix/client/v1/media/download"),
            self._download_path(mxc_url, "/_matrix/media/v3/download"),
        ]
        last_error: Exception | None = None
        for index, path in enumerate(paths):
            request = urllib.request.Request(
                f"{self.homeserver}{path}",
                method="GET",
                headers={"Authorization": f"Bearer {self.token}"},
            )
            try:
                with urllib.request.urlopen(request, timeout=60) as response:
                    announced = response.headers.get("Content-Length")
                    if announced and int(announced) > max_bytes:
                        raise RuntimeError(f"Matrix attachment exceeds {max_bytes} bytes")
                    data = response.read(max_bytes + 1)
                    if len(data) > max_bytes:
                        raise RuntimeError(f"Matrix attachment exceeds {max_bytes} bytes")
                    return data, str(response.headers.get_content_type() or "application/octet-stream")
            except urllib.error.HTTPError as error:
                last_error = error
                if index == 0 and error.code in (404, 405):
                    continue
                raise
        raise RuntimeError(f"Matrix media download failed: {last_error}")

    def upload_media(self, path: Path) -> tuple[str, str, int]:
        data = path.read_bytes()
        mimetype = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
        query = urllib.parse.urlencode({"filename": path.name})
        paths = [f"/_matrix/client/v1/media/upload?{query}", f"/_matrix/media/v3/upload?{query}"]
        last_error: Exception | None = None
        for index, request_path in enumerate(paths):
            request = urllib.request.Request(
                f"{self.homeserver}{request_path}",
                data=data,
                method="POST",
                headers={
                    "Authorization": f"Bearer {self.token}",
                    "Content-Type": mimetype,
                    "Content-Length": str(len(data)),
                },
            )
            try:
                with urllib.request.urlopen(request, timeout=120) as response:
                    result = json.loads(response.read().decode("utf-8") or "{}")
                    content_uri = str(result.get("content_uri") or "")
                    if not content_uri.startswith("mxc://"):
                        raise RuntimeError("Matrix media upload returned no content_uri")
                    return content_uri, mimetype, len(data)
            except urllib.error.HTTPError as error:
                last_error = error
                if index == 0 and error.code in (404, 405):
                    continue
                raise
        raise RuntimeError(f"Matrix media upload failed: {last_error}")

    def send_content(self, room_id: str, content: dict[str, Any], transaction_key: str) -> str:
        room = urllib.parse.quote(room_id, safe="")
        txn = matrix_transaction_id(transaction_key)
        result = self.request("PUT", f"/_matrix/client/v3/rooms/{room}/send/m.room.message/{txn}", content)
        return str(result.get("event_id") or "")

    def send_text(
        self,
        room_id: str,
        text: str,
        reply_to: str | None = None,
        transaction_key: str | None = None,
        mentioned_user_ids: list[str] | None = None,
        auto_source_mention: bool = False,
    ) -> str:
        content: dict[str, Any] = {"msgtype": "m.text", "body": text}
        if reply_to:
            content["m.relates_to"] = {"m.in_reply_to": {"event_id": reply_to}}
        mentions = sorted({user_id for user_id in (mentioned_user_ids or []) if user_id})
        if mentions:
            content["m.mentions"] = {"user_ids": mentions}
        if auto_source_mention:
            content["com.agentteams.auto_source_mention"] = True
        return self.send_content(room_id, content, transaction_key or random_transaction_key())

    def send_file(
        self,
        room_id: str,
        path: Path,
        reply_to: str,
        transaction_key: str,
        mentioned_user_ids: list[str] | None = None,
        auto_source_mention: bool = False,
    ) -> str:
        content_uri, mimetype, size = self.upload_media(path)
        msgtype = "m.image" if mimetype.startswith("image/") else "m.file"
        content: dict[str, Any] = {
            "msgtype": msgtype,
            "body": path.name,
            "filename": path.name,
            "url": content_uri,
            "info": {"mimetype": mimetype, "size": size},
            "m.relates_to": {"m.in_reply_to": {"event_id": reply_to}},
        }
        mentions = sorted({user_id for user_id in (mentioned_user_ids or []) if user_id})
        if mentions:
            content["m.mentions"] = {"user_ids": mentions}
        if auto_source_mention:
            content["com.agentteams.auto_source_mention"] = True
        return self.send_content(room_id, content, transaction_key)
