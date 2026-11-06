"""Sidecar protocol: one-JSON-per-line over a 0600 Unix socket (P9, PR1 M1).

The sidecar is how the OpenClaw adapter (PR2, TypeScript) talks to the
Python core without embedding it.  The protocol (see ``15-通用AgentTeamsMatrix-channel-20261029.md``
§3 and ``17-P9-详细设计-20261029.md`` §4):

* socket: ``$AGENTTEAMS_MATRIX_SOCKET`` (default ``<state>/sidecar.sock``),
  mode **0600** (only the owning user can connect);
* one JSON object per line;
* first frame is a ``hello`` handshake carrying the protocol version;
  the server answers a successful handshake with an ``ack`` whose
  ``event_id`` is empty (a non-empty ``event_id`` always refers to a
  Matrix event);
* the protocol version must match :data:`PROTOCOL_VERSION`; on mismatch
  (or any other handshake failure) the server **closes the connection
  and fail-closes** — there is no silent fallback to native Matrix;
* one agent has one sidecar and at most **one** connection at a time;
  a second concurrent connection is rejected (error frame + close);
* Matrix credentials never cross the protocol; the sidecar reads them
  from the environment / secret store.

Frames::

    {"type":"hello","protocol":1,"agent":"worker-a","capabilities":["text","media","e2ee","threads"]}
    {"type":"inbound","event_id":"$…","room_id":"!…","sender":"@…","text":"…","media":[],"mentions":[],"thread_root":null,"ts":0}
    {"type":"outbound","room_id":"!…","text":"…","reply_to":null,"kind":"final","media":[]}
    {"type":"typing","room_id":"!…","state":"start"}
    {"type":"ack","event_id":"$…"}
    {"type":"error","code":"…","message":"…"}

Note: the design sample inbound frame carries no ``kind``; when absent,
:func:`frame_to_inbound` derives it (``media`` if the frame has media,
else ``text``).  An explicit ``kind`` field is accepted for forward
compatibility.
"""

from __future__ import annotations

import json
import logging
import os
import socket
import stat
import threading
from pathlib import Path
from typing import Any, Union

from .interfaces import (
    INBOUND_KINDS,
    OUTBOUND_KINDS,
    TYPING_STATES,
    InboundEvent,
    MediaRef,
    OutboundMessage,
)

log = logging.getLogger("agentteams.matrix.core.sidecar")

PROTOCOL_VERSION = 1

# Hard per-line limit (both directions).
MAX_LINE_BYTES = 1024 * 1024  # 1 MiB

# Socket permission: only the owning user may connect.
SOCKET_MODE = 0o600

# Handshake read timeout (seconds) — a client that connects and sends
# nothing must not wedge the accept loop.
HANDSHAKE_TIMEOUT = 10.0

# Frame types.
HELLO = "hello"
INBOUND = "inbound"
OUTBOUND = "outbound"
TYPING = "typing"
ACK = "ack"
ERROR = "error"
FRAME_TYPES = (HELLO, INBOUND, OUTBOUND, TYPING, ACK, ERROR)


class SidecarProtocolError(Exception):
    """Protocol violation (bad frame, version mismatch, size limit, …)."""

    def __init__(self, message: str, code: str = "protocol_error") -> None:
        super().__init__(message)
        self.code = code


class SidecarNotConnectedError(RuntimeError):
    """The server has no active (handshaken) connection."""


# ---------------------------------------------------------------------------
# Codec: one JSON object per line
# ---------------------------------------------------------------------------


def encode_frame(frame: dict[str, Any]) -> bytes:
    """Serialise *frame* to a single protocol line (UTF-8, trailing ``\\n``)."""
    if not isinstance(frame, dict):
        raise SidecarProtocolError("frame must be a JSON object", "invalid_frame")
    if frame.get("type") not in FRAME_TYPES:
        raise SidecarProtocolError(
            f"frame type must be one of {FRAME_TYPES}, got {frame.get('type')!r}",
            "unknown_type",
        )
    line = json.dumps(frame, ensure_ascii=False, separators=(",", ":"))
    data = line.encode("utf-8") + b"\n"
    if len(data) > MAX_LINE_BYTES:
        raise SidecarProtocolError(
            f"frame exceeds the {MAX_LINE_BYTES} byte line limit", "line_exceeds_limit"
        )
    return data


