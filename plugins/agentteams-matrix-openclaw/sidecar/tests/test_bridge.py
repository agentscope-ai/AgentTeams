"""The shared bridge/worker semantics (no real homeserver needed).

AgentBridge: own-echo suppression, event-id dedupe, forward shape, and
on_runtime_output being log-only. run_outbound_worker: acks with the
event id the core returns, never acks a failed submit, retries the
"no active connection" window, ignores typing frames, and returns when
the connection ends.
"""

from __future__ import annotations

import asyncio
import threading
import time

import pytest

from sidecar import _core
from sidecar.bridge import AgentBridge, run_outbound_worker

AGENT = "@agent:example.com"
OTHER = "@other:example.com"


def lib():
    return _core.load_core()


def quiet():
    def log(_msg):
        pass

    return log


class RecordingServer:
    """Duck-typed SidecarServer: records sends, has no connection."""

    def __init__(self):
        self.sent = []

    def send(self, frame):
        self.sent.append(frame)

    def recv(self):
        raise RuntimeError("sidecar has no active connection")


class ScriptedServer:
    """Duck-typed SidecarServer: pops a scripted recv() sequence.

    Entries are frame dicts or Exception instances (re-raised).
    """

    def __init__(self, script):
        self._script = list(script)
        self.sent = []

    def send(self, frame):
        self.sent.append(frame)

    def recv(self):
        item = self._script.pop(0)
        if isinstance(item, Exception):
            raise item
        return item


def make_event(event_id="$1", sender=OTHER, text="hi"):
    return lib().core.InboundEvent(
        event_id=event_id,
        room_id="!room:example.com",
        sender=sender,
        ts=111,
        text=text,
    )


def run(coro):
    return asyncio.run(coro)


# --- AgentBridge -----------------------------------------------------------------


def test_inbound_is_forwarded_as_make_inbound_frame():
    server = RecordingServer()
    bridge = AgentBridge(server, AGENT, log=quiet())
    event = make_event()
    run(bridge.on_inbound(event))
    assert server.sent == [lib().core.make_inbound(event)]
    assert bridge.forwarded == 1
    assert bridge.dropped == 0
    assert bridge.last_event is event


def test_duplicate_event_id_is_dropped_once():
    server = RecordingServer()
    bridge = AgentBridge(server, AGENT, log=quiet())
    event = make_event()
    run(bridge.on_inbound(event))
    run(bridge.on_inbound(event))  # same event, second delivery
    run(bridge.on_inbound(event))  # ...and a third
    assert len(server.sent) == 1
    assert bridge.dropped == 2
    assert bridge.forwarded == 1


def test_own_echo_is_suppressed_and_not_forwarded():
    server = RecordingServer()
    bridge = AgentBridge(server, AGENT, log=quiet())
    run(bridge.on_inbound(make_event(event_id="$echo-1", sender=AGENT)))
    assert server.sent == []
    assert bridge.suppressed == 1
    assert bridge.forwarded == 0
    assert bridge.dropped == 0


def test_on_runtime_output_is_log_only():
    lines = []
    server = RecordingServer()
    bridge = AgentBridge(server, AGENT, log=lines.append)
    msg = lib().core.OutboundMessage(room_id="!room:example.com", text="hello", kind="final")
    run(bridge.on_runtime_output(msg))
    assert server.sent == []  # no frame goes to the sidecar for output
    assert any("runtime output delivered" in line for line in lines)


def test_log_sink_receives_dedupe_and_forward_lines():
    lines = []
    server = RecordingServer()
    bridge = AgentBridge(server, AGENT, log=lines.append)
    event = make_event(event_id="$dedupe-me")
    run(bridge.on_inbound(event))
    run(bridge.on_inbound(event))
    assert any("forwarded inbound $dedupe-me" in line for line in lines)
    assert any("dedupe: dropping duplicate delivery of $dedupe-me" in line for line in lines)


# --- run_outbound_worker -----------------------------------------------------------


def start_core_and_loop():
    """Fake core on its own event loop, started and running."""
    core_module, transport_module = lib().core, lib().transport
    transport = transport_module.NioTransport(
        transport_module.TransportConfig(
            homeserver="https://matrix.example.com",
            user_id=AGENT,
            access_token="unit-token",
            encryption=False,
        ),
        store=object(),
        state_dir="/tmp/bridge-test-state",
        encryption=False,
    )
    bridge = AgentBridge(RecordingServer(), AGENT, log=quiet())
    core = core_module.MatrixCore(
        transport,
        object(),
        bridge,
        core_module.CoreConfig("/tmp/bridge-test-state", "unit-agent"),
    )
    loop = asyncio.new_event_loop()
    loop_thread = threading.Thread(target=loop.run_forever, daemon=True)
    loop_thread.start()
    core_task = asyncio.run_coroutine_threadsafe(core.run(), loop)
    if not transport.started.wait(5):
        raise AssertionError("fake transport did not start")
    return core, transport, loop, loop_thread, core_task


