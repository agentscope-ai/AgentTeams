"""The production runtime (sidecar/app.py) against the fake core.

Covers: build() wiring (0600 socket, 0700 parent dir), the hello
handshake (agent name reaches the server), inbound dedupe/echo over a
real socket, the outbound -> core.send -> ack round trip, and a full
SIGTERM lifecycle of ``python -m sidecar`` (clean exit, socket removed).
"""

from __future__ import annotations

import asyncio
import json
import os
import signal
import socket
import stat
import subprocess
import sys
import threading
import time

import pytest

from sidecar import _core, load_config
from sidecar.app import build, shutdown

PLUGIN_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))


def make_config(tmp_path, **overrides):
    data = {
        "homeserver_url": "https://matrix.unit.test",
        "agent_user": "@unit:unit.test",
        "agent_access_token": "unit-token",
        "socket_path": str(tmp_path / "run" / "sidecar.sock"),
        "agent_name": "unit-agent",
    }
    data.update(overrides)
    cfg_file = tmp_path / "sidecar-config.json"
    cfg_file.write_text(json.dumps(data) + "\n", encoding="utf-8")
    os.chmod(cfg_file, 0o600)
    return str(cfg_file)


def connect_client(sock_path, agent="unit-agent", timeout=5.0):
    """A raw Unix-socket client: connect + hello handshake.

    Returns (sock, handshake_frame).
    """
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    s.connect(str(sock_path))
    hello = {"type": "hello", "protocol": 1, "agent": agent, "capabilities": ["text"]}
    s.sendall((json.dumps(hello) + "\n").encode())
    buf = b""
    while not buf.endswith(b"\n"):
        chunk = s.recv(65536)
        if not chunk:
            raise RuntimeError("server closed the connection during the handshake")
        buf += chunk
    return s, json.loads(buf.decode().rstrip("\n"))


def recv_frame(sock, timeout=5.0):
    sock.settimeout(timeout)
    buf = b""
    while not buf.endswith(b"\n"):
        chunk = sock.recv(65536)
        if not chunk:
            raise RuntimeError("connection closed before the frame arrived")
        buf += chunk
    return json.loads(buf.decode().rstrip("\n"))


def wait_until(predicate, timeout=5.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.05)
    return False


@pytest.fixture()
def config_path(tmp_path):
    return make_config(tmp_path)


@pytest.fixture()
def runtime(config_path):
    rt = build(load_config(config_path))
    yield rt
    # deterministic teardown whatever the test left running
    shutdown(rt, threading.Event(), timeout=5)


def _start_core(runtime):
    runtime.core_task = asyncio.run_coroutine_threadsafe(runtime.core.run(), runtime.loop)
    assert runtime.transport.started.wait(5), "fake transport did not start"


# --- build() wiring -----------------------------------------------------------------


def test_socket_is_0600_and_parent_dir_0700(runtime, config_path):
    cfg = load_config(config_path)
    st = os.stat(cfg.socket_path)
    assert stat.S_ISSOCK(st.st_mode)
    assert stat.S_IMODE(st.st_mode) == 0o600
    parent_mode = stat.S_IMODE(os.stat(os.path.dirname(cfg.socket_path)).st_mode)
    assert parent_mode == 0o700


def test_handshake_ack_and_agent_name(runtime, config_path):
    cfg = load_config(config_path)
    sock, frame = connect_client(cfg.socket_path, agent="unit-agent")
    try:
        assert frame == {"type": "ack", "event_id": ""}
        assert wait_until(lambda: runtime.server.connected)
        assert runtime.server.agent == "unit-agent"
        assert runtime.server.capabilities == ["text"]
    finally:
        sock.close()


# --- inbound through the real socket -------------------------------------------------


