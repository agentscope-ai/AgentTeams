"""A minimal stand-in for the agentteams-matrix core, for the sidecar tests.

Implements only the surface the sidecar package touches (see
sidecar/_core.py): the dataclasses, the session store (no-op), the core
lifecycle (async run/stop/submit with the same running/stop semantics),
the sidecar server (a REAL Unix socket speaking the v1 line protocol,
0600, one connection at a time, reconnectable) and the frame builders.
No matrix-nio, no network, deterministic.

The behavioural contract mirrored here is the one in the real core's
core/sidecar.py and core/core.py: recv on an empty server raises
"sidecar has no active connection", EOF raises "sidecar connection
closed by peer", the handshake ack carries an empty event_id, a second
concurrent connection is rejected, and stop() removes the socket file.
"""

from __future__ import annotations

import asyncio
import json
import os
import socket
import stat
import threading
from dataclasses import dataclass, field
from typing import Any, Optional

PROTOCOL_VERSION = 1


class CoreLockedError(RuntimeError):
    """Mirrors core.state.CoreLockedError (the tests do not take locks)."""


@dataclass(frozen=True)
class MediaRef:
    mxc_uri: str
    mime_type: Optional[str] = None
    file_name: Optional[str] = None
    size: Optional[int] = None

    def to_dict(self) -> dict:
        return {
            "mxc_uri": self.mxc_uri,
            "mime_type": self.mime_type,
            "file_name": self.file_name,
            "size": self.size,
        }

    @classmethod
    def from_dict(cls, data: dict) -> "MediaRef":
        return cls(
            mxc_uri=str(data["mxc_uri"]),
            mime_type=data.get("mime_type"),
            file_name=data.get("file_name"),
            size=data.get("size"),
        )


@dataclass(frozen=True)
class InboundEvent:
    event_id: str
    room_id: str
    sender: str
    ts: int
    text: str
    media: list = field(default_factory=list)
    mentions: list = field(default_factory=list)
    thread_root: Optional[str] = None
    kind: str = "text"


@dataclass(frozen=True)
class OutboundMessage:
    room_id: str
    text: str
    reply_to: Optional[str] = None
    kind: str = "final"
    media: list = ()


@dataclass(frozen=True)
class CoreConfig:
    state_dir: str
    agent_id: str
    socket_path: Optional[str] = None

    def __post_init__(self) -> None:
        if not self.agent_id:
            raise ValueError("CoreConfig.agent_id must be non-empty")


class FileSessionStore:
    """No-op session store (the fake transport never persists)."""

    def __init__(self, state_dir: str) -> None:
        self.state_dir = str(state_dir)


# ---------------------------------------------------------------------------
# Frame builders (same shapes as the real core.sidecar)
# ---------------------------------------------------------------------------


def make_hello(agent: str, protocol: int = PROTOCOL_VERSION, capabilities=("text",)) -> dict:
    return {
        "type": "hello",
        "protocol": protocol,
        "agent": agent,
        "capabilities": list(capabilities),
    }


def make_ack(event_id: str) -> dict:
    return {"type": "ack", "event_id": event_id}


def make_inbound(event: InboundEvent) -> dict:
    return {
        "type": "inbound",
        "event_id": event.event_id,
        "room_id": event.room_id,
        "sender": event.sender,
        "text": event.text,
        "media": [m.to_dict() for m in event.media],
        "mentions": list(event.mentions),
        "thread_root": event.thread_root,
        "ts": event.ts,
    }


def frame_to_outbound(frame: dict) -> OutboundMessage:
    return OutboundMessage(
        room_id=frame["room_id"],
        text=frame["text"],
        reply_to=frame.get("reply_to"),
        kind=frame.get("kind", "final"),
        media=[MediaRef.from_dict(m) for m in frame.get("media", [])],
    )


def make_outbound(msg: OutboundMessage) -> dict:
    return {
        "type": "outbound",
        "room_id": msg.room_id,
        "text": msg.text,
        "reply_to": msg.reply_to,
        "kind": msg.kind,
        "media": [m.to_dict() for m in msg.media],
    }


# ---------------------------------------------------------------------------
# Line framer + connection state
# ---------------------------------------------------------------------------


class _Conn:
    """One accepted connection: the socket plus its line buffer."""

    def __init__(self, sock: socket.socket) -> None:
        self.sock = sock
        self.buffer = bytearray()

    def read_line(self) -> Optional[bytes]:
        """One newline-terminated line (without the newline), or None on a
        clean EOF. Bytes after the first newline stay buffered."""
        while True:
            newline = self.buffer.find(b"\n")
            if newline != -1:
                line = bytes(self.buffer[:newline])
                del self.buffer[: newline + 1]
                return line
            chunk = self.sock.recv(65536)
            if not chunk:
                if self.buffer:
                    raise RuntimeError("sidecar connection closed mid-frame")
                return None
            self.buffer.extend(chunk)


# ---------------------------------------------------------------------------
# SidecarServer (real Unix socket, v1 protocol, one connection at a time)
# ---------------------------------------------------------------------------


