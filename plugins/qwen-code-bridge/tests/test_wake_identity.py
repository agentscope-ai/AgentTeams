# -*- coding: utf-8 -*-
"""Wake identity in the durable queue (#1411 round 2).

The queue record carries a ``kind`` (``terminal`` vs ``denial``) and the
delivery callbacks receive it, so a mid-run denial can never own the task
file's ``wake_status``:

1. distinct-rule denials are each delivered once to the origin session by
   the REAL queue, and none of them flips ``wake_status`` to ``sent``;
2. a third distinct-rule denial still lands after two delivered denials
   (the sent marker cannot retroactively suppress it);
3. a denial record already queued is still delivered when the task file
   reports ``sent`` (terminal delivered first);
4. restart recovery: a delivered denial does not swallow the terminal
   completion notice — the terminal wake (queued via the real reaper
   path) is delivered with the TERMINAL text and only it flips ``sent``;
5. pre-upgrade records without ``kind`` keep the old ``terminal``
   behavior (delivered → sent; crash re-enqueue → dropped).

Only the HTTP sender is faked (the designed seam); the queue file, the
drain loop, ``mark_wake_delivered`` and ``wake_is_delivered`` are the
production code, wired exactly like ``plugin.py`` does.
"""

import asyncio
import json

import pytest

from qwen_code_bridge import state, wake_queue
from qwen_code_bridge.dispatch import (
    DispatchService,
    mark_wake_delivered,
    wake_is_delivered,
)

TASK = "d" * 32
ORIGIN = "origin-1"


@pytest.fixture(autouse=True)
def _queue_isolation(monkeypatch):
    """Real queue file (per-test state dir via the conftest guard) with
    the production callbacks; no background drain loop, no real sender."""
    wake_queue.stop()
    monkeypatch.setattr(wake_queue, "_sender", None)
    monkeypatch.setattr(wake_queue, "_on_delivered", None)
    monkeypatch.setattr(wake_queue, "_is_delivered", None)
    yield
    wake_queue.stop()


def _dispatch_dir():
    return state.state_dir() / "dispatch"


def _svc(**service_kwargs) -> DispatchService:
    return DispatchService(dispatch_dir=_dispatch_dir(), **service_kwargs)


def _write_task(task_id: str = TASK, **fields) -> None:
    """Write (or merge into) the task file under the per-test state dir."""
    dispatch_dir = _dispatch_dir()
    dispatch_dir.mkdir(parents=True, exist_ok=True)
    path = dispatch_dir / f"{task_id}.json"
    task = {}
    if path.exists():
        task = json.loads(path.read_text(encoding="utf-8"))
    if not task:
        task = {
            "task_id": task_id,
            "task": "identity-probe",
            "state": "running",
            "wake": {"session_id": "wake-1", "agent_id": "A"},
            "origin": {"session_id": ORIGIN, "channel": "console"},
            "wake_status": None,
        }
    task.update(fields)
    path.write_text(json.dumps(task, ensure_ascii=False), encoding="utf-8")


def _task(task_id: str = TASK) -> dict:
    return json.loads(
        (_dispatch_dir() / f"{task_id}.json").read_text(encoding="utf-8")
    )


def _wire_real_callbacks() -> None:
    """The production wiring (``plugin._start_wake_queue`` does this)."""
    wake_queue._on_delivered = mark_wake_delivered
    wake_queue._is_delivered = wake_is_delivered


def _drain(injected: list) -> dict:
    """One drain pass with a recording sender (the only faked seam)."""

    def sender(
        session_id,
        agent_id,
        base_url,
        text,
        channel="console",
        user_id="default",
        room_bell=False,
        approval_level="",
    ):
        injected.append({"session_id": session_id, "text": text})

    return asyncio.run(wake_queue.drain_once(sender))


def _note_denial(svc: DispatchService, rule: str, tool: str) -> None:
    svc._note_denial(
        TASK, tool, {"rule": rule, "reason": rule, "behavior": "deny"}
    )


# ---------------------------------------------------------------------------
# 1-2. distinct-rule denials: delivered by the real queue, never flip sent
# ---------------------------------------------------------------------------


def test_distinct_rule_denials_delivered_without_flipping_sent() -> None:
    injected: list = []
    svc = _svc()
    _write_task()
    _wire_real_callbacks()

    _note_denial(svc, "exec-compound", "run_shell_command")
    stats = _drain(injected)
    assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}
    assert wake_queue.load() == []

    _note_denial(svc, "outside-root", "cat")
    stats = _drain(injected)
    assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}

    # Both notices landed in the ORIGIN session (not the wake target).
    assert [call["session_id"] for call in injected] == [ORIGIN, ORIGIN]
    assert "exec-compound" in injected[0]["text"]
    assert "outside-root" in injected[1]["text"]
    # Denials never own the terminal field.
    assert _task()["wake_status"] is None
    assert "wake_delivered_at" not in _task()
    assert set(_task()["denial_wakes"]) == {"exec-compound", "outside-root"}


def test_coexisting_denials_and_terminal_deliver_once() -> None:
    injected: list = []
    svc = _svc()
    _write_task()
    _wire_real_callbacks()
    _note_denial(svc, "exec-compound", "run_shell_command")
    _note_denial(svc, "outside-root", "cat")
    wake_queue.enqueue({"task_id": TASK, "text": "terminal-complete", "session_id": ORIGIN})
    assert len(wake_queue.load()) == 3
    assert len({r.get("event_id") for r in wake_queue.load() if r.get("kind") == "denial"}) == 2
    stats = _drain(injected)
    assert stats == {"sent": 3, "failed": 0, "dropped": 0, "busy": 0}
    assert len(injected) == 3
    assert injected[-1]["text"] == "terminal-complete"
    assert _task()["wake_status"] == "sent"
    assert not wake_queue.load()


