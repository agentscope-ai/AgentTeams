"""MatrixCore loop tests with a fake transport (P9, PR1 M1).

Covers: events injected by the transport reach the bridge; bridge output
goes through ``transport.send``; the single-writer lock is fail-closed
for a second core and released after ``stop``; errors are recorded
without blocking the loop.
"""

from __future__ import annotations

import asyncio
import contextlib

import pytest

from core.core import MatrixCore
from core.interfaces import CoreConfig, InboundEvent, OutboundMessage
from core.state import CoreLockedError, FileSessionStore, SingleWriterLock

E1 = InboundEvent(event_id="$e1", room_id="!room", sender="@alice", ts=1, text="hello")
E2 = InboundEvent(event_id="$e2", room_id="!room", sender="@bob", ts=2, text="reply: pong")


class FakeTransport:
    """In-memory transport: injects *events* during start()."""

    def __init__(self, events=()):
        self.events = list(events)
        self.started = False
        self.stopped = False
        self.sent = []
        self.send_error = None

    async def start(self, on_event):
        self.started = True
        for event in self.events:
            await on_event(event)

    async def stop(self):
        self.stopped = True

    async def send(self, msg):
        if self.send_error is not None:
            raise self.send_error
        self.sent.append(msg)
        return f"$sent{len(self.sent)}"

    async def edit(self, room_id, event_id, text):
        raise NotImplementedError

    async def react(self, room_id, event_id, key):
        raise NotImplementedError

    async def typing(self, room_id, state):
        raise NotImplementedError

    async def download(self, media):
        return b""


class FakeBridge:
    """Bridge that records events; ``reply:`` text triggers core.submit."""

    def __init__(self, core=None):
        self._core = core
        self.received = []
        self.outputs = []
        self.fail_next_inbound = False

    async def on_inbound(self, event):
        self.received.append(event)
        if self.fail_next_inbound:
            self.fail_next_inbound = False
            raise RuntimeError("bridge boom")
        if event.text.startswith("reply:"):
            assert self._core is not None
            await self._core.submit(
                OutboundMessage(
                    room_id=event.room_id,
                    text=event.text[len("reply:"):].strip(),
                    kind="final",
                )
            )

    async def on_runtime_output(self, msg):
        self.outputs.append(msg)


async def _wait_until(predicate, timeout=5.0):
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while not predicate():
        if loop.time() > deadline:
            raise TimeoutError("condition not met in time")
        await asyncio.sleep(0.01)


def _build(tmp_path, transport, bridge, config=None):
    state_dir = tmp_path / "state"
    if config is None:
        config = CoreConfig(state_dir=state_dir, agent_id="worker-a")
    store = FileSessionStore(state_dir)
    core = MatrixCore(transport, store, bridge, config)
    bridge._core = core
    return core, config


def _run_scenario(scenario):
    """Run an async scenario coroutine to completion (no pytest-asyncio)."""
    return asyncio.run(scenario)


def test_events_reach_bridge_and_output_reaches_transport(tmp_path):
    async def scenario():
        transport = FakeTransport(events=[E1, E2])
        bridge = FakeBridge()
        core, config = _build(tmp_path, transport, bridge)
        task = asyncio.ensure_future(core.run())
        try:
            await _wait_until(lambda: transport.started and len(bridge.received) == 2)
            assert bridge.received == [E1, E2]
            # E2 ("reply: pong") made the bridge submit -> transport.send.
            assert [m.text for m in transport.sent] == ["pong"]
            assert transport.sent[0].kind == "final"
            assert transport.sent[0].room_id == "!room"
            # Core callback: the bridge saw its own output after the send.
            assert bridge.outputs == [transport.sent[0]]
            # State directory tree was created under the config.
            assert (config.state_dir / "e2ee").is_dir()
            assert (config.state_dir / "media_cache").is_dir()
            await core.stop()
        finally:
            if not task.done():
                with contextlib.suppress(TimeoutError):
                    await _wait_until(task.done, timeout=2.0)
                if not task.done():
                    task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await task
        # stop() completed run() cleanly.
        assert task.result() is None
        assert transport.stopped
        assert not core.running

    _run_scenario(scenario())
    # The single-writer lock is released after stop.
    lock = SingleWriterLock((tmp_path / "state") / "core.lock")
    lock.acquire()
    lock.release()


