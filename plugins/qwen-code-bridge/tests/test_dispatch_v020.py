# -*- coding: utf-8 -*-
"""v0.2.0 closeout tests for the dispatch surface (9 cases).

Covers:
  1. request_cancel happy path (SIGTERM -> process gone -> ``cancelled``)
  2. cancel does not affect other tasks
  3. cancel idempotency on a terminal task (``already_terminal``)
  4. identity verification: pid whose cmdline lacks the session id
  5. escalation: SIGKILL after the grace window (stubborn child)
  6. pending cancel: no pid -> ``cancelled`` directly; a cancel raced into
     the spawn window prevents the spawn
  7. log tail truncation (``get_log_tail``)
  8. default timeout: dispatch without ``timeout_s`` -> 7200
  9. timeout terminal state (v0.2.0 fix): watchdog-lost + spent budget ->
     ``timeout`` / ``timeout_killed`` (never dangling ``stale``); no
     evidence -> ``stale`` with ``finished_at``

Isolation: every test redirects ``QWEN_CODE_BRIDGE_STATE_DIR`` to a tmp
dir (and passes an explicit ``dispatch_dir``); the real dispatch state
under ``/app/working/.qwenpaw/qwen-code-bridge/dispatch/`` is never
touched. Child processes are short-lived ``bash``/``sleep`` doubles,
each in its own process group, killed in ``finally``.
"""
from __future__ import annotations

import asyncio
import json
import os
import signal
import subprocess
import sys
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

import pytest

from qwen_code_bridge import dispatch as D
from qwen_code_bridge.dispatch import (
    DEFAULT_DISPATCH_TIMEOUT,
    DispatchService,
    PidMismatchError,
    orphan_cleanup,
)
from qwen_code_bridge.state import STATE_DIR_ENV

#: Basename of the test interpreter — the child's argv[0], as seen in
#: ``/proc/<pid>/cmdline``; the task's ``cli`` must match it.
_CLI_NAME = os.path.basename(sys.executable) or "python3"


# ---- helpers -------------------------------------------------------------


def _iso(epoch: float) -> str:
    """RFC3339 ``Z`` timestamp, same format as ``_utcnow()``."""
    return datetime.fromtimestamp(
        epoch, tz=timezone.utc
    ).strftime("%Y-%m-%dT%H:%M:%SZ")


def _write_task(dispatch_dir: Path, **overrides) -> str:
    """Write a minimal task file; returns the task id."""
    now = time.time()
    task: dict = {
        "task_id": uuid.uuid4().hex,
        "task": "unit-test",
        "cli": "bash",
        "session_id": uuid.uuid4().hex,
        "state": "running",
        "pid": None,
        "exit_code": None,
        "timeout": 7200.0,
        "started_at": _iso(now),
        "started_epoch": now,
        "updated_at": _iso(now),
        "finished_at": None,
        "terminating": False,
        "signals": [],
        "wake_status": None,
    }
    task.update(overrides)
    path = dispatch_dir / f"{task['task_id']}.json"
    path.write_text(json.dumps(task, ensure_ascii=False), encoding="utf-8")
    return task["task_id"]


def _read_task(dispatch_dir: Path, task_id: str) -> dict:
    return json.loads(
        (dispatch_dir / f"{task_id}.json").read_text(encoding="utf-8")
    )


def _spawn_verifiable(
    sid: str,
    ignore_term: bool = False,
    ready_dir: str | None = None,
) -> subprocess.Popen:
    """A killable child whose ``/proc/<pid>/cmdline`` carries the session id.

    A python child (argv[0] = interpreter path) with the session id embedded
    in its ``-c`` code. SIGTERM handling is explicit: by default the child
    dies on SIGTERM; with ``ignore_term=True`` it ignores SIGTERM
    deterministically (``signal.SIG_IGN``) so the escalation test does not
    depend on shell trap semantics.

    ``ready_dir``: when given, the child writes a flag file right after
    installing the SIGTERM disposition; the parent waits for it before
    returning, so a SIGTERM cannot race the interpreter startup.
    """
    ignore = (
        "signal.signal(signal.SIGTERM, signal.SIG_IGN)\n" if ignore_term else ""
    )
    ready = os.path.join(ready_dir, f"ready-{sid[:8]}") if ready_dir else None
    ready_line = f"open({ready!r}, 'w').write('1')\n" if ready else ""
    code = f"import os, signal, time\n{ignore}{ready_line}time.sleep(300)\n# {sid}"
    proc = subprocess.Popen(
        [sys.executable, "-c", code],
        start_new_session=True,
    )
    if ready:
        deadline = time.monotonic() + 10
        while not os.path.exists(ready) and time.monotonic() < deadline:
            time.sleep(0.02)
        try:
            os.unlink(ready)  # test fixture, safe to remove
        except OSError:
            pass
    return proc