def test_third_distinct_rule_denial_still_delivered() -> None:
    """After two delivered denials, a third distinct rule still lands:
    proof the sent marker (which denials never set) cannot suppress it."""
    injected: list = []
    svc = _svc()
    _write_task()
    _wire_real_callbacks()

    for rule, tool in (
        ("exec-compound", "run_shell_command"),
        ("outside-root", "cat"),
        ("exec-denied", "run_shell_command"),
    ):
        _note_denial(svc, rule, tool)
        stats = _drain(injected)
        assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}

    assert [call["session_id"] for call in injected] == [ORIGIN] * 3
    assert "exec-denied" in injected[2]["text"]
    assert _task()["wake_status"] is None


# ---------------------------------------------------------------------------
# 3. a sent task file must not drop a queued denial record
# ---------------------------------------------------------------------------


def test_sent_marker_does_not_suppress_queued_denial() -> None:
    injected: list = []
    _write_task()
    _wire_real_callbacks()

    # The denial was queued while the task ran (kind carried by the record)...
    wake_queue.enqueue(
        {
            "task_id": TASK,
            "kind": "denial",
            "text": "denied: exec-compound",
            "session_id": ORIGIN,
        }
    )
    # ...and the terminal wake went out before it drained.
    _write_task(wake_status="sent")

    stats = _drain(injected)
    assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}
    assert wake_queue.load() == []
    assert injected[0]["text"] == "denied: exec-compound"
    # The delivery leaves the terminal sent state alone.
    assert _task()["wake_status"] == "sent"


# ---------------------------------------------------------------------------
# 4. restart recovery: the terminal notice still lands after a denial
# ---------------------------------------------------------------------------


def test_terminal_wake_lands_after_delivered_denial(monkeypatch) -> None:
    """The maintainer's sequence: a denial wake is delivered mid-run, the
    task ends, and restart recovery's terminal notice (inline send fails
    → durable queue) must still be delivered — with the terminal text —
    and only it flips ``wake_status`` to ``sent``."""
    monkeypatch.delenv("QWEN_CODE_BRIDGE_RUNS_DIR", raising=False)
    injected: list = []

    def flaky(*args, **kwargs):
        raise RuntimeError("wake inject failed: http 409")

    svc = _svc(wake_sender=flaky)
    _write_task()
    _wire_real_callbacks()

    # 1) Mid-run denial delivered by the real queue.
    _note_denial(svc, "exec-compound", "run_shell_command")
    stats = _drain(injected)
    assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}
    assert _task()["wake_status"] is None

    # 2) The task ends; recovery finalizes it and notifies externally.
    #    The inline send fails → the terminal wake goes to the queue.
    _write_task(state="failed", exit_code=1, wall_s=2.0)
    asyncio.run(svc._notify_external(_task()))
    assert str(_task()["wake_status"]).startswith("queued:")
    record = wake_queue.load()[0]
    assert record["kind"] == "terminal"

    # 3) The durable queue delivers the TERMINAL text (not the denial's).
    stats = _drain(injected)
    assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}
    assert wake_queue.load() == []
    terminal_call = injected[-1]
    assert terminal_call["session_id"] == ORIGIN
    assert "终态=failed" in terminal_call["text"]
    assert "approval denied" not in terminal_call["text"]
    assert _task()["wake_status"] == "sent"
    assert _task()["wake_delivered_at"]


# ---------------------------------------------------------------------------
# 5. pre-upgrade records without ``kind`` keep the terminal behavior
# ---------------------------------------------------------------------------


def test_legacy_record_without_kind_treated_as_terminal() -> None:
    injected: list = []
    _write_task()
    _wire_real_callbacks()

    # No "kind" key: exactly what the pre-upgrade code persisted.
    wake_queue.enqueue(
        {"task_id": TASK, "text": "legacy done", "session_id": ORIGIN}
    )
    stats = _drain(injected)
    assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}
    assert injected[0]["text"] == "legacy done"
    assert _task()["wake_status"] == "sent"

    # Crash between the send and the durable removal: the record is back on
    # disk and the task file reports sent → dropped, no second injection.
    wake_queue.enqueue(
        {"task_id": TASK, "text": "legacy done", "session_id": ORIGIN}
    )
    stats = _drain(injected)
    assert stats == {"sent": 0, "failed": 0, "dropped": 1, "busy": 0}
    assert wake_queue.load() == []
    assert len(injected) == 1


# ---------------------------------------------------------------------------
# contract pins: the callbacks are kind-aware at the boundary
# ---------------------------------------------------------------------------


def test_wake_is_delivered_is_kind_aware() -> None:
    _write_task(wake_status="sent")
    assert wake_is_delivered(TASK, "terminal") is True
    assert wake_is_delivered(TASK) is True  # legacy default
    assert wake_is_delivered(TASK, "denial") is False


def test_mark_wake_delivered_ignores_denial() -> None:
    _write_task()
    mark_wake_delivered(TASK, "denial")  # must leave the task file alone
    assert _task()["wake_status"] is None
    assert "wake_delivered_at" not in _task()
    mark_wake_delivered(TASK, "terminal")
    assert _task()["wake_status"] == "sent"
    assert _task()["wake_delivered_at"]