def stop_core_and_loop(core, loop, loop_thread, core_task):
    asyncio.run_coroutine_threadsafe(core.stop(), loop).result(5)
    core_task.result(5)
    loop.call_soon_threadsafe(loop.stop)
    loop_thread.join(5)


OUTBOUND_FRAME = {
    "type": "outbound",
    "room_id": "!room:example.com",
    "text": "hello from the gateway",
    "reply_to": "$original-1",
    "kind": "final",
    "media": [],
}


def test_worker_acks_with_the_event_id_the_core_returns():
    server = ScriptedServer(
        [OUTBOUND_FRAME, RuntimeError("sidecar connection closed by peer")]
    )
    core, transport, loop, loop_thread, core_task = start_core_and_loop()
    try:
        run_outbound_worker(server, core, loop, log=quiet())
    finally:
        stop_core_and_loop(core, loop, loop_thread, core_task)
    assert server.sent == [{"type": "ack", "event_id": "$fake-1"}]
    sent = transport.sent[0]
    assert sent.room_id == OUTBOUND_FRAME["room_id"]
    assert sent.text == OUTBOUND_FRAME["text"]
    assert sent.reply_to == OUTBOUND_FRAME["reply_to"]
    assert sent.kind == "final"


def test_worker_retries_no_active_connection_then_exits_on_close():
    # The Pi incident shape: the worker starts before the gateway
    # connects (recv raises "no active connection"), later the
    # connection ends — the worker must have kept alive through the
    # first phase and returned (not raised) on the close.
    started = time.monotonic()
    server = ScriptedServer(
        [
            RuntimeError("sidecar has no active connection"),
            OUTBOUND_FRAME,
            RuntimeError("sidecar connection closed by peer"),
        ]
    )
    core, transport, loop, loop_thread, core_task = start_core_and_loop()
    try:
        run_outbound_worker(server, core, loop, log=quiet())
    finally:
        stop_core_and_loop(core, loop, loop_thread, core_task)
    # it got past the no-connection window (>= one 0.2s sleep)
    assert time.monotonic() - started >= 0.2
    assert server.sent == [{"type": "ack", "event_id": "$fake-1"}]
    assert len(transport.sent) == 1


def test_worker_does_not_ack_a_failed_submit_and_returns():
    core_module, transport_module = lib().core, lib().transport

    class FailingTransport(transport_module.NioTransport):
        async def send(self, msg):
            raise RuntimeError("homeserver rejected the send")

    transport = FailingTransport(
        transport_module.TransportConfig(
            homeserver="https://matrix.example.com",
            user_id=AGENT,
            access_token="unit-token",
        ),
        store=object(),
        state_dir="/tmp/bridge-test-state",
        encryption=False,
    )
    bridge = AgentBridge(RecordingServer(), AGENT, log=quiet())
    core = core_module.MatrixCore(
        transport,
        object(),
        bridge,
        core_module.CoreConfig("/tmp/bridge-test-state", "unit-agent"),
    )
    loop = asyncio.new_event_loop()
    loop_thread = threading.Thread(target=loop.run_forever, daemon=True)
    loop_thread.start()
    core_task = asyncio.run_coroutine_threadsafe(core.run(), loop)
    transport.started.wait(5)
    try:
        server = ScriptedServer([OUTBOUND_FRAME])
        run_outbound_worker(server, core, loop, log=quiet())  # returns, no raise
    finally:
        stop_core_and_loop(core, loop, loop_thread, core_task)
    assert server.sent == []  # no ack: a fabricated one would desync the FIFO
    assert core.last_error is not None and "submit" in core.last_error


def test_worker_ignores_typing_frames():
    server = ScriptedServer(
        [
            {"type": "typing", "room_id": "!room:example.com", "state": "start"},
            RuntimeError("sidecar connection closed by peer"),
        ]
    )
    core, transport, loop, loop_thread, core_task = start_core_and_loop()
    try:
        run_outbound_worker(server, core, loop, log=quiet())
    finally:
        stop_core_and_loop(core, loop, loop_thread, core_task)
    assert server.sent == []
    assert transport.sent == []