def _killpg(proc: subprocess.Popen) -> None:
    if proc.poll() is None:
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        except (ProcessLookupError, OSError):
            pass
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        pass


def _wait_dead(proc: subprocess.Popen, timeout: float = 8.0) -> None:
    deadline = time.monotonic() + timeout
    while proc.poll() is None and time.monotonic() < deadline:
        time.sleep(0.05)


# ---- fixtures -------------------------------------------------------------


@pytest.fixture()
def state_dir(tmp_path, monkeypatch):
    d = tmp_path / "bridge-state"
    d.mkdir()
    # Hard requirement: tests must never touch the real dispatch state.
    monkeypatch.setenv(STATE_DIR_ENV, str(d))
    monkeypatch.delenv("QWENPAW_WORKING_DIR", raising=False)
    monkeypatch.delenv("QWENPAW_CODING_CLI_STATE_DIR", raising=False)
    # Keep start() side effects (goals settings, chat sync) inside tmp.
    monkeypatch.setenv("QWEN_CODE_BRIDGE_CLI_HOME", str(tmp_path / "cli-home"))
    monkeypatch.setenv("QWEN_CODE_BRIDGE_CHAT_SYNC", "0")
    return d


@pytest.fixture()
def dispatch_dir(state_dir) -> Path:
    d = state_dir / "dispatch"
    d.mkdir()
    return d


@pytest.fixture()
def svc(dispatch_dir) -> DispatchService:
    return DispatchService(dispatch_dir=dispatch_dir, watch_interval=0.05)


# ---- the 9 cases -----------------------------------------------------------


def test_cancel_happy_path(svc, dispatch_dir):
    """1. live child cancelled: process disappears, state=cancelled,
    signals contains SIGTERM."""
    sid = uuid.uuid4().hex
    proc = _spawn_verifiable(sid)
    tid = _write_task(
        dispatch_dir,
        state="running",
        pid=proc.pid,
        cli=_CLI_NAME,
        session_id=sid,
    )
    try:
        res = asyncio.run(svc.request_cancel(tid, reason="unit-test"))
        assert res["ok"] is True
        assert res["signalled"] == "SIGTERM"
        _wait_dead(proc)
        assert proc.poll() is not None  # SIGTERM took effect
        task = svc.get_task(tid)
        assert task["state"] == "cancelled"
        assert task["death_reason"] == "cancelled_by_user"
        assert task["finished_at"] is not None
        signals = [s.get("signal") for s in task.get("signals") or []]
        assert "SIGTERM" in signals
    finally:
        _killpg(proc)


def test_cancel_does_not_affect_other_tasks(svc, dispatch_dir):
    """2. cancelling A leaves B alive and untouched."""
    sid_a, sid_b = uuid.uuid4().hex, uuid.uuid4().hex
    proc_a = _spawn_verifiable(sid_a)
    proc_b = _spawn_verifiable(sid_b)
    tid_a = _write_task(
        dispatch_dir,
        state="running",
        pid=proc_a.pid,
        cli=_CLI_NAME,
        session_id=sid_a,
        task="task-a",
    )
    tid_b = _write_task(
        dispatch_dir,
        state="running",
        pid=proc_b.pid,
        cli=_CLI_NAME,
        session_id=sid_b,
        task="task-b",
    )
    try:
        asyncio.run(svc.request_cancel(tid_a, reason="only-a"))
        _wait_dead(proc_a)
        assert proc_a.poll() is not None
        assert proc_b.poll() is None  # B still alive
        task_b = _read_task(dispatch_dir, tid_b)
        assert task_b["state"] == "running"
        assert task_b["pid"] == proc_b.pid
        assert task_b.get("signals") == []
    finally:
        _killpg(proc_a)
        _killpg(proc_b)


