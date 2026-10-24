"""Upgrade job lifecycle: state file, detached two-phase spawn, status reads.

Design doc §4.5-4.8. The runner never does pip/pkill itself — it validates
the request, pre-downloads the target wheel (mandatory C3 input), then spawns
``upgrade_qwenpaw_runtime.sh --phase pre-restart`` as a DETACHED child
(``start_new_session``) so the orchestrator keeps running after the plugin's
app process is killed in S4. All results are re-read from the state file at
``$QWENPAW_WORKING_DIR/upgrade/`` (single source of truth across container
restarts); the endpoint layer only translates the typed exceptions below
into flat JSON bodies.
"""
from __future__ import annotations

import json
import os
import subprocess
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Optional

try:  # guarded: the module must import even where pydantic is absent
    from pydantic import BaseModel

    class UpgradeRunBody(BaseModel):
        """``POST /api/upgrade/run`` request body (design §4.5)."""

        version: str
        mode: str = "in-container"
        venv: Optional[str] = None
        restart_mode: Optional[str] = None
        container: Optional[str] = None
        index_url: Optional[str] = None
        timeout: int = 90
        settle: int = 65
        skip_check: bool = False

except ImportError:  # pragma: no cover - plugin host always has pydantic
    UpgradeRunBody = None  # type: ignore[assignment]

from qwenpaw_worker.upgrade import versions


class UpgradeError(Exception):
    """Base class for upgrade request failures (message goes into the body)."""


class UpgradeRejected(UpgradeError):
    """400 — invalid / below-current request, or preflight download failed."""


class UpgradeUnsupported(UpgradeError):
    """501 — interface reserved, implementation not present (mode=docker)."""


class UpgradeInProgress(UpgradeError):
    """409 — a job is already running."""

    def __init__(self, job_id: str) -> None:
        super().__init__("upgrade_in_progress")
        self.job_id = job_id


class UpgradeNotFound(UpgradeError):
    """404 — unknown job_id."""

    def __init__(self, job_id: str) -> None:
        super().__init__("job_not_found")
        self.job_id = job_id


@dataclass
class JobState:
    """The on-disk state file record (subset that endpoints surface)."""

    job_id: str
    # running|boot-check-passed|recovering|success|rolled_back|rollback_failed|precheck_failed
    # `recovering` is the NON-terminal rollback-recovery state: it stays on
    # disk (pending.json kept, expect=old) until the restored OLD runtime
    # passes its readiness+settle verification, which alone finalizes
    # rolled_back + pending removal.
    state: str = "running"
    exit_code: Optional[int] = None
    current: str = ""
    target: str = ""
    phase: str = ""
    started_at: str = ""
    finished_at: str = ""
    pid: Optional[int] = None
    extra: dict = field(default_factory=dict)

    def to_dict(self) -> dict:
        data = dict(self.extra)
        data.update({
            "job_id": self.job_id,
            "state": self.state,
            "exit_code": self.exit_code,
            "current": self.current,
            "target": self.target,
            "phase": self.phase,
            "started_at": self.started_at,
            "finished_at": self.finished_at,
            "pid": self.pid,
        })
        return data


# ---------------------------------------------------------------------------
# path conventions
# ---------------------------------------------------------------------------

def state_dir() -> Path:
    """``$QWENPAW_WORKING_DIR/upgrade`` — created on demand."""
    root = Path(os.environ.get("QWENPAW_WORKING_DIR", ".")) / "upgrade"
    root.mkdir(parents=True, exist_ok=True)
    return root


def _state_path(job_id: str) -> Path:
    return state_dir() / f"upgrade-{job_id}.json"


def _log_path(job_id: str) -> Path:
    return state_dir() / f"upgrade-{job_id}.log"


def _pending_path() -> Path:
    return state_dir() / "pending.json"


def _now_iso() -> str:
    from datetime import datetime, timezone

    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _job_stamp() -> str:
    from datetime import datetime, timezone

    return datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")


