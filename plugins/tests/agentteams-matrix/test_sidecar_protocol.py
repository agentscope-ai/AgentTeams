"""Sidecar protocol tests: codec roundtrips, handshake fail-closed
behaviour, single-connection enforcement, size limits (P9, PR1 M1).
"""

from __future__ import annotations

import json
import os
import socket
import threading

import pytest

from core.interfaces import InboundEvent, MediaRef, OutboundMessage
from core.sidecar import (
    MAX_LINE_BYTES,
    PROTOCOL_VERSION,
    LineReader,
    SidecarClient,
    SidecarNotConnectedError,
    SidecarProtocolError,
    SidecarServer,
    decode_line,
    encode_frame,
    frame_to_inbound,
    frame_to_outbound,
    make_ack,
    make_error,
    make_hello,
    make_inbound,
    make_outbound,
    make_typing,
)


@pytest.fixture()
def server(tmp_path):
    srv = SidecarServer(tmp_path / "sidecar.sock")
    srv.start()
    yield srv
    srv.stop()


# ---------------------------------------------------------------------------
# Raw-socket helpers (for handshake failure paths)
# ---------------------------------------------------------------------------


def _raw_connect(path):
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.connect(str(path))
    return sock


def _raw_read_line(sock) -> bytes:
    buf = b""
    while b"\n" not in buf:
        chunk = sock.recv(65536)
        if not chunk:
            break
        buf += chunk
    return buf.rstrip(b"\n")


# ---------------------------------------------------------------------------
# Codec: encode/decode roundtrips
# ---------------------------------------------------------------------------


def test_frame_roundtrip_all_types():
    event = InboundEvent(
        event_id="$e1",
        room_id="!room:local",
        sender="@alice:local",
        ts=1700000000,
        text="hello",
        media=[
            MediaRef(
                mxc_uri="mxc://local/png1",
                mime_type="image/png",
                file_name="p.png",
                size=3,
            )
        ],
        mentions=["@bot:local"],
        thread_root="$root:local",
        kind="media",
    )
    frame = make_inbound(event)
    assert decode_line(encode_frame(frame).decode()) == frame

    # Wire frames per the design sample carry no kind; it is derived.
    wire = {k: v for k, v in frame.items() if k != "kind"}
    back = frame_to_inbound(decode_line(encode_frame(wire)))
    assert back == event
    assert back.kind == "media"

    text_event = InboundEvent(
        event_id="$e2", room_id="!room:local", sender="@bob:local", ts=1, text="plain"
    )
    wire2 = {k: v for k, v in make_inbound(text_event).items() if k != "kind"}
    assert frame_to_inbound(decode_line(encode_frame(wire2))).kind == "text"

    msg = OutboundMessage(
        room_id="!room:local",
        text="out",
        reply_to="$e1",
        kind="tool",
        media=[MediaRef(mxc_uri="mxc://local/x2")],
    )
    oframe = make_outbound(msg)
    assert decode_line(encode_frame(oframe)) == oframe
    assert frame_to_outbound(oframe) == msg

    assert decode_line(encode_frame(make_typing("!r", "stop"))) == make_typing("!r", "stop")
    assert decode_line(encode_frame(make_ack("$e1"))) == {"type": "ack", "event_id": "$e1"}
    assert decode_line(encode_frame(make_error("code-x", "boom"))) == {
        "type": "error",
        "code": "code-x",
        "message": "boom",
    }
    hello = make_hello("worker-a", capabilities=("text", "media"))
    assert decode_line(encode_frame(hello)) == hello
    assert hello["protocol"] == PROTOCOL_VERSION == 1


def test_ack_and_error_serialization():
    assert json.loads(encode_frame(make_ack("$e"))) == {"type": "ack", "event_id": "$e"}
    assert json.loads(encode_frame(make_error("e2ee_failed", "decrypt failed"))) == {
        "type": "error",
        "code": "e2ee_failed",
        "message": "decrypt failed",
    }


def test_decode_hello_does_not_check_version():
    # Version checking is the server's job; the codec accepts any int.
    frame = make_hello("a", protocol=99)
    assert decode_line(encode_frame(frame))["protocol"] == 99


def test_encode_is_single_line():
    data = encode_frame(make_ack("$e"))
    assert data.endswith(b"\n")
    assert data.count(b"\n") == 1


# ---------------------------------------------------------------------------
# Codec: rejections
# ---------------------------------------------------------------------------


def test_decode_rejects_invalid_json():
    with pytest.raises(SidecarProtocolError) as exc:
        decode_line("not json")
    assert exc.value.code == "invalid_json"


def test_decode_rejects_non_object():
    with pytest.raises(SidecarProtocolError) as exc:
        decode_line("[1, 2]")
    assert exc.value.code == "invalid_frame"


def test_decode_rejects_unknown_type():
    with pytest.raises(SidecarProtocolError) as exc:
        decode_line('{"type": "bogus"}')
    assert exc.value.code == "unknown_type"


def test_decode_rejects_inbound_missing_ts():
    frame = make_inbound(
        InboundEvent(event_id="$e", room_id="!r", sender="@a", ts=0, text="t")
    )
    del frame["ts"]
    with pytest.raises(SidecarProtocolError) as exc:
        decode_line(json.dumps(frame))
    assert exc.value.code == "invalid_frame"


