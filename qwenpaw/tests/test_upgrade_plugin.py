"""agentteams-upgrade plugin: registration + endpoint contracts (no network).

Loads the plugin module directly (spec-load, like the checker wrapper
tests) and exercises the registered routes by calling their endpoint
coroutines with a fake ``api``. The /run 202 path uses a fake orchestrator
script via ``AGENTTEAMS_UPGRADE_SCRIPT`` and ``skip_check=True`` so no
network or real venv is touched.
"""
from __future__ import annotations

import asyncio
import importlib.util
import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]  # .../AgentTeams-runtime-upgrade/qwenpaw
SRC = REPO_ROOT / "src"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))

PLUGIN = REPO_ROOT.parent / "plugins" / "agentteams-upgrade" / "plugin.py"


def _load_plugin():
    spec = importlib.util.spec_from_file_location("agentteams_upgrade_plugin", PLUGIN)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class _FakeApi:
    def __init__(self):
        self.calls = []

    def register_http_router(self, router, prefix=None, tags=None):
        self.calls.append({"router": router, "prefix": prefix, "tags": tags})


def _register():
    module = _load_plugin()
    api = _FakeApi()
    module.plugin.register(api)
    assert len(api.calls) == 1
    return {r.path: r for r in api.calls[0]["router"].routes}, api


def _call(handler, **kwargs):
    return asyncio.run(handler(**kwargs))


def _body(resp) -> dict:
    return json.loads(resp.body.decode())


def test_registers_upgrade_router_with_three_routes():
    routes, api = _register()
    assert api.calls[0]["prefix"] == "/upgrade"
    assert api.calls[0]["tags"] == ["agentteams-upgrade"]
    assert set(routes) == {"/check", "/run", "/status"}
    assert routes["/check"].methods == {"GET"}
    assert routes["/run"].methods == {"POST"}
    assert routes["/status"].methods == {"GET"}


def test_status_endpoint_reads_state_file(tmp_path, monkeypatch):
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    job = "upg-20261012-000000-abcd"
    up = tmp_path / "upgrade"
    up.mkdir()
    (up / f"upgrade-{job}.json").write_text(json.dumps({
        "job_id": job,
        "state": "success",
        "exit_code": 0,
        "current": "2.2.1",
        "target": "2.2.2",
        "phase": "boot-check",
        "started_at": "2026-10-12T00:00:00Z",
        "finished_at": "2026-10-12T00:01:00Z",
    }))
    (up / f"upgrade-{job}.log").write_text("line1\n\nline2\n")

    routes, _ = _register()
    resp = _call(routes["/status"].endpoint, job_id=job)
    assert resp.status_code == 200
    data = _body(resp)
    assert data["job_id"] == job
    assert data["state"] == "success"
    assert data["current"] == "2.2.1"
    assert data["target"] == "2.2.2"
    assert data["log_tail"] == ["line1", "line2"]


def test_status_unknown_job_404(tmp_path, monkeypatch):
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    routes, _ = _register()
    resp = _call(routes["/status"].endpoint, job_id="upg-20260101-000000-zzzz")
    assert resp.status_code == 404
    assert _body(resp) == {"error": "job_not_found", "job_id": "upg-20260101-000000-zzzz"}


def test_run_endpoint_202_spawns_orchestrator(tmp_path, monkeypatch):
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    from qwenpaw_worker.upgrade import versions

    # deterministic current version (no live API in the test env)
    monkeypatch.setattr(versions, "current_version",
                        lambda: ("2.2.1", "importlib.metadata"))

    fake_script = tmp_path / "fake-orchestrator.sh"
    fake_script.write_text("#!/bin/bash\necho fake-orchestrator ran\nexit 0\n")
    monkeypatch.setenv("AGENTTEAMS_UPGRADE_SCRIPT", str(fake_script))

    from qwenpaw_worker.upgrade import runner

    routes, _ = _register()
    body = runner.UpgradeRunBody(version="2.2.2", skip_check=True)
    resp = _call(routes["/run"].endpoint, body=body)
    assert resp.status_code == 202
    data = _body(resp)
    assert data["job_id"].startswith("upg-")
    assert data["state"] == "running"
    assert isinstance(data["pid"], int)
    assert data["status_url"] == f"/api/upgrade/status?job_id={data['job_id']}"
    state_file = tmp_path / "upgrade" / f"upgrade-{data['job_id']}.json"
    assert state_file.is_file()
    on_disk = json.loads(state_file.read_text())
    assert on_disk["state"] == "running"
    assert on_disk["target"] == "2.2.2"


def test_run_409_when_job_running(tmp_path, monkeypatch):
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    from qwenpaw_worker.upgrade import runner, versions

    monkeypatch.setattr(versions, "current_version",
                        lambda: ("2.2.1", "importlib.metadata"))
    up = tmp_path / "upgrade"
    up.mkdir()
    (up / "upgrade-upg-20260101-000000-aaaa.json").write_text(json.dumps({
        "job_id": "upg-20260101-000000-aaaa",
        "state": "running",
        "started_at": "2026-01-01T00:00:00Z",
    }))

    routes, _ = _register()
    body = runner.UpgradeRunBody(version="2.2.2", skip_check=True)
    resp = _call(routes["/run"].endpoint, body=body)
    assert resp.status_code == 409
    assert _body(resp) == {
        "error": "upgrade_in_progress",
        "job_id": "upg-20260101-000000-aaaa",
    }


