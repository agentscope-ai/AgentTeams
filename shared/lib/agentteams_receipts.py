"""Exact Matrix receipts from committed Harness inputs, never from queue admission."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import threading
from urllib.parse import quote, urlencode
from urllib.request import Request, urlopen

RECEIPT_TYPE = "com.agentteams.session_receipt"


class Receipts:
    def __init__(self, workspace):
        self.workspace = Path(workspace)
        self.path = self.workspace / ".agentteams/message-receipts.sqlite"
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with self.connect() as db:
            db.executescript("""
                CREATE TABLE IF NOT EXISTS receipts (
                    room TEXT NOT NULL, event TEXT NOT NULL, status TEXT NOT NULL,
                    delivered INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(room,event,status));
                CREATE TABLE IF NOT EXISTS committed (event TEXT PRIMARY KEY);
                CREATE TABLE IF NOT EXISTS state (key TEXT PRIMARY KEY, value TEXT);
                CREATE TABLE IF NOT EXISTS sources (
                    anchor TEXT NOT NULL, event TEXT NOT NULL, PRIMARY KEY(anchor,event));
            """)

    def connect(self):
        return sqlite3.connect(self.path, timeout=10)

    def enqueue(self, room, event, status="read"):
        if not room.startswith("!") or not event.startswith("$") or status not in {"read", "received"}:
            return
        with self.connect() as db:
            db.execute("INSERT OR IGNORE INTO receipts(room,event,status) VALUES(?,?,?)", (room, event, status))
            if status == "received" and db.execute("SELECT 1 FROM committed WHERE event=?", (event,)).fetchone():
                db.execute("INSERT OR IGNORE INTO receipts(room,event,status) VALUES(?,?,'read')", (room, event))

    def committed(self, events):
        with self.connect() as db:
            for event in set(events):
                if not isinstance(event, str) or not event.startswith("$"):
                    continue
                db.execute("INSERT OR IGNORE INTO committed VALUES(?)", (event,))
                db.execute("INSERT OR IGNORE INTO committed SELECT event FROM sources WHERE anchor=?", (event,))
            db.execute("""INSERT OR IGNORE INTO receipts(room,event,status)
                SELECT room,event,'read' FROM receipts JOIN committed USING(event)""")

    def link(self, room, anchor, events):
        """Track only history IDs actually retained in this native input."""
        with self.connect() as db:
            db.executemany("INSERT OR IGNORE INTO sources VALUES(?,?)", [(anchor, event) for event in events])
        for event in events:
            self.enqueue(room, event, "received")

    def flush(self, request):
        with self.connect() as db:
            pending = db.execute("SELECT room,event,status FROM receipts WHERE delivered=0").fetchall()
        for room, event, status in pending:
            txn = hashlib.sha256(json.dumps([room, event, status]).encode()).hexdigest()
            try:
                request("PUT", f"/_matrix/client/v3/rooms/{quote(room, safe='')}/send/{RECEIPT_TYPE}/{txn}",
                        {"version": 1, "status": status, "m.relates_to": {"rel_type": RECEIPT_TYPE, "event_id": event}})
            except Exception:
                continue
            with self.connect() as db:
                db.execute("UPDATE receipts SET delivered=1 WHERE room=? AND event=? AND status=?", (room, event, status))


def matrix_request(homeserver, token):
    def request(method, path, content=None):
        req = Request(homeserver.rstrip("/") + path, method=method,
                      data=None if content is None else json.dumps(content).encode(),
                      headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
        with urlopen(req, timeout=35 if "/sync?" in path else 5) as response:
            return json.load(response)
    return request


def capture_audience(request, room, content):
    """Snapshot recipients before sending a new chat message; never block delivery."""
    if "com.agentteams.read_audience" in content or content.get("msgtype") not in {"m.text", "m.image", "m.file", "m.audio", "m.video"}:
        return content
    if content.get("m.relates_to", {}).get("rel_type") == "m.replace":
        return content
    try:
        joined = request("GET", f"/_matrix/client/v3/rooms/{quote(room, safe='')}/joined_members")["joined"]
        content = {**content, "com.agentteams.read_audience": {"version": 1, "members": [
            {"user_id": user, "display_name": member.get("display_name") or user, "kind": "member"}
            for user, member in joined.items()
        ]}}
    except Exception:
        pass
    return content


def openclaw_inputs(row):
    message = row.get("message", {})
    transport = message.get("__openclaw", {}).get("transport", {})
    if message.get("role") == "user" and not message.get("excludeFromContext") and transport.get("channel") in {"matrix", "agentteams-matrix"}:
        event = transport.get("messageId")
        return [event] if isinstance(event, str) and event.startswith("$") else []
    return []


def scan_inputs(receipts, runtime, offsets):
    workspace = receipts.workspace
    if runtime == "openclaw":
        for path in (workspace / ".openclaw/agents").glob("*/agent/openclaw-agent.sqlite"):
            wal = Path(str(path) + "-wal")
            version = (path.stat().st_mtime_ns, wal.stat().st_mtime_ns if wal.exists() else None)
            if offsets.get(str(path)) == version:
                continue
            result = subprocess.run(["node", str(Path(__file__).with_name("agentteams_openclaw_receipts.mjs")), str(path)],
                                    capture_output=True, text=True, check=True, timeout=15)
            receipts.committed(json.loads(result.stdout))
            offsets[str(path)] = version
        for path in (workspace / ".openclaw/agents").glob("*/sessions/*.jsonl"):
            stat = path.stat()
            key = (str(path), stat.st_ino)
            offset = offsets.get(key, 0)
            if stat.st_size < offset:
                offset = 0
            with path.open() as source:
                source.seek(offset)
                while line := source.readline():
                    if not line.endswith("\n"):
                        break
                    try:
                        receipts.committed(openclaw_inputs(json.loads(line)))
                    except ValueError:
                        break
                    offsets[key] = source.tell()
    elif runtime == "hermes":
        path = workspace / ".hermes/state.db"
        if path.exists():
            with sqlite3.connect(f"file:{path}?mode=ro", uri=True) as db:
                rows = db.execute("""SELECT platform_message_id FROM messages m JOIN sessions s
                    ON m.session_id=s.id WHERE s.source='matrix' AND m.role='user'
                    AND platform_message_id IS NOT NULL""").fetchall()
            receipts.committed([row[0] for row in rows])
    elif runtime in {"openhuman", "deepseek"}:
        directory = "matrix-sessions" if runtime == "openhuman" else "dsh-checkpoints"
        for path in (workspace / ".agentteams" / directory).glob("*.json"):
            data = json.loads(path.read_text())
            for item in data.get("receipts", []):
                receipts.enqueue(item["room_id"], item["event_id"])
    elif runtime == "codex":
        path = workspace / "runtime/codex-bridge-state.json"
        if path.exists():
            for record in json.loads(path.read_text()).get("events", {}).values():
                if record.get("input_checkpointed"):
                    event = record["event"]
                    for event_id in event.get("session_source_event_ids", [event["event_id"]]):
                        receipts.enqueue(event["room_id"], event_id)


def credentials(workspace):
    server = os.getenv("AGENTTEAMS_MATRIX_URL") or os.getenv("AGENTTEAMS_MATRIX_HOMESERVER")
    token = os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN")
    if server and token:
        return server, token
    for path in [workspace / "openclaw.json", workspace / "agent-config/openclaw.json"]:
        if path.exists():
            channel = json.loads(path.read_text()).get("channels", {}).get("matrix", {})
            if channel.get("homeserver") and channel.get("accessToken"):
                return channel["homeserver"], channel["accessToken"]
    raise ValueError("Matrix credentials are not ready")


def serve(receipts, runtime):
    stop = threading.Event()

    def receive():
        while not stop.is_set():
            try:
                request = matrix_request(*credentials(receipts.workspace))
                user_id = request("GET", "/_matrix/client/v3/account/whoami")["user_id"]
                with receipts.connect() as db:
                    cursor = db.execute("SELECT value FROM state WHERE key='since'").fetchone()
                query = {"timeout": 20000, "filter": json.dumps({"room": {"timeline": {"types": ["m.room.message"], "limit": 100}}})}
                if cursor:
                    query["since"] = cursor[0]
                payload = request("GET", "/_matrix/client/v3/sync?" + urlencode(query))
                for room_id, room in payload.get("rooms", {}).get("join", {}).items():
                    for event in room.get("timeline", {}).get("events", []):
                        if event.get("type") == "m.room.message" and event.get("sender") != user_id:
                            receipts.enqueue(room_id, event.get("event_id", ""), "received")
                with receipts.connect() as db:
                    db.execute("INSERT OR REPLACE INTO state VALUES('since',?)", (payload["next_batch"],))
            except Exception:
                stop.wait(5)

    threading.Thread(target=receive, daemon=True).start()
    offsets = {}
    while True:
        try:
            scan_inputs(receipts, runtime, offsets)
            receipts.flush(matrix_request(*credentials(receipts.workspace)))
        except Exception:
            pass  # A failed native checkpoint or transport never creates a read.
        stop.wait(1)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["serve", "enqueue"])
    parser.add_argument("--workspace", type=Path, default=Path(os.getenv("AGENTTEAMS_WORKER_HOME") or os.environ["HOME"]))
    parser.add_argument("--runtime", choices=["openclaw", "hermes", "deepseek", "codex", "openhuman"])
    parser.add_argument("--room")
    parser.add_argument("--event")
    parser.add_argument("--status", choices=["received", "read"], default="read")
    args = parser.parse_args()
    receipts = Receipts(args.workspace)
    if args.action == "enqueue":
        receipts.enqueue(args.room, args.event, args.status)
    else:
        serve(receipts, args.runtime)


if __name__ == "__main__":
    main()
