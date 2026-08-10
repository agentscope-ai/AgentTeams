"""Host-side bridge: pulls REAL messages out of the Dockerised Agent Team.

Responsibilities
  1. Log in to the Tuwunel homeserver running inside `agentteams-controller`.
  2. Keep a live /sync loop so every room event (admin DM, WeChat gateway room,
     Manager<->Worker rooms) lands in an in-memory event log.
  3. Auto-join rooms the Manager invites us into (Worker rooms appear this way).
  4. Serve the two observability views plus a small JSON API.

Every byte the two HTML views render comes from this event log, which is fed
exclusively by the homeserver inside the container. There is no mock path.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from matrix_client import MatrixClient, MatrixError, build_client_from_env  # noqa: E402

VIEWER_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "viewer")

WECHAT_ENVELOPE_RE = re.compile(
    r"^\[微信群消息\]\s*群:\s*(?P<group>[^|]+?)\s*\|\s*成员:\s*(?P<sender>[^|]+?)\s*\|\s*"
    r"消息ID:\s*(?P<mid>[^|\n]+?)\s*(?:\|\s*时间:\s*(?P<ts>[^\n]+?))?\s*\n内容:\s*(?P<body>.*)$",
    re.S,
)
GROUP_REPLY_RE = re.compile(r"^\[群回复\]\s*(?P<body>.*)$", re.S)


class EventStore:
    """Thread-safe, append-only log of real Matrix events."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._events: list[dict[str, Any]] = []
        self._seen: set[str] = set()
        self._rooms: dict[str, dict[str, Any]] = {}
        self._seq = 0
        self.status: dict[str, Any] = {
            "connected": False,
            "user_id": None,
            "gateway_room_id": None,
            "gateway_room_name": None,
            "last_sync": None,
            "error": None,
            "sync_cycles": 0,
        }

    def upsert_room(self, room_id: str, **fields: Any) -> None:
        with self._lock:
            room = self._rooms.setdefault(room_id, {"room_id": room_id})
            room.update({k: v for k, v in fields.items() if v is not None})

    def rooms(self) -> list[dict[str, Any]]:
        with self._lock:
            return [dict(r) for r in self._rooms.values()]

    def room_label(self, room_id: str) -> str:
        with self._lock:
            room = self._rooms.get(room_id, {})
            return room.get("label") or room.get("name") or room_id

    def add(self, event: dict[str, Any]) -> bool:
        eid = event.get("event_id")
        if not eid:
            return False
        with self._lock:
            if eid in self._seen:
                return False
            self._seen.add(eid)
            self._seq += 1
            event["seq"] = self._seq
            self._events.append(event)
            self._events.sort(key=lambda e: (e.get("ts") or 0, e.get("seq") or 0))
            for idx, item in enumerate(self._events, start=1):
                item["seq"] = idx
            self._seq = len(self._events)
        return True

    def since(self, seq: int) -> list[dict[str, Any]]:
        with self._lock:
            return [dict(e) for e in self._events if (e.get("seq") or 0) > seq]

    def all(self) -> list[dict[str, Any]]:
        with self._lock:
            return [dict(e) for e in self._events]

    def set_status(self, **fields: Any) -> None:
        with self._lock:
            self.status.update(fields)

    def get_status(self) -> dict[str, Any]:
        with self._lock:
            return dict(self.status)


def short_name(user_id: str | None) -> str:
    if not user_id:
        return "unknown"
    return user_id.lstrip("@").split(":", 1)[0]


def classify(sender_local: str, admin_user: str) -> str:
    if sender_local == admin_user:
        return "admin"
    if sender_local == "manager":
        return "manager"
    return "worker"