def test_run_400_target_not_above_current(tmp_path, monkeypatch):
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    from qwenpaw_worker.upgrade import runner, versions

    monkeypatch.setattr(versions, "current_version",
                        lambda: ("2.5.0", "importlib.metadata"))
    routes, _ = _register()
    body = runner.UpgradeRunBody(version="2.2.2", skip_check=True)
    resp = _call(routes["/run"].endpoint, body=body)
    assert resp.status_code == 400
    assert "not above current" in _body(resp)["error"]


def test_run_501_docker_mode_reserved(tmp_path, monkeypatch):
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    from qwenpaw_worker.upgrade import runner

    routes, _ = _register()
    body = runner.UpgradeRunBody(version="2.2.2", mode="docker")
    resp = _call(routes["/run"].endpoint, body=body)
    assert resp.status_code == 501
    assert "docker" in _body(resp)["error"]


def test_active_job_returns_intermediate_states(tmp_path, monkeypatch):
    """P1-1 / round 3: between boot-check-passed and the post-start verdict —
    and through the whole `recovering` rollback-recovery window — the job is
    still in flight; active_job() must keep reporting it (409 source)."""
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    from qwenpaw_worker.upgrade import runner

    up = tmp_path / "upgrade"
    up.mkdir()
    job = "upg-20261014-000000-cccc"
    (up / f"upgrade-{job}.json").write_text(json.dumps({
        "job_id": job,
        "state": "boot-check-passed",  # post-start health gate running
        "started_at": "2026-10-14T00:00:00Z",
    }))
    job2 = "upg-20261015-000000-cccc"
    (up / f"upgrade-{job2}.json").write_text(json.dumps({
        "job_id": job2,
        "state": "recovering",  # rollback verification in flight
        "started_at": "2026-10-15T00:00:00Z",
    }))
    active = runner.active_job()
    assert active is not None
    # both are non-terminal; the newest in-flight job is the mutex holder
    assert active["job_id"] == job2


def test_active_job_ignores_terminal_states(tmp_path, monkeypatch):
    """P1-1 regression: once a job reaches a terminal state it frees the
    mutex again, even when several terminal jobs are on disk."""
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    from qwenpaw_worker.upgrade import runner

    up = tmp_path / "upgrade"
    up.mkdir()
    for job, state in {
        "upg-20260101-000000-succ": "success",
        "upg-20260102-000000-roll": "rolled_back",
        "upg-20260103-000000-rfal": "rollback_failed",
        "upg-20260104-000000-pref": "precheck_failed",
    }.items():
        (up / f"upgrade-{job}.json").write_text(json.dumps({
            "job_id": job,
            "state": state,
            "started_at": "2026-01-01T00:00:00Z",
        }))
    assert runner.active_job() is None


def test_run_409_during_post_start_verification(tmp_path, monkeypatch):
    """P1-1 (shiyiyue case): a second /run while the first job is inside its
    post-start health gate must be rejected with 409 (the whole two-phase
    window is the mutex, not just the pre-restart process lifetime)."""
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    from qwenpaw_worker.upgrade import runner, versions

    monkeypatch.setattr(versions, "current_version",
                        lambda: ("2.2.1", "importlib.metadata"))
    up = tmp_path / "upgrade"
    up.mkdir()
    job = "upg-20261014-000000-dddd"
    (up / f"upgrade-{job}.json").write_text(json.dumps({
        "job_id": job,
        "state": "boot-check-passed",
        "started_at": "2026-10-14T00:00:00Z",
    }))

    routes, _ = _register()
    body = runner.UpgradeRunBody(version="2.2.2", skip_check=True)
    resp = _call(routes["/run"].endpoint, body=body)
    assert resp.status_code == 409
    assert _body(resp) == {"error": "upgrade_in_progress", "job_id": job}


def test_run_409_during_recovering_verification(tmp_path, monkeypatch):
    """Round 3: a second /run while a job is in `recovering` (the restored
    old runtime has not yet passed its readiness+settle verification) must be
    rejected with 409 — the mutex covers the whole recovery window, not just
    the pre-restart process lifetime."""
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    from qwenpaw_worker.upgrade import runner, versions

    monkeypatch.setattr(versions, "current_version",
                        lambda: ("2.2.1", "importlib.metadata"))
    up = tmp_path / "upgrade"
    up.mkdir()
    job = "upg-20261015-000000-eeee"
    (up / f"upgrade-{job}.json").write_text(json.dumps({
        "job_id": job,
        "state": "recovering",
        "started_at": "2026-10-15T00:00:00Z",
    }))

    routes, _ = _register()
    body = runner.UpgradeRunBody(version="2.2.2", skip_check=True)
    resp = _call(routes["/run"].endpoint, body=body)
    assert resp.status_code == 409
    assert _body(resp) == {"error": "upgrade_in_progress", "job_id": job}


def test_check_endpoint_delegates_to_run_check(tmp_path, monkeypatch):
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path))
    from qwenpaw_worker.upgrade import checker

    fixed = {
        "current": "2.2.1",
        "current_source": "importlib.metadata",
        "checked_at": "2026-10-12T00:00:00Z",
        "index": "pypi",
        "candidates": [],
        "errors": [],
    }
    monkeypatch.setattr(checker, "run_check", lambda **kwargs: fixed)

    routes, _ = _register()
    resp = _call(routes["/check"].endpoint,
                 verify=False, include_prerelease=False, index_url=None)
    assert resp.status_code == 200
    assert _body(resp) == fixed