def decode_line(line: Union[str, bytes, bytearray]) -> dict[str, Any]:
    """Parse and validate one protocol line into a frame dict."""
    if isinstance(line, (bytes, bytearray)):
        line = bytes(line).decode("utf-8")
    if len(line.encode("utf-8")) > MAX_LINE_BYTES:
        raise SidecarProtocolError(
            f"line exceeds the {MAX_LINE_BYTES} byte limit", "line_exceeds_limit"
        )
    try:
        frame = json.loads(line)
    except json.JSONDecodeError as exc:
        raise SidecarProtocolError(f"invalid JSON: {exc}", "invalid_json") from exc
    if not isinstance(frame, dict):
        raise SidecarProtocolError("frame must be a JSON object", "invalid_frame")
    if frame.get("type") not in FRAME_TYPES:
        raise SidecarProtocolError(
            f"unknown frame type: {frame.get('type')!r}", "unknown_type"
        )
    _validate_frame(frame)
    return frame


def _require(frame: dict[str, Any], *fields: str) -> None:
    for name in fields:
        if name not in frame:
            raise SidecarProtocolError(
                f"{frame['type']} frame is missing field {name!r}", "invalid_frame"
            )


def _validate_frame(frame: dict[str, Any]) -> None:
    ftype = frame["type"]
    if ftype == HELLO:
        _require(frame, "protocol", "agent", "capabilities")
        if not isinstance(frame["protocol"], int) or isinstance(frame["protocol"], bool):
            raise SidecarProtocolError("hello.protocol must be an integer", "invalid_frame")
        if not isinstance(frame["agent"], str) or not frame["agent"]:
            raise SidecarProtocolError("hello.agent must be a non-empty string", "invalid_frame")
        if not isinstance(frame["capabilities"], list):
            raise SidecarProtocolError("hello.capabilities must be a list", "invalid_frame")
    elif ftype == INBOUND:
        _require(
            frame, "event_id", "room_id", "sender", "text", "media", "mentions", "thread_root", "ts"
        )
        if not isinstance(frame["ts"], int) or isinstance(frame["ts"], bool):
            raise SidecarProtocolError("inbound.ts must be an integer", "invalid_frame")
        if "kind" in frame and frame["kind"] not in INBOUND_KINDS:
            raise SidecarProtocolError(
                f"inbound.kind must be one of {INBOUND_KINDS}, got {frame['kind']!r}",
                "invalid_frame",
            )
    elif ftype == OUTBOUND:
        _require(frame, "room_id", "text", "reply_to", "kind", "media")
        if frame["kind"] not in OUTBOUND_KINDS:
            raise SidecarProtocolError(
                f"outbound.kind must be one of {OUTBOUND_KINDS}, got {frame['kind']!r}",
                "invalid_frame",
            )
    elif ftype == TYPING:
        _require(frame, "room_id", "state")
        if frame["state"] not in TYPING_STATES:
            raise SidecarProtocolError(
                f"typing.state must be one of {TYPING_STATES}, got {frame['state']!r}",
                "invalid_frame",
            )
    elif ftype == ACK:
        _require(frame, "event_id")
    elif ftype == ERROR:
        _require(frame, "code", "message")


# ---------------------------------------------------------------------------
# Frame builders / dataclass conversion
# ---------------------------------------------------------------------------


def make_hello(agent: str, protocol: int = PROTOCOL_VERSION, capabilities: tuple = ("text",)) -> dict[str, Any]:
    return {
        "type": HELLO,
        "protocol": protocol,
        "agent": agent,
        "capabilities": list(capabilities),
    }


def make_inbound(event: InboundEvent) -> dict[str, Any]:
    return {
        "type": INBOUND,
        "event_id": event.event_id,
        "room_id": event.room_id,
        "sender": event.sender,
        "text": event.text,
        "media": [m.to_dict() for m in event.media],
        "mentions": list(event.mentions),
        "thread_root": event.thread_root,
        "ts": event.ts,
    }


def frame_to_inbound(frame: dict[str, Any]) -> InboundEvent:
    kind = frame.get("kind")
    if kind is None:
        # Design sample frames carry no kind; derive it.
        kind = "media" if frame.get("media") else "text"
    return InboundEvent(
        event_id=frame["event_id"],
        room_id=frame["room_id"],
        sender=frame["sender"],
        ts=frame["ts"],
        text=frame["text"],
        media=[MediaRef.from_dict(m) for m in frame["media"]],
        mentions=list(frame["mentions"]),
        thread_root=frame["thread_root"],
        kind=kind,
    )