def test_decode_rejects_outbound_bad_kind():
    frame = make_outbound(OutboundMessage(room_id="!r", text="t"))
    frame["kind"] = "shout"
    with pytest.raises(SidecarProtocolError) as exc:
        decode_line(json.dumps(frame))
    assert exc.value.code == "invalid_frame"


def test_decode_rejects_typing_bad_state():
    frame = make_typing("!r", "start")
    frame["state"] = "pause"
    with pytest.raises(SidecarProtocolError) as exc:
        decode_line(json.dumps(frame))
    assert exc.value.code == "invalid_frame"


def test_decode_rejects_hello_string_protocol():
    with pytest.raises(SidecarProtocolError) as exc:
        decode_line('{"type": "hello", "protocol": "1", "agent": "a", "capabilities": []}')
    assert exc.value.code == "invalid_frame"


def test_decode_rejects_oversized_line():
    with pytest.raises(SidecarProtocolError) as exc:
        decode_line("a" * (MAX_LINE_BYTES + 1))
    assert exc.value.code == "line_exceeds_limit"


def test_encode_rejects_missing_type():
    with pytest.raises(SidecarProtocolError) as exc:
        encode_frame({"nope": 1})
    assert exc.value.code == "unknown_type"


def test_encode_rejects_oversized_frame():
    with pytest.raises(SidecarProtocolError) as exc:
        encode_frame({"type": "error", "code": "big", "message": "a" * 2_000_000})
    assert exc.value.code == "line_exceeds_limit"


# ---------------------------------------------------------------------------
# Server: handshake
# ---------------------------------------------------------------------------


def test_handshake_success_socket_mode_and_framing(server):
    client = SidecarClient(server.socket_path)
    client.connect("worker-a", capabilities=("text", "media", "e2ee", "threads"))
    try:
        assert client.connected
        assert server.wait_connected(2.0)
        assert server.agent == "worker-a"
        assert server.capabilities == ["text", "media", "e2ee", "threads"]
        assert os.stat(server.socket_path).st_mode & 0o777 == 0o600
        # Bidirectional framing works after the handshake.
        server.send(make_ack("$e9"))
        assert client.recv() == make_ack("$e9")
        msg = OutboundMessage(room_id="!room", text="hi", kind="final")
        client.send(make_outbound(msg))
        assert server.recv() == make_outbound(msg)
    finally:
        client.close()


def test_client_version_mismatch_fail_closed(server):
    client = SidecarClient(server.socket_path)
    with pytest.raises(SidecarProtocolError) as exc:
        client.connect("worker-a", protocol=PROTOCOL_VERSION + 1)
    assert exc.value.code == "protocol_mismatch"
    assert not client.connected
    assert not server.connected


def test_version_mismatch_error_frame_then_close(server):
    sock = _raw_connect(server.socket_path)
    try:
        sock.sendall(
            encode_frame(make_hello("worker-a", protocol=PROTOCOL_VERSION + 1))
        )
        frame = json.loads(_raw_read_line(sock))
        assert frame["type"] == "error"
        assert frame["code"] == "protocol_mismatch"
        # Fail-closed: the connection is closed, no native fallback.
        assert sock.recv(65536) == b""
        assert not server.connected
    finally:
        sock.close()


def test_first_frame_not_hello_fail_closed(server):
    sock = _raw_connect(server.socket_path)
    try:
        sock.sendall(encode_frame(make_typing("!room", "start")))
        frame = json.loads(_raw_read_line(sock))
        assert frame["type"] == "error"
        assert frame["code"] == "expected_hello"
        assert sock.recv(65536) == b""
        assert not server.connected
    finally:
        sock.close()


def test_invalid_json_first_frame_fail_closed(server):
    sock = _raw_connect(server.socket_path)
    try:
        sock.sendall(b"this is not json\n")
        frame = json.loads(_raw_read_line(sock))
        assert frame["type"] == "error"
        assert frame["code"] == "invalid_json"
        assert sock.recv(65536) == b""
    finally:
        sock.close()


def test_oversized_line_rejected(server):
    sock = _raw_connect(server.socket_path)
    try:
        payload = b'{"pad":"' + b"a" * MAX_LINE_BYTES + b'"}\n'
        sock.sendall(payload)
        frame = json.loads(_raw_read_line(sock))
        assert frame["type"] == "error"
        assert frame["code"] == "line_exceeds_limit"
        assert sock.recv(65536) == b""
    finally:
        sock.close()


# ---------------------------------------------------------------------------
# Server: single-connection enforcement
# ---------------------------------------------------------------------------


def test_second_concurrent_connection_rejected(server):
    first = SidecarClient(server.socket_path)
    first.connect("worker-a")
    try:
        second = SidecarClient(server.socket_path)
        with pytest.raises(SidecarProtocolError) as exc:
            second.connect("worker-b")
        assert exc.value.code == "connection_limit"
        assert not second.connected
        assert server.agent == "worker-a"
        # The first connection keeps working.
        server.send(make_ack("$x"))
        assert first.recv() == make_ack("$x")
    finally:
        first.close()


