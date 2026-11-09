"""End-to-end entrypoint tests for the two-phase in-container upgrade.

Runs the REAL ``qwenpaw-worker-entrypoint.sh`` (the image PID-1 wrapper) against
a stub venv: a fake ``qwenpaw-worker`` whose served version is driven by state
files, plus the REAL orchestrator script with every external command replaced
by tmp-dir hooks (``UG_VERSION_CMD`` / ``UG_INSTALL_CMD`` / ``UG_PATCH_CMD`` /
``UG_RESTART_CMD``). No network, no docker.

Model (mirrors the selftest's single version file, which stands in for the
runtime's version probe — venv metadata before boot, live API after boot):

* ``$d/version``           — the version the runtime currently serves.
* ``$d/installed-version`` — what the (re)started worker will serve.
* the worker deletes ``$d/version`` when it "crashes".

Scenarios:

1. broken NEW runtime: boot-check matches the target (pending retained), the
   running app dies -> the post-start target gate times out -> kill-mode
   rollback (install+patch old, UNVERIFIED handoff, rc 3) -> the entrypoint
   relaunches the OLD runtime and re-enters post-start; the expect=old branch
   verifies it (readiness+settle) and only THEN finalizes: ``rolled_back`` +
   pending removed. The entrypoint must then keep serving (exit 0 on TERM).
2. broken OLD runtime: same chain, but the relaunched old app also dies ->
   verification fails -> rc 4 -> FATAL: ``rollback_failed`` + pending KEPT,
   the entrypoint exits 1 (never reports a successful recovery).
3. old runtime dies MID-SETTLE: the relaunched app serves briefly, then
   crashes inside the settle window -> rc 4 -> same FATAL contract as 2.
4. boot-check mismatch: the venv still reports the old version (the target
   install never completed) -> boot-check rollback defers (expect=old,
   recovering, exit 0) -> the worker boots the old runtime -> the post-start
   expect branch verifies -> ``rolled_back``, entrypoint keeps serving.
"""
from __future__ import annotations

import json
import os
import signal
import subprocess
import time
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]  # .../AgentTeams-runtime-upgrade/qwenpaw
ENTRYPOINT = REPO_ROOT / "scripts" / "qwenpaw-worker-entrypoint.sh"
ORCHESTRATOR = REPO_ROOT / "scripts" / "upgrade_qwenpaw_runtime.sh"
ENV_LIB = REPO_ROOT.parent / "shared" / "lib" / "agentteams-env.sh"

JOB = "upg-20261015-000001-aaaa"
CASE_BUDGET_S = 60  # hard per-scenario wall budget

WORKER_TMPL = r"""#!/bin/bash
# fake qwenpaw-worker: the served version is driven by files in $D.
D="{d}"
trap 'exit 0' TERM
echo "$$" > "$D/worker.pid"
V="$(cat "$D/installed-version" 2>/dev/null)"
if [ "$V" = "2.2.2" ] && [ -f "$D/broken-new" ]; then
    rm -f "$D/version"   # the new app dies before serving any version
    exit 1
fi
if [ "$V" = "2.2.1" ] && [ -f "$D/broken-old" ]; then
    rm -f "$D/version"   # the restored old app dies too
    exit 1
fi
if [ "$V" = "2.2.1" ] && [ -f "$D/die-in-settle" ]; then
    echo "$V" > "$D/version"
    sleep 1.5
    rm -f "$D/version"   # crashes inside the settle window (process lingers)
    while :; do sleep 1; done
fi
echo "$V" > "$D/version"
while :; do sleep 1; done
"""


def _seed(d: Path, version: str, installed: str) -> tuple[Path, Path]:
    """Working dir + the pending/state pair the entrypoint's phases expect."""
    up = d / "home" / ".qwenpaw" / "upgrade"
    up.mkdir(parents=True, exist_ok=True)
    state_file = up / f"upgrade-{JOB}.json"
    state_file.write_text(json.dumps({
        "job_id": JOB,
        "state": "boot-check-passed",
        "target": "2.2.2",
        "started_at": "2026-10-15T00:00:00Z",
    }) + "\n")
    pending = {
        "job_id": JOB,
        "old": "2.2.1",
        "target": "2.2.2",
        "venv": str(d / "venv"),
        "state_file": str(state_file),
    }
    (up / "pending.json").write_text(json.dumps(pending, indent=2) + "\n")
    (d / "version").write_text(version + "\n")
    (d / "installed-version").write_text(installed + "\n")
    return state_file, up / "pending.json"


def _env(d: Path, settle: int = 0) -> dict:
    return {
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "HOME": str(d / "home"),
        "AGENTTEAMS_WORKER_NAME": "test-worker",
        "AGENTTEAMS_RUNTIME": "k8s",
        "AGENTTEAMS_WORKER_HOME": str(d / "home"),
        "AGENTTEAMS_ENV_LIB": str(ENV_LIB),
        "AGENTTEAMS_UPGRADE_SCRIPT": str(ORCHESTRATOR),
        "AGENTTEAMS_QWENPAW_VENV": str(d / "venv"),
        "UG_VERSION_CMD": f"cat {d}/version",
        "UG_INSTALL_CMD": (
            f"echo install-{{target}} >> {d}/calls; "
            f"echo {{target}} > {d}/installed-version"
        ),
        "UG_PATCH_CMD": f"echo patch-{{target}} >> {d}/calls",
        "UG_RESTART_CMD": f"echo restart-kill >> {d}/calls",
        "UG_CHECKER_CMD": "true",
        "UG_TIMEOUT": "3",
        "UG_SETTLE": str(settle),
        "UG_SETTLE_POLL": "1",
    }