def make_outbound(msg: OutboundMessage) -> dict[str, Any]:
    return {
        "type": OUTBOUND,
        "room_id": msg.room_id,
        "text": msg.text,
        "reply_to": msg.reply_to,
        "kind": msg.kind,
        "media": [m.to_dict() for m in msg.media],
    }


def frame_to_outbound(frame: dict[str, Any]) -> OutboundMessage:
    return OutboundMessage(
        room_id=frame["room_id"],
        text=frame["text"],
        reply_to=frame["reply_to"],
        kind=frame["kind"],
        media=[MediaRef.from_dict(m) for m in frame["media"]],
    )


def make_typing(room_id: str, state: str) -> dict[str, Any]:
    if state not in TYPING_STATES:
        raise SidecarProtocolError(
            f"typing.state must be one of {TYPING_STATES}, got {state!r}", "invalid_frame"
        )
    return {"type": TYPING, "room_id": room_id, "state": state}


def make_ack(event_id: str) -> dict[str, Any]:
    """Ack frame; an empty ``event_id`` acknowledges the hello handshake."""
    return {"type": ACK, "event_id": event_id}


def make_error(code: str, message: str) -> dict[str, Any]:
    return {"type": ERROR, "code": code, "message": message}


# ---------------------------------------------------------------------------
# Line reader (shared by server and client)
# ---------------------------------------------------------------------------


class LineReader:
    """Per-connection newline framer. Bytes after the first line are retained."""

    def __init__(self) -> None:
        self._buffer = bytearray()

    def read_line(self, sock: socket.socket) -> bytes:
        """Read one ``\\n``-terminated line, enforcing :data:`MAX_LINE_BYTES`.

        Returns the line **without** the trailing newline.  Bytes that
        arrive beyond the first newline stay buffered for the next
        ``read_line`` call (coalesced frames), so one connection always
        uses one reader.  A clean EOF before any data returns ``b""``;
        an EOF mid-frame is a protocol error.
        """
        while True:
            newline = self._buffer.find(b"\n")
            if newline != -1:
                line = bytes(self._buffer[:newline])
                del self._buffer[: newline + 1]
                if len(line) > MAX_LINE_BYTES:
                    raise SidecarProtocolError(
                        f"line exceeds the {MAX_LINE_BYTES} byte limit",
                        "line_exceeds_limit",
                    )
                return line
            if len(self._buffer) > MAX_LINE_BYTES:
                raise SidecarProtocolError(
                    f"line exceeds the {MAX_LINE_BYTES} byte limit",
                    "line_exceeds_limit",
                )
            chunk = sock.recv(65536)
            if not chunk:
                if self._buffer:
                    raise SidecarProtocolError(
                        "connection closed mid-frame", "connection_closed"
                    )
                return b""
            self._buffer.extend(chunk)


# ---------------------------------------------------------------------------
# Server
# ---------------------------------------------------------------------------


