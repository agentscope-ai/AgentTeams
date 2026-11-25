"""OutboundQueue (MX-1) tests: persistence, idempotent replay, backoff,
dead letter, room ordering, permissions, and the core delivery worker.

Acceptance list (MX-1 §4):
  1. same-key re-entry: payload replaced, generation+1, attempts reset,
     queue position kept
  2. Later delay never counts as an attempt; claimable only after due
  3. completion matches the (kind, key, seq, generation) quadruple
  4. per-kind concurrency cap: the (N+1)th waits
  5. exponential backoff sequences and caps ([2s, 60s] default,
     [5s, 300s] long-running kinds)
  6. 3 consecutive failures -> dead letter, never claimed again
  7. restart replay: pending rows replay, completed rows not re-sent
  8. duplicate suppression: deterministic tx_id across restart, one send
  9. same-room ordering: the second waits for the first
  10. outbox dir is 0700 (traversable), queue file 0600
  11. dropped rows are permanent and never re-queued
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import stat

import pytest

from core.core import MatrixCore
from core.interfaces import CoreConfig, OutboundMessage
from core.outbox import (
    DEAD_LETTER_ATTEMPTS,
    OutboundQueue,
    deterministic_tx_id,
    outbox_marker,
    with_marker,
)
from core.state import FileSessionStore, StateLayout

# Deterministic clock base for queue-level tests (no wall time).
NOW = 1_000_000.0


def _queue(tmp_path, sub="outbox", **kwargs) -> OutboundQueue:
    return OutboundQueue(tmp_path / sub, **kwargs)


class FakeMatrixServer:
    """Simulates homeserver tx_id dedup (shared across "process" restarts)."""

    def __init__(self) -> None:
        self.events: dict[str, str] = {}
        self.sends = 0

    def send(self, tx_id: str, room_id: str) -> str:
        if tx_id in self.events:
            return self.events[tx_id]
        self.sends += 1
        event_id = f"$evt{self.sends}"
        self.events[tx_id] = event_id
        return event_id


class RecordingTransport:
    """In-memory MatrixTransport recording sent messages (with tx_id).

    ``server`` simulates the homeserver's per-tx_id dedup and can be
    shared across transports to span a "process restart".
    """

    def __init__(self, server: FakeMatrixServer | None = None, events=()):
        self.server = server if server is not None else FakeMatrixServer()
        self.events = list(events)
        self.started = False
        self.stopped = False
        self.sent: list[tuple[OutboundMessage, str]] = []
        self.send_error: Exception | None = None
        # text prefix -> asyncio.Event: holds matching sends until set
        self.gates: dict[str, asyncio.Event] = {}

    async def start(self, on_event) -> None:
        self.started = True
        for event in self.events:
            await on_event(event)

    async def stop(self) -> None:
        self.stopped = True

    async def send(self, msg: OutboundMessage) -> str:
        if self.send_error is not None:
            raise self.send_error
        for prefix, gate in self.gates.items():
            if msg.text.startswith(prefix):
                await gate.wait()
        event_id = self.server.send(msg.tx_id or "", msg.room_id)
        self.sent.append((msg, event_id))
        return event_id

    def sent_msgs(self) -> list[OutboundMessage]:
        return [msg for msg, _ in self.sent]

    def sent_texts(self) -> list[str]:
        return [msg.text for msg, _ in self.sent]

    async def edit(self, room_id, event_id, text) -> None:
        raise NotImplementedError

    async def react(self, room_id, event_id, key) -> None:
        raise NotImplementedError

    async def typing(self, room_id, state) -> None:
        raise NotImplementedError

    async def download(self, media) -> bytes:
        return b""


class QuietBridge:
    """RuntimeBridge that only records core callback outputs."""

    def __init__(self) -> None:
        self.outputs: list[OutboundMessage] = []

    async def on_inbound(self, event) -> None:
        pass

    async def on_runtime_output(self, msg: OutboundMessage) -> None:
        self.outputs.append(msg)


async def _wait_until(predicate, timeout: float = 5.0) -> None:
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while not predicate():
        if loop.time() > deadline:
            raise TimeoutError("condition not met in time")
        await asyncio.sleep(0.01)


def _build_core(tmp_path, transport, bridge, config=None):
    state_dir = tmp_path / "state"
    if config is None:
        config = CoreConfig(state_dir=state_dir, agent_id="worker-a")
    store = FileSessionStore(state_dir)
    core = MatrixCore(transport, store, bridge, config)
    return core, config


def _run(scenario) -> None:
    """Run an async scenario coroutine to completion (no pytest-asyncio)."""
    return asyncio.run(scenario)


async def _drain(task) -> None:
    """Wait for a core run() task to finish; cancel it if it hangs."""
    if not task.done():
        with contextlib.suppress(TimeoutError):
            await _wait_until(task.done, timeout=2.0)
        if not task.done():
            task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task


def _queue_data(state_dir) -> dict:
    return json.loads((state_dir / "outbox" / "queue.json").read_text(encoding="utf-8"))


def _row_status(state_dir, key: str):
    for row in _queue_data(state_dir)["rows"]:
        if row["key"] == key:
            return row["status"]
    return None


def _strip_marker(text: str) -> str:
    return text.split(" <!-- ")[0]


# ---------------------------------------------------------------------------
# 1. same-key re-entry
# ---------------------------------------------------------------------------


def test_same_key_reentry_replaces_keeps_position(tmp_path):
    q = _queue(tmp_path)
    a = q.enqueue("final", "k", OutboundMessage(room_id="!r1", text="v1"), now=NOW)
    b = q.enqueue("final", "k2", OutboundMessage(room_id="!r2", text="v2"), now=NOW)
    assert (a.seq, b.seq) == (1, 2)

    # Re-entry: payload replaced, generation+1, attempts reset, seq kept.
    c = q.enqueue("final", "k", OutboundMessage(room_id="!r1", text="v1-updated"), now=NOW + 10)
    assert c.seq == a.seq
    assert c.generation == 2
    assert c.attempts == 0
    assert c.payload["text"] == "v1-updated"
    assert c.status == "pending"

    # Queue position kept: the re-entered row still claims ahead of k2.
    claimed = q.claim_due(now=NOW + 11)
    assert [r.key for r in claimed] == ["k", "k2"]

    # Attempts from a prior failure are also reset by re-entry.
    q2 = _queue(tmp_path, sub="o2")
    r1 = q2.enqueue("final", "k", OutboundMessage(room_id="!r1", text="x"), now=NOW)
    q2.claim_due(now=NOW)
    assert q2.fail("final", "k", r1.seq, r1.generation, now=NOW) == "pending"
    assert q2.get("final", "k").attempts == 1
    r2 = q2.enqueue("final", "k", OutboundMessage(room_id="!r1", text="y"), now=NOW + 1)
    assert r2.attempts == 0
    assert r2.generation == 2
    assert r2.seq == r1.seq


# ---------------------------------------------------------------------------
# 2. Later delay
# ---------------------------------------------------------------------------


def test_later_delay_no_attempts_claimable_only_after_due(tmp_path):
    q = _queue(tmp_path)
    row = q.enqueue(
        "final", "k", OutboundMessage(room_id="!r", text="later"),
        delay=3600, now=NOW,
    )
    assert row.due == pytest.approx(NOW + 3600)
    assert q.claim_due(now=NOW) == []
    assert q.claim_due(now=NOW + 3599) == []
    # Waiting on a future due is not an attempt.
    assert q.get("final", "k").attempts == 0
    claimed = q.claim_due(now=NOW + 3600)
    assert [r.key for r in claimed] == ["k"]
    assert claimed[0].attempts == 0


# ---------------------------------------------------------------------------
# 3. completion quadruple
# ---------------------------------------------------------------------------


def test_complete_matches_quadruple(tmp_path):
    q = _queue(tmp_path)
    row = q.enqueue("final", "k", OutboundMessage(room_id="!r", text="x"), now=NOW)
    # Wrong generation / wrong seq never complete.
    assert q.complete("final", "k", row.seq, row.generation + 1) is False
    assert q.complete("final", "k", row.seq + 1, row.generation) is False
    assert q.get("final", "k") is not None
    # The exact quadruple removes the row.
    assert q.complete("final", "k", row.seq, row.generation) is True
    assert q.get("final", "k") is None
    # A second completion of the same quadruple is a no-op.
    assert q.complete("final", "k", row.seq, row.generation) is False


# ---------------------------------------------------------------------------
# 4. per-kind concurrency cap
# ---------------------------------------------------------------------------


def test_per_kind_concurrency_cap(tmp_path):
    q = _queue(tmp_path, concurrency=2)
    for i in range(3):
        q.enqueue("final", f"k{i}", OutboundMessage(room_id=f"!r{i}", text=str(i)), now=NOW)
    claimed = q.claim_due(now=NOW + 1)
    assert sorted(r.key for r in claimed) == ["k0", "k1"]
    # The third row of the same kind waits for a slot.
    assert q.claim_due(now=NOW + 1) == []
    assert q.complete("final", "k0", 1, 1) is True
    claimed = q.claim_due(now=NOW + 1)
    assert [r.key for r in claimed] == ["k2"]


def test_concurrency_caps_are_per_kind(tmp_path):
    q = _queue(tmp_path, concurrency=1)
    q.enqueue("final", "a", OutboundMessage(room_id="!ra", text="a"), now=NOW)
    q.enqueue("tool", "b", OutboundMessage(room_id="!rb", text="b", kind="tool"), now=NOW)
    # Separate caps per kind: one in-flight per kind, not overall.
    claimed = q.claim_due(now=NOW + 1)
    assert sorted(r.kind for r in claimed) == ["final", "tool"]


# ---------------------------------------------------------------------------
# 5. backoff sequences
# ---------------------------------------------------------------------------


def test_backoff_delay_sequences_default_and_long_running(tmp_path):
    q = _queue(tmp_path, long_running_kinds=("tool",))
    default = [q.backoff_delay("final", a) for a in range(1, 9)]
    assert default == pytest.approx([2, 4, 8, 16, 32, 60, 60, 60])
    long_running = [q.backoff_delay("tool", a) for a in range(1, 9)]
    assert long_running == pytest.approx([5, 10, 20, 40, 80, 160, 300, 300])


def test_fail_backoff_schedules_due(tmp_path):
    q = _queue(tmp_path, long_running_kinds=("tool",))
    row = q.enqueue("final", "k", OutboundMessage(room_id="!r", text="x"), now=NOW)
    t = NOW
    assert len(q.claim_due(now=t)) == 1
    assert q.fail("final", "k", row.seq, row.generation, now=t) == "pending"
    assert q.get("final", "k").due == pytest.approx(t + 2.0)
    t2 = t + 3
    q.claim_due(now=t2)
    assert q.fail("final", "k", row.seq, row.generation, now=t2) == "pending"
    assert q.get("final", "k").due == pytest.approx(t2 + 4.0)
    # Long-running kind uses its own schedule.
    tool_row = q.enqueue("tool", "t", OutboundMessage(room_id="!rt", text="t", kind="tool"), now=t2)
    q.claim_due(now=t2 + 1)
    assert q.fail("tool", "t", tool_row.seq, tool_row.generation, now=t2 + 1) == "pending"
    assert q.get("tool", "t").due == pytest.approx(t2 + 1 + 5.0)


# ---------------------------------------------------------------------------
# 6. dead letter after 3 consecutive failures
# ---------------------------------------------------------------------------


def test_dead_letter_after_three_failures(tmp_path):
    q = _queue(tmp_path)
    row = q.enqueue("final", "k", OutboundMessage(room_id="!r", text="x"), now=NOW)
    t = NOW
    q.claim_due(now=t)
    assert q.fail("final", "k", row.seq, row.generation, now=t) == "pending"
    t += 100
    q.claim_due(now=t)
    assert q.fail("final", "k", row.seq, row.generation, now=t) == "pending"
    t += 100
    q.claim_due(now=t)
    assert q.fail("final", "k", row.seq, row.generation, now=t) == "dead_letter"
    assert q.get("final", "k") is None  # out of the live queue
    dead = [r for r in q.archive() if r.status == "dead_letter"]
    assert len(dead) == 1
    assert dead[0].attempts == DEAD_LETTER_ATTEMPTS
    # Never claimed again, and the dead letter survives a restart.
    assert q.claim_due(now=t + 10_000) == []
    q2 = _queue(tmp_path)
    assert q2.claim_due(now=t + 20_000) == []
    assert any(r.status == "dead_letter" for r in q2.archive())


# ---------------------------------------------------------------------------
# 7. restart replay (core level)
# ---------------------------------------------------------------------------


def test_restart_replay_pending_and_no_resend_completed(tmp_path):
    async def scenario():
        state_dir = tmp_path / "state"
        config = CoreConfig(state_dir=state_dir, agent_id="worker-a")

        # Phase 1: deliver one message to completion, then stop cleanly.
        transport = RecordingTransport()
        core, _ = _build_core(tmp_path, transport, QuietBridge(), config)
        task = asyncio.ensure_future(core.run())
        try:
            await _wait_until(lambda: transport.started)
            await core.submit(OutboundMessage(room_id="!r", text="done"))
            await _wait_until(lambda: len(transport.sent) == 1)
            await core.stop()
        finally:
            await _drain(task)
        assert task.result() is None
        # The completed row is removed from the queue file.
        assert not any(
            r["payload"]["text"] == "done" for r in _queue_data(state_dir)["rows"]
        )

        # Phase 2: fresh queue/core on the same state dir: one immediate
        # pending row, one row with a Later delay (must not be sent early).
        q = OutboundQueue(state_dir / "outbox")
        q.enqueue("final", "immediate", OutboundMessage(room_id="!r", text="go now"))
        q.enqueue(
            "final", "delayed",
            OutboundMessage(room_id="!r2", text="go later"), delay=1.0,
        )

        transport2 = RecordingTransport(server=transport.server)
        bridge2 = QuietBridge()
        core2, _ = _build_core(tmp_path, transport2, bridge2, config)
        task2 = asyncio.ensure_future(core2.run())
        try:
            await _wait_until(lambda: transport2.started)
            # The delayed row's due is 1.0s in the future: only the
            # immediate row can be out yet.
            await _wait_until(lambda: len(transport2.sent) >= 1)
            assert _strip_marker(transport2.sent_texts()[0]) == "go now"
            await _wait_until(lambda: len(transport2.sent) == 2, timeout=10.0)
            texts = [_strip_marker(t) for t in transport2.sent_texts()]
            assert texts == ["go now", "go later"]
            # The completed phase-1 message is NOT re-sent.
            assert not any(t.startswith("done") for t in texts)
            assert len(transport2.sent) == 2
            await core2.stop()
        finally:
            await _drain(task2)
        assert task2.result() is None
        assert transport2.stopped
        assert [_strip_marker(m.text) for m in bridge2.outputs] == ["go now", "go later"]

    _run(scenario())


# ---------------------------------------------------------------------------
# 8. duplicate suppression: deterministic tx_id across restart
# ---------------------------------------------------------------------------


def test_deterministic_tx_id_restart_single_send(tmp_path):
    outbox_dir = tmp_path / "state" / "outbox"
    server = FakeMatrixServer()
    msg = OutboundMessage(room_id="!r", text="idempotent")

    # "Process 1": enqueue + claim, then crash before completing.
    q1 = OutboundQueue(outbox_dir)
    row = q1.enqueue("final", "idem-key", msg, now=NOW)
    claimed = q1.claim_due(now=NOW)
    assert len(claimed) == 1
    tx1 = deterministic_tx_id(row.kind, row.key, row.seq, row.generation)
    event1 = server.send(tx1, "!r")  # the send that survived the crash

    # "Process 2" (restart): restore replays the in-flight row.
    q2 = OutboundQueue(outbox_dir)
    replayed = q2.restore()
    assert [r.key for r in replayed] == ["idem-key"]
    claimed2 = q2.claim_due(now=NOW)
    assert len(claimed2) == 1
    tx2 = deterministic_tx_id(
        claimed2[0].kind, claimed2[0].key, claimed2[0].seq, claimed2[0].generation
    )
    assert tx2 == tx1  # deterministic across restart
    event2 = server.send(tx2, "!r")
    assert event2 == event1  # homeserver dedup: same event
    assert server.sends == 1  # exactly one send ever happened
    assert q2.complete(
        claimed2[0].kind, claimed2[0].key,
        claimed2[0].seq, claimed2[0].generation,
    ) is True


def test_core_submit_uses_deterministic_tx_id_and_marker(tmp_path):
    async def scenario():
        transport = RecordingTransport()
        bridge = QuietBridge()
        core, _ = _build_core(tmp_path, transport, bridge)
        task = asyncio.ensure_future(core.run())
        try:
            await _wait_until(lambda: transport.started)
            qid = await core.submit(
                OutboundMessage(room_id="!r", text="hello"), key="core-key"
            )
            assert qid == "final:core-key"
            await _wait_until(lambda: len(transport.sent) == 1)
            sent_msg, event_id = transport.sent[0]
            data = _queue_data(tmp_path / "state")
            # The row completed (removed); the receipts carry the identity.
            assert data["rows"] == []
            statuses = [r["status"] for r in data["receipts"]]
            assert "sent" in statuses and "delivered" in statuses
            delivered = [r for r in data["receipts"] if r["status"] == "delivered"][0]
            assert delivered["event_id"] == event_id
            sent_receipt = data["receipts"][0]
            expected_tx = deterministic_tx_id(
                "final", "core-key", sent_receipt["seq"], sent_receipt["generation"]
            )
            assert sent_msg.tx_id == expected_tx
            # The idempotency marker rides at the end of the body.
            marker = outbox_marker(
                "final", "core-key", sent_receipt["seq"], sent_receipt["generation"]
            )
            assert sent_msg.text == with_marker("hello", marker)
            # The bridge callback sees the original (unmarked) message.
            assert bridge.outputs == [OutboundMessage(room_id="!r", text="hello")]
            await core.stop()
        finally:
            await _drain(task)
        assert task.result() is None

    _run(scenario())


# ---------------------------------------------------------------------------
# 9. same-room ordering
# ---------------------------------------------------------------------------


def test_same_room_ordering_queue_level(tmp_path):
    q = _queue(tmp_path)
    q.enqueue("final", "a", OutboundMessage(room_id="!room", text="1"), now=NOW)
    q.enqueue("final", "b", OutboundMessage(room_id="!room", text="2"), now=NOW)
    q.enqueue("final", "c", OutboundMessage(room_id="!other", text="3"), now=NOW)
    # "b" waits for its room; the other room's row claims freely.
    claimed = q.claim_due(now=NOW + 1)
    assert [r.key for r in claimed] == ["a", "c"]
    q.complete("final", "a", 1, 1)
    claimed = q.claim_due(now=NOW + 1)
    assert [r.key for r in claimed] == ["b"]


def test_same_room_ordering_second_waits_for_first(tmp_path):
    async def scenario():
        state_dir = tmp_path / "state"
        gate = asyncio.Event()
        transport = RecordingTransport()
        transport.gates["first"] = gate
        bridge = QuietBridge()
        core, _ = _build_core(tmp_path, transport, bridge)
        task = asyncio.ensure_future(core.run())
        try:
            await _wait_until(lambda: transport.started)
            await core.submit(OutboundMessage(room_id="!room", text="first msg"), key="a")
            await core.submit(OutboundMessage(room_id="!room", text="second msg"), key="b")
            # "a" is in flight (gated); "b" must wait for the room.
            await _wait_until(
                lambda: _row_status(state_dir, "a") == "in_flight"
                and _row_status(state_dir, "b") == "pending"
            )
            assert transport.sent == []
            gate.set()
            await _wait_until(lambda: len(transport.sent) == 2)
            assert [_strip_marker(t) for t in transport.sent_texts()] == [
                "first msg",
                "second msg",
            ]
            await core.stop()
        finally:
            await _drain(task)
        assert task.result() is None

    _run(scenario())


# ---------------------------------------------------------------------------
# 10. permissions
# ---------------------------------------------------------------------------


def test_outbox_permissions_0700_dir_0600_file(tmp_path):
    layout = StateLayout(tmp_path / "state")
    layout.ensure()
    # The dir must be traversable (0700): non-root processes need the
    # exec bit to enter it and create queue.json.
    assert stat.S_IMODE(layout.outbox.stat().st_mode) == 0o700
    q = OutboundQueue(layout.outbox)
    q.enqueue("final", "k", OutboundMessage(room_id="!r", text="secret"), now=NOW)
    assert stat.S_IMODE(q.file_path.stat().st_mode) == 0o600
    # The payload (message bodies) round-trips intact.
    assert q.get("final", "k").payload["text"] == "secret"


# ---------------------------------------------------------------------------
# 11. dropped permanence
# ---------------------------------------------------------------------------


def test_dropped_permanent_never_requeued(tmp_path):
    q = _queue(tmp_path)
    row = q.enqueue("final", "k", OutboundMessage(room_id="!r", text="x"), now=NOW)
    assert q.drop("final", "k", row.seq, row.generation, reason="room_gone") is True
    assert q.get("final", "k") is None
    assert q.claim_due(now=NOW + 10_000) == []
    dropped = [r for r in q.archive() if r.status == "dropped"]
    assert len(dropped) == 1
    receipts = q.receipts()
    assert receipts[-1]["status"] == "dropped"
    assert receipts[-1]["reason"] == "room_gone"
    # Permanent across restart; still never claimable.
    q2 = _queue(tmp_path)
    assert q2.claim_due(now=NOW + 20_000) == []
    assert any(r.status == "dropped" for r in q2.archive())
    # Dropping a missing/stale quadruple is a no-op.
    assert q2.drop("final", "k", row.seq, row.generation, reason="again") is False


def test_gone_records_and_archives(tmp_path):
    q = _queue(tmp_path)
    row = q.enqueue("final", "k", OutboundMessage(room_id="!r", text="x"), now=NOW)
    q.claim_due(now=NOW)
    assert q.record("final", "k", row.seq, row.generation, "gone") is True
    assert q.claim_due(now=NOW + 10_000) == []
    assert any(r.status == "gone" for r in q.archive())
    assert q.receipts()[-1]["status"] == "gone"


# ---------------------------------------------------------------------------
# core worker: dead letter health error, accepted callback point
# ---------------------------------------------------------------------------


def test_core_dead_letter_health_error(tmp_path):
    async def scenario():
        config = CoreConfig(
            state_dir=tmp_path / "state",
            agent_id="worker-a",
            outbox_backoff_initial=0.01,
            outbox_backoff_max=0.02,
        )
        transport = RecordingTransport()
        transport.send_error = OSError("send refused")
        bridge = QuietBridge()
        errors: list[tuple[str, BaseException]] = []
        core, _ = _build_core(tmp_path, transport, bridge, config)
        core._on_error = lambda context, exc: errors.append((context, exc))
        task = asyncio.ensure_future(core.run())
        try:
            await _wait_until(lambda: transport.started)
            qid = await core.submit(OutboundMessage(room_id="!r", text="x"))
            assert isinstance(qid, str) and qid.startswith("final:")
            # 3 consecutive failed attempts -> health error + dead letter.
            await _wait_until(
                lambda: any(c == "outbox_dead_letter" for c, _ in errors),
                timeout=10.0,
            )
            assert any(c == "submit" for c, _ in errors)
            assert core.last_error is not None
            assert "outbox_dead_letter" in core.last_error
            assert bridge.outputs == []  # no output callback on failed sends
            await core.stop()
        finally:
            await _drain(task)
        assert task.result() is None
        # The dead letter persisted to the outbox file.
        assert any(
            r["status"] == "dead_letter" for r in _queue_data(tmp_path / "state")["archive"]
        )

    _run(scenario())


def test_accepted_receipt_after_completion(tmp_path):
    q = _queue(tmp_path)
    row = q.enqueue("final", "k", OutboundMessage(room_id="!r", text="x"), now=NOW)
    q.claim_due(now=NOW)
    q.record("final", "k", row.seq, row.generation, "sent", tx_id="tx1")
    q.record("final", "k", row.seq, row.generation, "delivered", event_id="$e")
    q.complete("final", "k", row.seq, row.generation)
    # "accepted" (channel ack) can still be attributed after completion.
    assert q.record("final", "k", row.seq, row.generation, "accepted") is True
    assert [r["status"] for r in q.receipts()] == ["sent", "delivered", "accepted"]