def _read_state(job_id: str) -> Optional[dict]:
    path = _state_path(job_id)
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None


def _write_state(job_id: str, data: dict) -> None:
    tmp = _state_path(job_id).with_suffix(".json.tmp")
    tmp.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    os.replace(str(tmp), str(_state_path(job_id)))


# States a job leaves behind forever. Everything else on disk (running,
# boot-check-passed, recovering, ...) is still in flight: the two-phase
# window runs across a container restart, so the mutex must cover boot-check
# and the post-start health gate, not just the pre-restart process lifetime.
# `recovering` is likewise NON-terminal: a rollback whose restored OLD
# runtime has not yet passed its readiness+settle verification keeps the
# single-upgrade mutex engaged; only a PASSED verification finalizes
# rolled_back (and removes pending.json).
_TERMINAL_STATES = frozenset(
    {"success", "rolled_back", "rollback_failed", "precheck_failed"})


def active_job() -> Optional[dict]:
    """The newest job that has not reached a terminal state (a live pid is
    not required — the orchestrator exits 0 after S4 while the app is
    restarting, and the phase-2 boot-check / post-start gate own completion).
    ``boot-check-passed`` and ``recovering`` are deliberately NON-terminal:
    during the post-start health gate — success or rollback verification —
    the job is still in flight and a second /run must 409."""
    best: Optional[tuple[str, dict]] = None
    for record in _iter_jobs():
        if record.get("state") in _TERMINAL_STATES:
            continue
        key = (record.get("started_at") or "", record.get("job_id") or "")
        if best is None or key > (best[0], best[1]):
            best = (key[0], record)
    return best[1] if best else None


def _iter_jobs() -> list[dict]:
    jobs = []
    for path in sorted(state_dir().glob("upgrade-upg-*.json")):
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        if isinstance(data, dict) and data.get("job_id"):
            jobs.append(data)
    return jobs


# ---------------------------------------------------------------------------
# orchestrator resolution
# ---------------------------------------------------------------------------

_ORCHESTRATOR_IMAGE = "/opt/agentteams/scripts/upgrade_qwenpaw_runtime.sh"


def orchestrator_path() -> str:
    """Env override -> image location -> repo co-location (dev/tests)."""
    override = os.environ.get("AGENTTEAMS_UPGRADE_SCRIPT")
    if override:
        return override
    if Path(_ORCHESTRATOR_IMAGE).is_file():
        return _ORCHESTRATOR_IMAGE
    return str(Path(__file__).resolve().parents[3] / "scripts" / "upgrade_qwenpaw_runtime.sh")


# ---------------------------------------------------------------------------
# lifecycle
# ---------------------------------------------------------------------------