class SidecarServer:
    """Single-connection sidecar server over a 0600 Unix socket.

    * The socket's parent directory is created if missing; the socket
      itself is ``chmod 0600``.
    * At most one connection is served at a time.  A second concurrent
      connection is rejected (``connection_limit`` error frame + close)
      and logged.
    * Handshake failure (first frame not ``hello``, protocol version
      mismatch, oversized line, …) closes the connection and
      fail-closes: the server does not serve that peer and there is no
      native fallback.
    * After a successful handshake the server sends an ``ack`` with an
      empty ``event_id`` (handshake ack).
    * A new connection may be accepted again after the previous one
      closed (sidecar restarts must not re-consume: the sync token is
      persisted, not in-flight).
    """

    def __init__(self, socket_path: Union[str, Path]) -> None:
        self._socket_path = Path(socket_path)
        self._listener: socket.socket | None = None
        self._conn: socket.socket | None = None
        self._reader: LineReader | None = None
        self._state_lock = threading.Lock()
        self._send_lock = threading.Lock()
        self._connected = threading.Event()
        self._stopped = threading.Event()
        self._accept_thread: threading.Thread | None = None
        self.agent: str | None = None
        self.capabilities: list[str] = []

    # -- lifecycle ---------------------------------------------------------

    @property
    def socket_path(self) -> Path:
        return self._socket_path

    @property
    def connected(self) -> bool:
        with self._state_lock:
            return self._conn is not None

    def start(self) -> None:
        """Bind the socket (0600) and start the accept loop."""
        if self._listener is not None:
            raise RuntimeError("sidecar server already started")
        self._socket_path.parent.mkdir(parents=True, exist_ok=True)
        if self._socket_path.exists():
            mode = os.stat(str(self._socket_path)).st_mode
            if not stat.S_ISSOCK(mode):
                raise RuntimeError(
                    f"socket path exists and is not a socket: {self._socket_path}"
                )
            os.unlink(str(self._socket_path))
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.bind(str(self._socket_path))
        os.chmod(str(self._socket_path), SOCKET_MODE)
        listener.listen(2)
        self._listener = listener
        self._stopped.clear()
        self._accept_thread = threading.Thread(
            target=self._accept_loop, name="matrix-sidecar-accept", daemon=True
        )
        self._accept_thread.start()
        log.info("sidecar listening on %s (0600)", self._socket_path)

    def stop(self) -> None:
        """Close the listener and the active connection (idempotent)."""
        self._stopped.set()
        listener, self._listener = self._listener, None
        if listener is not None:
            try:
                listener.close()
            except OSError:
                pass
            # Remove the socket file on a clean stop.
            try:
                mode = os.stat(str(self._socket_path)).st_mode
                if stat.S_ISSOCK(mode):
                    os.unlink(str(self._socket_path))
            except OSError:
                pass
        self._close_connection()

    def wait_connected(self, timeout: float = 5.0) -> bool:
        """Wait until a peer completes the handshake (test/ops helper)."""
        return self._connected.wait(timeout)

    # -- framing -----------------------------------------------------------

    def send(self, frame: dict[str, Any]) -> None:
        """Send one frame to the active connection."""
        conn = self._active_connection()
        data = encode_frame(frame)
        with self._send_lock:
            conn.sendall(data)

    def recv(self) -> dict[str, Any]:
        """Read and validate one frame from the active connection."""
        conn, reader = self._active_frame_pair()
        line = reader.read_line(conn)
        if line == b"":
            self._close_connection()
            raise SidecarProtocolError(
                "sidecar connection closed by peer", "connection_closed"
            )
        return decode_line(line)

    # -- internals ----------------------------------------------------------

    def _active_frame_pair(self) -> tuple[socket.socket, LineReader]:
        """The active connection and its per-connection reader, atomically."""
        with self._state_lock:
            conn, reader = self._conn, self._reader
        if conn is None or reader is None:
            raise SidecarNotConnectedError("sidecar has no active connection")
        return conn, reader

    def _active_connection(self) -> socket.socket:
        return self._active_frame_pair()[0]

    def _close_connection(self) -> None:
        with self._state_lock:
            conn, self._conn = self._conn, None
            self._reader = None
        if conn is not None:
            try:
                conn.close()
            except OSError:
                pass
        self.agent = None
        self.capabilities = []
        self._connected.clear()

    def _accept_loop(self) -> None:
        assert self._listener is not None
        while not self._stopped.is_set():
            try:
                conn, _addr = self._listener.accept()
            except OSError:
                return  # listener closed
            try:
                self._handle_new_connection(conn)
            except Exception:  # never let the accept loop die
                log.exception("sidecar: failed to handle new connection")
                try:
                    conn.close()
                except OSError:
                    pass

    def _handle_new_connection(self, conn: socket.socket) -> None:
        with self._state_lock:
            already = self._conn is not None
        if already:
            log.warning(
                "sidecar: rejecting second concurrent connection "
                "(one agent = one connection)"
            )
            self._reject(conn, "connection_limit", "sidecar already has one connection")
            return
        # One reader per connection: it is created here and kept for the
        # lifetime of this connection (discarded in _close_connection),
        # so coalesced bytes survive the handshake read.
        reader = LineReader()
        try:
            conn.settimeout(HANDSHAKE_TIMEOUT)
            line = reader.read_line(conn)
            if line == b"":
                raise SidecarProtocolError(
                    "connection closed before handshake", "connection_closed"
                )
            frame = decode_line(line)
            if frame["type"] != HELLO:
                raise SidecarProtocolError(
                    f"first frame must be hello, got {frame['type']!r}",
                    "expected_hello",
                )
            if frame["protocol"] != PROTOCOL_VERSION:
                raise SidecarProtocolError(
                    f"protocol version mismatch: got {frame['protocol']}, "
                    f"want {PROTOCOL_VERSION} (fail-closed, no native fallback)",
                    "protocol_mismatch",
                )
        except SidecarProtocolError as exc:
            log.error("sidecar: handshake failed, fail-closed: %s", exc)
            self._reject(conn, exc.code, str(exc))
            return
        except socket.timeout:
            log.error("sidecar: handshake timed out, fail-closed")
            self._reject(conn, "handshake_timeout", "no hello within handshake timeout")
            return
        with self._state_lock:
            if self._conn is not None:  # lost a race with another accept
                conn.close()
                return
            self._conn = conn
            self._reader = reader
            self.agent = frame["agent"]
            self.capabilities = list(frame["capabilities"])
        conn.settimeout(None)
        self._connected.set()
        log.info(
            "sidecar: agent %s connected (capabilities=%s)",
            frame["agent"],
            frame["capabilities"],
        )
        # Handshake ack: empty event_id means "hello accepted".
        try:
            with self._send_lock:
                conn.sendall(encode_frame(make_ack("")))
        except OSError as exc:
            log.error("sidecar: failed to send handshake ack: %s", exc)
            self._close_connection()

    @staticmethod
    def _reject(conn: socket.socket, code: str, message: str) -> None:
        try:
            conn.sendall(encode_frame(make_error(code, message)))
        except OSError:
            pass
        try:
            conn.close()
        except OSError:
            pass