def _start(d: Path, settle: int = 0) -> subprocess.Popen:
    bin_dir = d / "venv" / "bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    worker = bin_dir / "qwenpaw-worker"
    worker.write_text(WORKER_TMPL.format(d=d))
    worker.chmod(0o755)
    return subprocess.Popen(
        ["bash", str(ENTRYPOINT)],
        env=_env(d, settle=settle),
        cwd=str(d),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )


def _cleanup(proc: subprocess.Popen) -> None:
    # The worker inherits the output pipe; killing only the entrypoint can
    # leave communicate() waiting forever after an assertion fails.
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    proc.communicate(timeout=5)


def _wait_until(fn, timeout: float = CASE_BUDGET_S, what: str = "condition") -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if fn():
            return
        time.sleep(0.2)
    raise AssertionError(f"{what!r} not met within {timeout}s")


def _term_worker(d: Path) -> None:
    pid_file = d / "worker.pid"
    _wait_until(lambda: pid_file.is_file(), what="worker.pid")
    pid = int(pid_file.read_text().strip())
    os.kill(pid, signal.SIGTERM)


def _calls(d: Path) -> list:
    path = d / "calls"
    if not path.exists():
        return []
    return path.read_text().split()


def test_recovery_full_chain_success(tmp_path):
    """rc-3 handoff + relaunch + expect=old verification -> rolled_back, live."""
    d = tmp_path / "work"
    d.mkdir()
    state_file, pending = _seed(d, "2.2.2", "2.2.2")
    (d / "broken-new").write_text("")
    proc = _start(d)
    try:
        # the recovery is FINALIZED (pending removed) only after the restored
        # old runtime passed its readiness+settle gate
        _wait_until(lambda: not pending.exists() and
                    json.loads(state_file.read_text())["state"] == "rolled_back",
                    what="recovery finalization")
        state = json.loads(state_file.read_text())
        assert state["state"] == "rolled_back"
        assert state["phase"] == "post-start"
        assert state["finished_at"]
        calls = _calls(d)
        assert "install-2.2.1" in calls
        assert "patch-2.2.1" in calls
        assert "restart-kill" in calls  # the kill-mode restart primitive
        # the entrypoint keeps serving the verified old runtime
        _term_worker(d)
        out, _ = proc.communicate(timeout=30)
        assert proc.returncode == 0
        assert "Restored runtime verified" in out.decode()
    finally:
        _cleanup(proc)


def test_recovery_verification_failure_broken_old(tmp_path):
    """Relaunched old runtime dies -> verification fails -> FATAL, exit 1."""
    d = tmp_path / "work"
    d.mkdir()
    state_file, pending = _seed(d, "2.2.2", "2.2.2")
    (d / "broken-new").write_text("")
    (d / "broken-old").write_text("")
    proc = _start(d)
    out, _ = proc.communicate(timeout=CASE_BUDGET_S)
    text = out.decode()
    assert proc.returncode == 1, text
    assert "FATAL" in text
    state = json.loads(state_file.read_text())
    assert state["state"] == "rollback_failed"
    # pending is KEPT for manual retry, and still marks the recovery intent
    assert pending.exists()
    assert json.loads(pending.read_text())["expect"] == "old"
    assert "reason=restore-verification-failed" in text
    # and it must never be read as a successful recovery
    assert '"state": "rolled_back"' not in state_file.read_text()


def test_recovery_verification_failure_die_in_settle(tmp_path):
    """Old runtime crashes inside the settle window -> FATAL, exit 1."""
    d = tmp_path / "work"
    d.mkdir()
    state_file, pending = _seed(d, "2.2.2", "2.2.2")
    (d / "broken-new").write_text("")
    (d / "die-in-settle").write_text("")
    proc = _start(d, settle=3)
    out, _ = proc.communicate(timeout=CASE_BUDGET_S)
    text = out.decode()
    assert proc.returncode == 1, text
    assert "FATAL" in text
    state = json.loads(state_file.read_text())
    assert state["state"] == "rollback_failed"
    assert pending.exists()
    assert "settle" in text  # the settle window is what caught the late crash
    assert "reason=restore-verification-failed" in text


def test_boot_check_rollback_end_to_end(tmp_path):
    """venv mismatch at boot -> deferred rollback (expect=old) -> verified."""
    d = tmp_path / "work"
    d.mkdir()
    state_file, pending = _seed(d, "2.2.1", "2.2.1")  # target install never landed
    proc = _start(d)
    try:
        _wait_until(lambda: not pending.exists() and
                    json.loads(state_file.read_text())["state"] == "rolled_back",
                    what="recovery finalization")
        state = json.loads(state_file.read_text())
        assert state["state"] == "rolled_back"
        assert state["phase"] == "post-start"
        assert state["finished_at"]
        calls = _calls(d)
        # boot-check installed the old version exactly once; the post-start
        # expect branch verified WITHOUT reinstalling (I3) and never needed
        # a restart (the app was booted by the entrypoint itself)
        assert calls.count("install-2.2.1") == 1
        assert calls.count("patch-2.2.1") == 1
        assert "restart-kill" not in calls
        _term_worker(d)
        out, _ = proc.communicate(timeout=30)
        assert proc.returncode == 0
    finally:
        _cleanup(proc)