def start_upgrade(body: "UpgradeRunBody") -> dict:
    """Validate, record, and detach the pre-restart phase.

    Returns ``{"job_id", "state", "pid", "status_url"}`` (the 202 body).
    Raises ``UpgradeUnsupported`` / ``UpgradeRejected`` / ``UpgradeInProgress``.
    """
    if body.mode == "docker":
        raise UpgradeUnsupported("mode=docker interface reserved; in-container only for now")
    target = (body.version or "").strip()
    if not target:
        raise UpgradeRejected("version is required")
    if not versions.is_valid_version(target):
        raise UpgradeRejected(f"invalid version: {target!r}")

    current, _source = versions.current_version()
    try:
        from qwenpaw_worker.upgrade.checker import _version_at_least

        if not _version_at_least(target, current) or target == current:
            raise UpgradeRejected(
                f"target {target} is not above current {current}")
    except UpgradeRejected:
        raise
    except Exception:
        pass  # current unknown -> allow, pre-checks will adjudicate

    running = active_job()
    if running:
        raise UpgradeInProgress(running.get("job_id") or "")

    # job_id: upg-YYYYMMDD-HHMMSS-xxxx (timestamp = stable ordering + readable)
    job_id = f"upg-{_job_stamp()}-{uuid.uuid4().hex[:4]}"
    started = _now_iso()

    venv = body.venv or "/opt/venv/qwenpaw"
    restart_mode = body.restart_mode or "kill"
    wheelhouse = state_dir() / "wheelhouse"

    state = {
        "job_id": job_id,
        "state": "running",
        "exit_code": None,
        "current": current,
        "target": target,
        "phase": "pre-restart",
        "started_at": started,
        "finished_at": "",
        "pid": None,
        "venv": venv,
        "restart_mode": restart_mode,
        "index_url": body.index_url,
    }
    _write_state(job_id, state)

    # Mandatory C3 input: the wheel must exist in the wheelhouse BEFORE the
    # orchestrator exits 0 (the app dies after S4 and cannot download).
    if not body.skip_check:
        try:
            from qwenpaw_worker.upgrade import checker

            checker.download_wheel(target, wheelhouse, index_url=body.index_url)
        except Exception as exc:
            _mark_failed(job_id, "precheck_failed",
                         f"wheel pre-download failed: {exc}")
            raise UpgradeRejected(f"wheel pre-download failed: {exc}") from exc

    log_file = open(_log_path(job_id), "ab")
    cmd = [
        "bash", orchestrator_path(),
        "--target", target,
        "--phase", "pre-restart",
        "--job-id", job_id,
        "--state-file", str(_state_path(job_id)),
        "--pending-file", str(_pending_path()),
        "--venv", venv,
        "--restart-mode", restart_mode,
        "--timeout", str(body.timeout),
        "--settle", str(body.settle),
    ]
    if body.skip_check:
        cmd.append("--skip-check")
    env = dict(os.environ)
    env["UG_WHEELHOUSE"] = str(wheelhouse)
    try:
        proc = subprocess.Popen(
            cmd, stdout=log_file, stderr=subprocess.STDOUT,
            stdin=subprocess.DEVNULL, start_new_session=True, env=env,
        )
    except Exception as exc:
        log_file.close()
        _mark_failed(job_id, "precheck_failed", f"spawn failed: {exc}")
        raise UpgradeRejected(f"spawn failed: {exc}") from exc
    log_file.close()

    state["pid"] = proc.pid
    _write_state(job_id, state)
    return {
        "job_id": job_id,
        "state": "running",
        "pid": proc.pid,
        "status_url": f"/api/upgrade/status?job_id={job_id}",
    }


def _mark_failed(job_id: str, state_name: str, reason: str) -> None:
    data = _read_state(job_id) or {"job_id": job_id}
    data.update({
        "state": state_name,
        "finished_at": _now_iso(),
        "note": reason,
    })
    _write_state(job_id, data)


def job_status(job_id: Optional[str] = None) -> dict:
    """``GET /api/upgrade/status`` payload (design §4.6), ``log_tail`` last 50.

    ``job_id=None`` -> the most recently started job. Raises ``UpgradeNotFound``
    when no job matches.
    """
    if job_id:
        data = _read_state(job_id)
        if data is None:
            raise UpgradeNotFound(job_id)
    else:
        jobs = _iter_jobs()
        if not jobs:
            raise UpgradeNotFound("")
        jobs.sort(key=lambda d: (d.get("started_at") or "", d.get("job_id") or ""))
        data = jobs[-1]

    return {
        "job_id": data.get("job_id", ""),
        "state": data.get("state", "running"),
        "exit_code": data.get("exit_code"),
        "current": data.get("current", ""),
        "target": data.get("target", ""),
        "phase": data.get("phase", ""),
        "started_at": data.get("started_at", ""),
        "finished_at": data.get("finished_at", ""),
        "log_tail": log_tail(_log_path(str(data.get("job_id", "")))),
    }


def log_tail(path: Path, n: int = 50) -> list[str]:
    """Last ``n`` non-empty lines of an orchestrator log (best effort)."""
    try:
        lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError:
        return []
    kept = [line for line in lines if line.strip()]
    return kept[-n:]