class SidecarServer:
    def __init__(self, socket_path) -> None:
        self._path = os.fspath(socket_path)
        self._listener: Optional[socket.socket] = None
        self._conn: Optional[_Conn] = None
        self._lock = threading.Lock()
        self.agent: Optional[str] = None
        self.capabilities: list = []

    @property
    def socket_path(self) -> str:
        return self._path

    @property
    def connected(self) -> bool:
        with self._lock:
            return self._conn is not None

    def start(self) -> None:
        if self._listener is not None:
            raise RuntimeError("sidecar server already started")
        parent = os.path.dirname(self._path)
        if parent:
            os.makedirs(parent, exist_ok=True)
        if os.path.exists(self._path):
            if not stat.S_ISSOCK(os.stat(self._path).st_mode):
                raise RuntimeError(f"socket path exists and is not a socket: {self._path}")
            os.unlink(self._path)
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.bind(self._path)
        os.chmod(self._path, 0o600)
        listener.listen(2)
        self._listener = listener
        threading.Thread(
            target=self._accept_loop, name="fake-sidecar-accept", daemon=True
        ).start()

    def stop(self) -> None:
        """Close listener + connection and remove the socket file (idempotent)."""
        listener, self._listener = self._listener, None
        if listener is not None:
            try:
                listener.close()
            except OSError:
                pass
            try:
                if stat.S_ISSOCK(os.stat(self._path).st_mode):
                    os.unlink(self._path)
            except OSError:
                pass
        self._close_connection()

    def _close_connection(self) -> None:
        with self._lock:
            conn, self._conn = self._conn, None
        self.agent = None
        self.capabilities = []
        if conn is not None:
            try:
                conn.sock.close()
            except OSError:
                pass

    def send(self, frame: dict) -> None:
        with self._lock:
            conn = self._conn
        if conn is None:
            raise RuntimeError("sidecar has no active connection")
        conn.sock.sendall(json.dumps(frame, separators=(",", ":")).encode() + b"\n")

    def recv(self) -> dict:
        with self._lock:
            conn = self._conn
        if conn is None:
            raise RuntimeError("sidecar has no active connection")
        line = conn.read_line()
        if line is None:
            self._close_connection()
            raise RuntimeError("sidecar connection closed by peer")
        return json.loads(line)

    # -- accept loop (keeps accepting after a peer EOFs, like the real one) --

    def _accept_loop(self) -> None:
        assert self._listener is not None
        while self._listener is not None:
            try:
                conn, _addr = self._listener.accept()
            except OSError:
                return  # listener closed
            try:
                self._handle_new_connection(conn)
            except Exception:
                try:
                    conn.close()
                except OSError:
                    pass

    def _handle_new_connection(self, conn: socket.socket) -> None:
        with self._lock:
            already = self._conn is not None
        if already:
            self._reject(conn, "connection_limit", "sidecar already has one connection")
            return
        state = _Conn(conn)
        try:
            conn.settimeout(10.0)
            line = state.read_line()
            if line is None:
                conn.close()
                return
            frame = json.loads(line)
            if frame.get("type") != "hello":
                raise ValueError("first frame must be hello")
            if frame.get("protocol") != PROTOCOL_VERSION:
                raise ValueError("protocol version mismatch")
            if not frame.get("agent") or not isinstance(frame.get("capabilities"), list):
                raise ValueError("bad hello fields")
        except (ValueError, KeyError, json.JSONDecodeError):
            self._reject(conn, "protocol_error", "handshake failed (fail-closed)")
            return
        except OSError:
            conn.close()
            return
        with self._lock:
            if self._conn is not None:  # lost a race with another accept
                conn.close()
                return
            self._conn = state
            self.agent = frame["agent"]
            self.capabilities = list(frame["capabilities"])
        conn.settimeout(None)
        try:
            conn.sendall(json.dumps(make_ack(""), separators=(",", ":")).encode() + b"\n")
        except OSError:
            self._close_connection()

    @staticmethod
    def _reject(conn: socket.socket, code: str, message: str) -> None:
        try:
            conn.sendall(
                json.dumps({"type": "error", "code": code, "message": message},
                           separators=(",", ":")).encode()
                + b"\n"
            )
        except OSError:
            pass
        try:
            conn.close()
        except OSError:
            pass


# ---------------------------------------------------------------------------
# MatrixCore (same lifecycle contract as core.core.MatrixCore)
# ---------------------------------------------------------------------------


class MatrixCore:
    def __init__(self, transport, store, bridge, config, on_error=None) -> None:
        self._transport = transport
        self._store = store
        self._bridge = bridge
        self._config = config
        self._on_error = on_error
        self._running = False
        self._stop_waiter: Optional[asyncio.Event] = None
        self.last_error: Optional[str] = None

    @property
    def config(self):
        return self._config

    @property
    def running(self) -> bool:
        return self._running

    async def run(self) -> None:
        if self._running:
            raise RuntimeError("core already running")
        self._running = True
        self._stop_waiter = asyncio.Event()
        try:
            await self._transport.start(self._on_transport_event)
        except BaseException:
            self._running = False
            self._stop_waiter = None
            raise
        await self._stop_waiter.wait()
        self._running = False
        self._stop_waiter = None

    async def stop(self) -> None:
        """Stop the transport and wake run() (idempotent)."""
        if not self._running:
            return
        await self._transport.stop()
        if self._stop_waiter is not None:
            self._stop_waiter.set()

    async def _on_transport_event(self, event) -> None:
        try:
            await self._bridge.on_inbound(event)
        except asyncio.CancelledError:
            raise
        except BaseException as exc:
            self.last_error = f"on_inbound: {exc!r}"

    async def submit(self, msg) -> str:
        if not self._running:
            raise RuntimeError("core is not running")
        try:
            event_id = await self._transport.send(msg)
        except BaseException as exc:
            self.last_error = f"submit: {exc!r}"
            raise
        try:
            await self._bridge.on_runtime_output(msg)
        except BaseException as exc:
            self.last_error = f"on_runtime_output: {exc!r}"
        return event_id