class Bridge:
    def __init__(
        self,
        client: MatrixClient,
        env: dict[str, str],
        store: EventStore,
        gateway_room_name: str,
        history_limit: int = 200,
    ) -> None:
        self.client = client
        self.env = env
        self.store = store
        self.admin_user = env.get("AGENTTEAMS_ADMIN_USER", "admin")
        self.gateway_room_name = gateway_room_name
        self.history_limit = history_limit
        self.gateway_room_id: str | None = None
        self._stop = threading.Event()

    # ------------------------------------------------------------------
    def connect(self) -> None:
        password = self.env.get("AGENTTEAMS_ADMIN_PASSWORD", "")
        if not password:
            raise MatrixError("AGENTTEAMS_ADMIN_PASSWORD missing from env file")
        self.client.login(self.admin_user, password)
        self.store.set_status(connected=True, user_id=self.client.user_id, error=None)

    def ensure_gateway_room(self) -> str:
        """Find (or create) the Matrix room that represents the WeChat group."""
        manager_id = f"@manager:{self.client.server_name}"
        for room_id in self.client.joined_rooms():
            name = self.client.room_name(room_id)
            if name == self.gateway_room_name:
                self.gateway_room_id = room_id
                members = self.client.room_members(room_id)
                if manager_id not in members:
                    try:
                        self.client.invite(room_id, manager_id)
                    except MatrixError:
                        pass
                break
        if not self.gateway_room_id:
            self.gateway_room_id = self.client.create_room(
                name=self.gateway_room_name,
                invite=[manager_id],
                topic="WeChat group gateway - inbound IT service desk requests",
            )
        self.store.set_status(
            gateway_room_id=self.gateway_room_id,
            gateway_room_name=self.gateway_room_name,
        )
        self.store.upsert_room(
            self.gateway_room_id, name=self.gateway_room_name, label=self.gateway_room_name,
            kind="gateway",
        )
        return self.gateway_room_id

    # ------------------------------------------------------------------
    def _refresh_room_meta(self, room_id: str) -> None:
        name = self.client.room_name(room_id)
        try:
            members = self.client.room_members(room_id)
        except MatrixError:
            members = []
        locals_ = sorted({short_name(m) for m in members})
        kind = "gateway" if room_id == self.gateway_room_id else None
        if kind is None:
            non_admin = [m for m in locals_ if m != self.admin_user]
            if "manager" in locals_ and len(non_admin) == 1:
                kind = "admin_dm"
            elif "manager" in locals_ and len(non_admin) > 1:
                kind = "worker_room"
            else:
                kind = "other"
        label = name
        if not label:
            peers = [m for m in locals_ if m not in (self.admin_user,)]
            label = " / ".join(peers) if peers else room_id
        self.store.upsert_room(room_id, name=name, label=label, members=locals_, kind=kind)

    def _ingest(self, room_id: str, event: dict[str, Any]) -> None:
        if event.get("type") != "m.room.message":
            return
        content = event.get("content") or {}
        body = content.get("body")
        if not body:
            return
        sender = event.get("sender") or ""
        sender_local = short_name(sender)
        ts = event.get("origin_server_ts") or int(time.time() * 1000)
        record: dict[str, Any] = {
            "event_id": event.get("event_id"),
            "room_id": room_id,
            "sender": sender,
            "sender_local": sender_local,
            "role": classify(sender_local, self.admin_user),
            "body": body,
            "ts": ts,
        }
        envelope = WECHAT_ENVELOPE_RE.match(body.strip())
        if envelope and room_id == self.gateway_room_id:
            record["kind"] = "wechat_inbound"
            record["wechat"] = {
                "group": envelope.group("group").strip(),
                "sender": envelope.group("sender").strip(),
                "message_id": envelope.group("mid").strip(),
                "text": envelope.group("body").strip(),
            }
        elif room_id == self.gateway_room_id and sender_local != self.admin_user:
            reply = GROUP_REPLY_RE.match(body.strip())
            record["kind"] = "wechat_reply" if reply else "gateway_progress"
            record["display_text"] = reply.groupdict()["body"].strip() if reply else body
        else:
            record["kind"] = "agent_flow"
        self.store.add(record)

    def bootstrap_history(self) -> None:
        for room_id in self.client.joined_rooms():
            self._refresh_room_meta(room_id)
            try:
                chunk = self.client.messages(room_id, limit=self.history_limit, direction="b")
            except MatrixError:
                continue
            for event in reversed(chunk):
                self._ingest(room_id, event)

    # ------------------------------------------------------------------
    def sync_loop(self) -> None:
        since: str | None = None
        try:
            first = self.client.sync(since=None, timeout_ms=0)
            since = first.get("next_batch")
        except MatrixError as exc:
            self.store.set_status(error=str(exc))
        known_rooms: set[str] = set()
        while not self._stop.is_set():
            try:
                resp = self.client.sync(since=since, timeout_ms=25000)
                since = resp.get("next_batch") or since
                rooms = resp.get("rooms") or {}

                for room_id in (rooms.get("invite") or {}):
                    try:
                        self.client._request(
                            "POST",
                            f"/_matrix/client/v3/rooms/{MatrixClient._encode_room(room_id)}/join",
                            {},
                            timeout=30,
                        )
                        self._refresh_room_meta(room_id)
                        chunk = self.client.messages(room_id, limit=self.history_limit, direction="b")
                        for event in reversed(chunk):
                            self._ingest(room_id, event)
                    except MatrixError:
                        continue

                for room_id, data in (rooms.get("join") or {}).items():
                    if room_id not in known_rooms:
                        known_rooms.add(room_id)
                        self._refresh_room_meta(room_id)
                    events = ((data.get("timeline") or {}).get("events")) or []
                    for event in events:
                        if event.get("type") in ("m.room.name", "m.room.member"):
                            self._refresh_room_meta(room_id)
                        self._ingest(room_id, event)

                status = self.store.get_status()
                self.store.set_status(
                    last_sync=int(time.time() * 1000),
                    error=None,
                    sync_cycles=(status.get("sync_cycles") or 0) + 1,
                )
            except MatrixError as exc:
                self.store.set_status(error=str(exc))
                time.sleep(3)
            except Exception as exc:  # noqa: BLE001
                self.store.set_status(error=f"{type(exc).__name__}: {exc}")
                time.sleep(3)

    def stop(self) -> None:
        self._stop.set()