def test_cancel_idempotent_on_terminal(svc, dispatch_dir):
    """3. cancelling an already-terminal task is a no-op."""
    tid = _write_task(
        dispatch_dir,
        state="done",
        exit_code=0,
        finished_at=_iso(time.time()),
    )
    res = asyncio.run(svc.request_cancel(tid, reason="late"))
    assert res["already_terminal"] is True
    assert res["state"] == "done"
    assert res["previous_state"] == "done"
    assert res["signalled"] is None
    task = svc.get_task(tid)
    assert task["state"] == "done"
    assert task.get("signals") == []


def test_cancel_pid_mismatch(svc, dispatch_dir):
    """4. pid whose cmdline lacks the task session_id: refused, no signal."""
    sid = uuid.uuid4().hex
    proc = subprocess.Popen(["sleep", "300"])  # cmdline: sleep 300
    try:
        tid = _write_task(
            dispatch_dir,
            state="running",
            pid=proc.pid,
            cli="sleep",
            session_id=sid,
        )
        with pytest.raises(PidMismatchError):
            asyncio.run(svc.request_cancel(tid, reason="should refuse"))
        assert proc.poll() is None  # process still alive
        task = _read_task(dispatch_dir, tid)
        assert task["state"] == "running"
        assert task.get("signals") == []
    finally:
        proc.kill()
        proc.wait()


def test_cancel_escalates_sigkill(svc, dispatch_dir, tmp_path):
    """5. child ignoring SIGTERM gets SIGKILL after the grace window;
    both signals are recorded."""
    sid = uuid.uuid4().hex
    proc = _spawn_verifiable(
        sid, ignore_term=True, ready_dir=str(tmp_path)
    )
    tid = _write_task(
        dispatch_dir,
        state="running",
        pid=proc.pid,
        cli=_CLI_NAME,
        session_id=sid,
    )
    try:

        async def run() -> None:
            res = await svc.request_cancel(
                tid, reason="stubborn", grace_s=0.2
            )
            assert res["signalled"] == "SIGTERM"
            deadline = time.monotonic() + 10
            while proc.poll() is None and time.monotonic() < deadline:
                await asyncio.sleep(0.1)
            await asyncio.sleep(0.3)  # let the SIGKILL record land

        asyncio.run(run())
        assert proc.poll() is not None
        assert proc.returncode == -9  # SIGKILL
        task = _read_task(dispatch_dir, tid)
        names = [s.get("signal") for s in task.get("signals") or []]
        assert "SIGTERM" in names and "SIGKILL" in names
        # Read path closes the watcher-less cancelling task.
        closed = svc.get_task(tid)
        assert closed["state"] == "cancelled"
    finally:
        _killpg(proc)


def test_pending_cancel_never_spawns(svc, dispatch_dir):
    """6. no-pid task -> cancelled directly; a cancel raced into the spawn
    window aborts start() so nothing is spawned."""
    # 6a: pending task (never spawned)
    tid = _write_task(dispatch_dir, state="pending", pid=None)
    res = asyncio.run(svc.request_cancel(tid, reason="not yet spawned"))
    assert res["state"] == "cancelled"
    assert res["already_terminal"] is False
    task = svc.get_task(tid)
    assert task["state"] == "cancelled"
    assert task["death_reason"] == "cancelled_before_spawn"
    assert task["pid"] is None
    assert task["finished_at"] is not None

    # 6b: cancel slips into the spawn window -> start() must abort
    async def abort(task_id: str) -> None:
        await svc.request_cancel(task_id, reason="spawn-window race")

    svc2 = DispatchService(
        dispatch_dir=dispatch_dir,
        watch_interval=0.05,
        pre_spawn_hook=abort,
    )
    tid2 = asyncio.run(
        svc2.start("unit-race", "prompt", None, "sleep", 7200, None)
    )
    task2 = _read_task(dispatch_dir, tid2)
    assert task2["state"] == "cancelled"
    assert task2["pid"] is None  # never spawned


def test_log_tail_truncation(svc, dispatch_dir):
    """7. 1000-line log, tail=200 -> 200 lines + truncated=true."""
    tid = _write_task(dispatch_dir, state="done")
    log = dispatch_dir / f"{tid}.log"
    log.write_text(
        "\n".join(f"line {i}" for i in range(1000)) + "\n",
        encoding="utf-8",
    )
    text, truncated, total_lines, tail_eff = svc.get_log_tail(tid, tail=200)
    assert truncated is True
    assert total_lines == 1000
    assert tail_eff == 200
    lines = text.splitlines()
    assert len(lines) == 200
    assert lines[0] == "line 800"
    assert lines[-1] == "line 999"