def test_new_connection_allowed_after_previous_closed(server):
    first = SidecarClient(server.socket_path)
    first.connect("worker-a")
    first.close()
    # The server detects the closed peer on the next read; force that so
    # the slot is freed before the reconnection below.
    try:
        with pytest.raises(SidecarProtocolError):
            server.recv()
    except SidecarNotConnectedError:
        pass
    second = SidecarClient(server.socket_path)
    second.connect("worker-b")
    try:
        assert server.agent == "worker-b"
    finally:
        second.close()


def test_send_recv_before_connection_raise(tmp_path):
    srv = SidecarServer(tmp_path / "sidecar.sock")
    srv.start()
    try:
        with pytest.raises(SidecarNotConnectedError):
            srv.send(make_ack("$x"))
        with pytest.raises(SidecarNotConnectedError):
            srv.recv()
    finally:
        srv.stop()


def test_server_recv_reports_peer_close(server):
    client = SidecarClient(server.socket_path)
    client.connect("worker-a")
    client.close()
    with pytest.raises(SidecarProtocolError) as exc:
        server.recv()
    assert exc.value.code == "connection_closed"
    assert not server.connected


# ---------------------------------------------------------------------------
# Server: socket path hygiene
# ---------------------------------------------------------------------------


def test_stale_non_socket_path_rejected(tmp_path):
    sock_path = tmp_path / "sidecar.sock"
    srv = SidecarServer(sock_path)
    srv.start()
    srv.stop()
    sock_path.write_text("not a socket")
    with pytest.raises(RuntimeError):
        SidecarServer(sock_path).start()
    sock_path.unlink()


def test_server_stop_is_idempotent(tmp_path):
    srv = SidecarServer(tmp_path / "sidecar.sock")
    srv.start()
    srv.stop()
    srv.stop()


# ---------------------------------------------------------------------------
# LineReader: per-connection buffering (coalesced and fragmented frames)
# ---------------------------------------------------------------------------


def test_client_recv_coalesced_frames_socketpair():
    """Maintainer reproduction: two ack frames in one sendall().

    With a stateless line reader the bytes after the first newline
    were discarded, so the second recv() would time out; the
    per-connection buffer must deliver both frames.
    """
    a, b = socket.socketpair()
    client = SidecarClient("/nonexistent.sock")
    client._sock = b
    client._reader = LineReader()
    try:
        a.sendall(encode_frame(make_ack("$first")) + encode_frame(make_ack("$second")))
        assert client.recv() == make_ack("$first")
        assert client.recv() == make_ack("$second")
    finally:
        client.close()
        a.close()


def test_server_handshake_and_following_frame_coalesced(server):
    """Hello + a second frame in one sendall(): both are processed."""
    sock = _raw_connect(server.socket_path)
    try:
        outbound = make_outbound(OutboundMessage(room_id="!room", text="after-hello"))
        sock.sendall(encode_frame(make_hello("worker-a")) + encode_frame(outbound))
        assert server.wait_connected(2.0)
        # The handshake ack was sent; the coalesced frame is still queued.
        assert json.loads(_raw_read_line(sock))["type"] == "ack"
        assert server.recv() == outbound
    finally:
        sock.close()


def test_read_line_fragmented_frame_waits_for_newline():
    a, b = socket.socketpair()
    try:
        frame = encode_frame(make_ack("$frag"))
        split = len(frame) // 2
        a.sendall(frame[:split])  # first half, no newline yet
        reader = LineReader()
        result = []
        errors = []

        def _read():
            try:
                result.append(reader.read_line(b))
            except Exception as exc:  # recorded, not swallowed
                errors.append(exc)

        worker = threading.Thread(target=_read, daemon=True)
        worker.start()
        worker.join(1.0)
        assert not result and not errors  # still blocked, no partial line
        a.sendall(frame[split:])
        worker.join(5.0)
        assert not errors
        assert result == [frame[:-1]]  # the full line, without the newline
    finally:
        a.close()
        b.close()


def test_read_line_per_line_limit_not_buffered_window():
    """Two lines, each under the limit, totalling over it: both are read.

    The sender runs in a thread: the combined payload is larger than
    the socket buffer, so a synchronous sendall would deadlock until
    the reader consumes the first line.
    """
    a, b = socket.socketpair()
    try:
        line1 = b"a" * (MAX_LINE_BYTES // 2 + 1)
        line2 = b"b" * (MAX_LINE_BYTES // 2 + 1)
        assert len(line1) + len(line2) > MAX_LINE_BYTES
        sent = []
        errors = []

        def _send():
            try:
                a.sendall(line1 + b"\n" + line2 + b"\n")
                sent.append(1)
            except OSError as exc:  # recorded, not swallowed
                errors.append(exc)

        sender = threading.Thread(target=_send, daemon=True)
        sender.start()
        reader = LineReader()
        assert reader.read_line(b) == line1
        assert reader.read_line(b) == line2
        sender.join(5.0)
        assert sent == [1] and not errors
    finally:
        a.close()
        b.close()
