# -*- coding: utf-8 -*-
"""Kill-target identity verification on the two bare-signal paths (F1).

The normal cancel path (``request_cancel``) already verifies the kill
target via ``_verify_kill_target``; the restart-recovery reaper
(``_scan_once``) and the cancel escalation (``_cancel_escalate``) used to
signal the recorded pid without any identity check. When the kernel
recycles that pid for an unrelated process, both paths killed an
innocent one. These tests prove both paths now:

  1. reaper mismatch: a live unrelated process (cmdline without the
     dispatch's CLI name / session id) is NOT signalled; the task lands
     terminal ``stale`` with ``death_reason=pid_reused_or_mismatch``;
  2. reaper control: a live process whose cmdline DOES carry the CLI
     name + session id is still killed (existing behavior preserved);
  3. escalation mismatch: a live unrelated process is NOT SIGKILLed;
     the skip is recorded as ``skip_pid_mismatch`` (no forged SIGKILL);
     the terminal state stays with the reaper/watchdog paths.

Isolation: same pattern as ``test_dispatch_v020`` — tmp state dir,
short-lived child doubles killed in ``finally``. The real dispatch
state is never touched.
"""
from __future__ import annotations

import asyncio
import subprocess
import sys
import time
import uuid

from qwen_code_bridge import dispatch as D  # noqa: F401  (import sanity)
from test_dispatch_v020 import (  # noqa: F401  (fixture re-export)
    _iso,
    _killpg,
    _read_task,
    _wait_dead,
    _write_task,
    dispatch_dir,
    state_dir,
    svc,
)

#: CLI name used in the task fixtures; the control child carries it as a
#: plain argv element (interpreter-wrapped dispatch shape).
_CLI = "qwen"


def _stalled_running(dispatch_dir, pid: int, **overrides) -> str:
    """A running task that satisfies every reaper kill precondition:
    spent wall budget + no progress file (inf > any stall window)."""
    now = time.time()
    return _write_task(
        dispatch_dir,
        state="running",
        pid=pid,
        cli=_CLI,
        session_id=overrides.pop("session_id", uuid.uuid4().hex),
        timeout=60.0,
        started_at=_iso(now - 130),
        started_epoch=now - 130,
        updated_at=_iso(now - 130),
        **overrides,
    )


def test_reaper_mismatch_does_not_kill(svc, dispatch_dir):
    """1. reaper kill gate: recorded pid is a live *unrelated* process
    (cmdline has no CLI name / session id) -> no signal, task lands
    terminal stale with the explicit unowned reason."""
    proc = subprocess.Popen(["sleep", "300"])  # cmdline: "sleep 300"
    try:
        sid = uuid.uuid4().hex
        tid = _stalled_running(dispatch_dir, proc.pid, session_id=sid)
        assert _read_task(dispatch_dir, tid)["state"] == "running"

        changed = svc._scan_once()

        assert proc.poll() is None  # innocent process still alive
        task = _read_task(dispatch_dir, tid)
        assert task["state"] == "stale"
        assert task["death_reason"] == "pid_reused_or_mismatch"
        assert task["finished_at"] is not None
        assert any("pid" in line for line in task["terminal_summary"])
        names = [s.get("signal") for s in task.get("signals") or []]
        assert "SIGKILL" not in names
        assert any(t.get("task_id") == tid for t in changed)
    finally:
        # No start_new_session: the child shares pytest's process group,
        # so a targeted kill, never killpg.
        proc.kill()
        proc.wait(timeout=5)


def test_reaper_verified_kill_preserved(svc, dispatch_dir):
    """2. control (no false negative): a live process whose cmdline
    carries the CLI name + session id is still killed; the task lands
    ``timeout`` with the reaper death reason (existing behavior)."""
    sid = uuid.uuid4().hex
    proc = subprocess.Popen(
        [sys.executable, "-c", "import time; time.sleep(300)", _CLI, sid],
        start_new_session=True,
    )
    try:
        tid = _stalled_running(dispatch_dir, proc.pid, session_id=sid)
        changed = svc._scan_once()
        proc.wait(timeout=10)
        assert proc.returncode == -9  # SIGKILL landed
        task = _read_task(dispatch_dir, tid)
        assert task["state"] == "timeout"
        assert task["death_reason"] == "timeout_killed_by_reaper"
        assert any(t.get("task_id") == tid for t in changed)
    finally:
        _killpg(proc)


def test_cancel_escalate_mismatch_records_skip(svc, dispatch_dir):
    """3. escalation kill gate: recorded pid is a live *unrelated*
    process -> no SIGKILL; ``signals[]`` records ``skip_pid_mismatch``
    (not a forged SIGKILL) and no terminal state is written here."""
    proc = subprocess.Popen(["sleep", "300"])
    try:
        sid = uuid.uuid4().hex
        tid = _write_task(
            dispatch_dir,
            state="cancelling",
            pid=proc.pid,
            cli=_CLI,
            session_id=sid,
            signals=[
                {"ts": _iso(time.time()), "signal": "SIGTERM", "pid": proc.pid}
            ],
        )
        asyncio.run(svc._cancel_escalate(tid, proc.pid, grace=0.05))

        assert proc.poll() is None  # innocent process still alive
        task = _read_task(dispatch_dir, tid)
        names = [s.get("signal") for s in task.get("signals") or []]
        assert "skip_pid_mismatch" in names
        assert "SIGKILL" not in names
        skip = [
            s for s in task.get("signals") or []
            if s.get("signal") == "skip_pid_mismatch"
        ][0]
        assert skip.get("pid") == proc.pid
        # Terminal state is left to the reaper/watchdog paths.
        assert task["state"] == "cancelling"
        assert task.get("death_reason") is None
    finally:
        proc.kill()
        proc.wait(timeout=5)


def test_cancel_escalate_verified_kill_preserved(svc, dispatch_dir, tmp_path):
    """4. control: a verified live child ignoring SIGTERM still gets
    SIGKILL after the grace window and the SIGKILL is recorded
    (existing behavior, unchanged by the identity gate)."""
    sid = uuid.uuid4().hex
    code = (
        "import signal, time\n"
        "signal.signal(signal.SIGTERM, signal.SIG_IGN)\n"
        f"time.sleep(300)\n# {sid}"
    )
    proc = subprocess.Popen(
        [sys.executable, "-c", code, _CLI, sid], start_new_session=True
    )
    try:
        time.sleep(0.5)  # let the child install its SIGTERM disposition
        tid = _write_task(
            dispatch_dir,
            state="cancelling",
            pid=proc.pid,
            cli=_CLI,
            session_id=sid,
        )
        asyncio.run(svc._cancel_escalate(tid, proc.pid, grace=0.05))
        _wait_dead(proc)
        assert proc.returncode == -9
        task = _read_task(dispatch_dir, tid)
        names = [s.get("signal") for s in task.get("signals") or []]
        assert "SIGKILL" in names
        assert "skip_pid_mismatch" not in names
    finally:
        _killpg(proc)