def test_inbound_dedupe_over_socket(runtime, config_path):
    cfg = load_config(config_path)
    _start_core(runtime)
    sock, _ = connect_client(cfg.socket_path)
    try:
        event = _core.load_core().core.InboundEvent(
            event_id="$dup-1",
            room_id="!room:unit.test",
            sender="@sender:unit.test",
            ts=123,
            text="hello there",
        )
        # two deliveries of the same event at the adapter boundary
        for _ in range(2):
            asyncio.run_coroutine_threadsafe(
                runtime.transport.on_event(event), runtime.loop
            ).result(5)
        frame = recv_frame(sock)
        assert frame["type"] == "inbound"
        assert frame["event_id"] == "$dup-1"
        assert frame["sender"] == "@sender:unit.test"
        assert frame["text"] == "hello there"
        # the second delivery produced no second frame
        with pytest.raises(socket.timeout):
            recv_frame(sock, timeout=1.0)
        assert runtime.bridge.dropped == 1
        assert runtime.bridge.forwarded == 1
    finally:
        sock.close()


def test_own_echo_is_suppressed_over_socket(runtime, config_path):
    cfg = load_config(config_path)
    _start_core(runtime)
    sock, _ = connect_client(cfg.socket_path)
    try:
        event = _core.load_core().core.InboundEvent(
            event_id="$echo-1",
            room_id="!room:unit.test",
            sender=cfg.agent_user,  # the agent's own MXID
            ts=124,
            text="my own reply echoing back",
        )
        asyncio.run_coroutine_threadsafe(
            runtime.transport.on_event(event), runtime.loop
        ).result(5)
        with pytest.raises(socket.timeout):
            recv_frame(sock, timeout=1.0)  # nothing must be forwarded
        assert runtime.bridge.suppressed == 1
        assert runtime.bridge.forwarded == 0
    finally:
        sock.close()


# --- outbound -> core.send -> ack ----------------------------------------------------


def test_outbound_ack_uses_the_core_returned_event_id(runtime, config_path):
    cfg = load_config(config_path)
    _start_core(runtime)
    runtime.worker.start()
    sock, _ = connect_client(cfg.socket_path)
    try:
        outbound = {
            "type": "outbound",
            "room_id": "!room:unit.test",
            "text": "ping from the gateway",
            "reply_to": "$orig-1",
            "kind": "final",
            "media": [],
        }
        sock.sendall((json.dumps(outbound) + "\n").encode())
        ack = recv_frame(sock, timeout=5.0)
        assert ack == {"type": "ack", "event_id": "$fake-1"}
        sent = runtime.transport.sent[0]
        assert sent.room_id == "!room:unit.test"
        assert sent.text == "ping from the gateway"
        assert sent.reply_to == "$orig-1"
    finally:
        sock.close()


# --- full process lifecycle: SIGTERM --------------------------------------------------


def test_sigterm_stops_the_process_and_removes_the_socket(tmp_path):
    """python -m sidecar as a real child process: start, SIGTERM, clean exit."""
    cfg_file = make_config(tmp_path)
    sock_path = str(tmp_path / "run" / "sidecar.sock")

    env = dict(os.environ)
    env["PYTHONPATH"] = PLUGIN_ROOT
    env["MATRIX_CORE_DIR"] = os.environ["MATRIX_CORE_DIR"]  # the in-tree fake core
    env.pop("AGENTTEAMS_MATRIX_SIDECAR_CONFIG", None)

    proc = subprocess.Popen(
        [sys.executable, "-m", "sidecar", "--config", cfg_file],
        env=env,
        cwd=str(tmp_path),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    try:
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if os.path.exists(sock_path):
                break
            if proc.poll() is not None:
                out = proc.stdout.read().decode(errors="replace")
                raise AssertionError(f"sidecar exited early (rc={proc.returncode}):\n{out}")
            time.sleep(0.1)
        else:
            raise AssertionError("socket never appeared")

        proc.send_signal(signal.SIGTERM)
        rc = proc.wait(timeout=20)
        output = proc.stdout.read().decode(errors="replace")
        assert rc == 0, output
        assert not os.path.exists(sock_path), "socket file must be removed on shutdown"
    finally:
        if proc.poll() is None:
            proc.kill()
        proc.wait(timeout=10)