def make_handler(bridge: Bridge, store: EventStore):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt: str, *args: Any) -> None:  # silence noise
            return

        def _send(self, code: int, payload: bytes, ctype: str) -> None:
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(payload)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(payload)

        def _json(self, obj: Any, code: int = 200) -> None:
            self._send(code, json.dumps(obj, ensure_ascii=False).encode("utf-8"),
                       "application/json; charset=utf-8")

        def _file(self, filename: str) -> None:
            path = os.path.join(VIEWER_DIR, filename)
            if not os.path.isfile(path):
                self._send(404, b"not found", "text/plain; charset=utf-8")
                return
            with open(path, "rb") as handle:
                data = handle.read()
            ctype = "text/html; charset=utf-8" if filename.endswith(".html") else "text/plain"
            self._send(200, data, ctype)

        def do_GET(self) -> None:  # noqa: N802
            path = self.path.split("?", 1)[0]
            query = {}
            if "?" in self.path:
                for pair in self.path.split("?", 1)[1].split("&"):
                    if "=" in pair:
                        k, v = pair.split("=", 1)
                        query[k] = v

            if path in ("/", "/index.html"):
                self._file("index.html")
            elif path == "/wechat.html":
                self._file("wechat.html")
            elif path == "/agentflow.html":
                self._file("agentflow.html")
            elif path == "/api/status":
                st = store.get_status()
                st["rooms"] = store.rooms()
                st["event_count"] = len(store.all())
                self._json(st)
            elif path == "/api/events":
                seq = int(query.get("since", "0") or 0)
                events = store.since(seq)
                for ev in events:
                    ev["room_label"] = store.room_label(ev["room_id"])
                self._json({
                    "events": events,
                    "cursor": events[-1]["seq"] if events else seq,
                    "status": store.get_status(),
                    "rooms": store.rooms(),
                })
            else:
                self._send(404, b"not found", "text/plain; charset=utf-8")

        def do_POST(self) -> None:  # noqa: N802
            path = self.path.split("?", 1)[0]
            length = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(length) if length else b"{}"
            try:
                payload = json.loads(raw.decode("utf-8"))
            except json.JSONDecodeError:
                self._json({"error": "invalid json"}, 400)
                return

            if path == "/api/send":
                text = (payload.get("text") or "").strip()
                sender = (payload.get("sender") or "访客").strip()
                group = (payload.get("group") or bridge.gateway_room_name).strip()
                if not text:
                    self._json({"error": "empty text"}, 400)
                    return
                mid = f"wx-{int(time.time() * 1000)}"
                envelope = (
                    f"[微信群消息] 群: {group} | 成员: {sender} | 消息ID: {mid} | "
                    f"时间: {time.strftime('%Y-%m-%d %H:%M:%S')}\n内容: {text}"
                )
                try:
                    event_id = bridge.client.send_text(bridge.gateway_room_id or "", envelope)
                    self._json({"ok": True, "event_id": event_id, "message_id": mid})
                except MatrixError as exc:
                    self._json({"error": str(exc)}, 502)
            else:
                self._json({"error": "unknown endpoint"}, 404)

    return Handler


def main() -> int:
    parser = argparse.ArgumentParser(description="AgentTeams WeChat bridge / viewer")
    parser.add_argument("--port", type=int, default=8770)
    parser.add_argument("--group-room", default="微信群-IT服务台支持群")
    parser.add_argument("--env-file", default=None)
    parser.add_argument("--history", type=int, default=200)
    args = parser.parse_args()

    from matrix_client import load_env_file

    env = load_env_file(args.env_file)
    if not env:
        print("[bridge] ERROR: agentteams-manager.env not found. Is AgentTeams installed?")
        return 2

    client, env = build_client_from_env(env)
    store = EventStore()
    bridge = Bridge(client, env, store, args.group_room, history_limit=args.history)

    print(f"[bridge] env file      : {env.get('__env_file__')}")
    print(f"[bridge] matrix connect: {client.connect_url} (Host: {client.host_header})")
    try:
        bridge.connect()
    except MatrixError as exc:
        print(f"[bridge] ERROR login failed: {exc}")
        return 3
    print(f"[bridge] logged in as  : {client.user_id}")

    room_id = bridge.ensure_gateway_room()
    print(f"[bridge] gateway room  : {args.group_room} -> {room_id}")

    bridge.bootstrap_history()
    print(f"[bridge] history loaded: {len(store.all())} real events")

    thread = threading.Thread(target=bridge.sync_loop, daemon=True)
    thread.start()

    handler = make_handler(bridge, store)
    server = ThreadingHTTPServer(("127.0.0.1", args.port), handler)
    print(f"[bridge] view 1 (agent flow) : http://127.0.0.1:{args.port}/agentflow.html")
    print(f"[bridge] view 2 (wechat group): http://127.0.0.1:{args.port}/wechat.html")
    print("[bridge] Ctrl+C to stop")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n[bridge] shutting down")
    finally:
        bridge.stop()
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