# ---------------------------------------------------------------------------
# Client (test support in M1; the PR2 TypeScript client is the production one)
# ---------------------------------------------------------------------------


class SidecarClient:
    """Minimal blocking sidecar client (handshake + framed send/recv)."""

    def __init__(
        self,
        socket_path: Union[str, Path],
        handshake_timeout: float = HANDSHAKE_TIMEOUT,
    ) -> None:
        self._socket_path = Path(socket_path)
        self._handshake_timeout = handshake_timeout
        self._sock: socket.socket | None = None
        self._reader: LineReader | None = None

    @property
    def connected(self) -> bool:
        return self._sock is not None

    def connect(
        self,
        agent: str,
        protocol: int = PROTOCOL_VERSION,
        capabilities: tuple = ("text",),
    ) -> None:
        """Connect and complete the hello handshake.

        Raises :class:`SidecarProtocolError` when the server rejects the
        handshake (version mismatch, non-hello handling, second
        connection, …) — fail-closed, no fallback.
        """
        if self._sock is not None:
            raise RuntimeError("client already connected")
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        # One reader per connection: created here and stored with the
        # socket, so coalesced bytes survive the handshake read.
        reader = LineReader()
        try:
            sock.connect(str(self._socket_path))
            sock.sendall(encode_frame(make_hello(agent, protocol, capabilities)))
            sock.settimeout(self._handshake_timeout)
            line = reader.read_line(sock)
            if line == b"":
                raise SidecarProtocolError(
                    "sidecar closed the connection during handshake",
                    "connection_closed",
                )
            frame = decode_line(line)
            if frame["type"] == ACK and frame["event_id"] == "":
                pass  # handshake accepted
            elif frame["type"] == ERROR:
                raise SidecarProtocolError(
                    f"sidecar rejected handshake: {frame['code']}: {frame['message']}",
                    frame["code"],
                )
            else:
                raise SidecarProtocolError(
                    f"unexpected frame during handshake: {frame.get('type')!r}",
                    "unexpected_frame",
                )
        except BaseException:
            sock.close()
            raise
        sock.settimeout(None)
        self._sock = sock
        self._reader = reader

    def send(self, frame: dict[str, Any]) -> None:
        sock = self._active_socket()
        sock.sendall(encode_frame(frame))

    def recv(self) -> dict[str, Any]:
        sock = self._active_socket()
        line = self._active_reader().read_line(sock)
        if line == b"":
            self.close()
            raise SidecarProtocolError(
                "sidecar connection closed by peer", "connection_closed"
            )
        return decode_line(line)

    def close(self) -> None:
        if self._sock is not None:
            try:
                self._sock.close()
            except OSError:
                pass
            self._sock = None
        self._reader = None

    def _active_socket(self) -> socket.socket:
        if self._sock is None:
            raise SidecarNotConnectedError("client is not connected")
        return self._sock

    def _active_reader(self) -> LineReader:
        if self._reader is None:
            raise SidecarNotConnectedError("client is not connected")
        return self._reader