def test_second_core_fails_closed_while_first_running(tmp_path):
    async def scenario():
        transport = FakeTransport(events=[E1])
        bridge = FakeBridge()
        core, config = _build(tmp_path, transport, bridge)
        task = asyncio.ensure_future(core.run())
        try:
            await _wait_until(lambda: transport.started)
            # A second core on the same state dir must fail closed.
            intruder = MatrixCore(
                FakeTransport(),
                FileSessionStore(config.state_dir),
                FakeBridge(),
                config,
            )
            with pytest.raises(CoreLockedError):
                await intruder.run()
            assert not intruder.running
            # The first core keeps running.
            assert core.running
            await core.stop()
        finally:
            if not task.done():
                with contextlib.suppress(TimeoutError):
                    await _wait_until(task.done, timeout=2.0)
                if not task.done():
                    task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await task

    _run_scenario(scenario())


def test_lock_released_after_stop(tmp_path):
    async def scenario():
        transport = FakeTransport()
        bridge = FakeBridge()
        core, config = _build(tmp_path, transport, bridge)
        task = asyncio.ensure_future(core.run())
        try:
            await _wait_until(lambda: transport.started)
            # While running, the lock is held.
            with pytest.raises(CoreLockedError):
                SingleWriterLock(config.state_dir / "core.lock").acquire()
            await core.stop()
        finally:
            if not task.done():
                with contextlib.suppress(TimeoutError):
                    await _wait_until(task.done, timeout=2.0)
                if not task.done():
                    task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await task

    _run_scenario(scenario())
    lock = SingleWriterLock((tmp_path / "state") / "core.lock")
    lock.acquire()
    assert lock.held
    lock.release()
    assert not lock.held


def test_bridge_error_recorded_and_loop_continues(tmp_path):
    errors = []

    async def scenario():
        transport = FakeTransport(events=[E1, E2])
        bridge = FakeBridge()
        bridge.fail_next_inbound = True
        core, _config = _build(tmp_path, transport, bridge)
        core._on_error = lambda context, exc: errors.append((context, exc))
        task = asyncio.ensure_future(core.run())
        try:
            # Both events are delivered even though the first handler blew up.
            await _wait_until(lambda: len(bridge.received) == 2)
            assert bridge.received == [E1, E2]
            assert core.last_error is not None
            assert "on_inbound" in core.last_error
            assert errors and errors[0][0] == "on_inbound"
            assert isinstance(errors[0][1], RuntimeError)
            await core.stop()
        finally:
            if not task.done():
                with contextlib.suppress(TimeoutError):
                    await _wait_until(task.done, timeout=2.0)
                if not task.done():
                    task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await task

    _run_scenario(scenario())


def test_submit_failure_recorded_and_raised(tmp_path):
    async def scenario():
        transport = FakeTransport(events=[E1])
        transport.send_error = OSError("send refused")
        bridge = FakeBridge()
        core, _config = _build(tmp_path, transport, bridge)
        task = asyncio.ensure_future(core.run())
        try:
            await _wait_until(lambda: transport.started)
            with pytest.raises(OSError, match="send refused"):
                await core.submit(OutboundMessage(room_id="!room", text="x"))
            assert core.last_error is not None and "submit" in core.last_error
            assert bridge.outputs == []  # no output callback on failed send
            await core.stop()
        finally:
            if not task.done():
                with contextlib.suppress(TimeoutError):
                    await _wait_until(task.done, timeout=2.0)
                if not task.done():
                    task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await task

    _run_scenario(scenario())


def test_submit_before_run_raises_and_stop_is_noop(tmp_path):
    async def scenario():
        transport = FakeTransport()
        bridge = FakeBridge()
        core, _config = _build(tmp_path, transport, bridge)
        with pytest.raises(RuntimeError):
            await core.submit(OutboundMessage(room_id="!room", text="x"))
        await core.stop()  # idempotent no-op while not running
        assert not transport.stopped

    _run_scenario(scenario())