def test_default_timeout_applied(svc, dispatch_dir):
    """8. dispatch without timeout_s -> task JSON carries 7200."""
    assert DEFAULT_DISPATCH_TIMEOUT == 7200.0

    async def abort(task_id: str) -> None:
        await svc.request_cancel(task_id, reason="no spawn needed")

    svc2 = DispatchService(
        dispatch_dir=dispatch_dir,
        watch_interval=0.05,
        pre_spawn_hook=abort,
    )
    tid = asyncio.run(
        svc2.start("unit-default-timeout", "prompt", None, "sleep", None, None)
    )
    task = _read_task(dispatch_dir, tid)
    assert task["timeout"] == 7200.0


def test_timeout_terminal_state_watchdog_lost(svc, dispatch_dir):
    """9. v0.2.0 fix: watchdog lost after killing the child (terminating
    set, terminal write never landed) + spent budget -> ``timeout`` /
    ``timeout_killed`` with ``finished_at``, never dangling ``stale``;
    an orphan WITHOUT timeout evidence may still be ``stale`` (but always
    with ``finished_at``)."""

    def dead_pid() -> int:
        proc = subprocess.Popen(["true"])
        proc.wait()  # reaped: the pid is truly gone
        return proc.pid

    now = time.time()
    # 9a: read path (get_task) — the exact incident shape.
    tid = _write_task(
        dispatch_dir,
        state="running",
        pid=dead_pid(),
        cli="bash",
        session_id=uuid.uuid4().hex,
        timeout=0.1,
        started_at=_iso(now - 2),
        started_epoch=now - 2,
        updated_at=_iso(now - 300),
        terminating=True,
    )
    time.sleep(1.0)
    task = svc.get_task(tid)
    assert task["state"] == "timeout"
    assert task["death_reason"] == "timeout_killed"
    assert task["finished_at"] is not None
    assert task["state"] != "stale"

    # 9b: plugin-reload path (orphan_cleanup) lands the same terminal state.
    tid2 = _write_task(
        dispatch_dir,
        state="running",
        pid=dead_pid(),
        cli="bash",
        session_id=uuid.uuid4().hex,
        timeout=0.1,
        started_at=_iso(now - 2),
        started_epoch=now - 2,
        updated_at=_iso(now - 300),
        terminating=True,
    )
    changed = orphan_cleanup(str(dispatch_dir))
    assert any(t.get("task_id") == tid2 for t in changed)
    task2 = _read_task(dispatch_dir, tid2)
    assert task2["state"] == "timeout"
    assert task2["death_reason"] == "timeout_killed"
    assert task2["finished_at"] is not None

    # 9c: no timeout evidence (young task, generous budget) -> stale is
    # still allowed, but it must carry finished_at.
    tid3 = _write_task(
        dispatch_dir,
        state="running",
        pid=dead_pid(),
        cli="bash",
        session_id=uuid.uuid4().hex,
        timeout=7200.0,
        started_at=_iso(now - 300),
        started_epoch=now - 300,
        updated_at=_iso(now - 300),
        terminating=False,
    )
    task3 = svc.get_task(tid3)
    assert task3["state"] == "stale"
    assert task3["death_reason"] == "crash_or_restart"
    assert task3["finished_at"] is not None


def test_cancel_accepts_interpreter_wrapped_cli(svc, dispatch_dir):
    """Real dispatches run ``node <path>/qwen …``: argv[0] is the
    interpreter, so the CLI name must be matched against any argv element.
    """
    sid = uuid.uuid4().hex
    proc = subprocess.Popen(
        [sys.executable, "-c", "import time; time.sleep(300)", "qwen", sid],
        start_new_session=True,
    )
    try:
        tid = _write_task(
            dispatch_dir,
            state="running",
            pid=proc.pid,
            cli="qwen",
            session_id=sid,
        )
        res = asyncio.run(svc.request_cancel(tid, reason="interpreter-wrapped"))
        assert res["ok"] is True
        assert res["signalled"] == "SIGTERM"
        _wait_dead(proc)
    finally:
        try:
            _killpg(proc)
        except Exception:
            pass
