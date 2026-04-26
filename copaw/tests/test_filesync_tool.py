import json
import subprocess

import pytest

from copaw_worker.hooks.tools.filesync import filesync
from copaw_worker.sync import FileSync


def _response_json(response):
    return json.loads(response.content[0].text)


def _sync(tmp_path):
    local_dir = tmp_path / "worker"
    workspace_dir = local_dir / ".copaw" / "workspaces" / "default"
    return FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="hiclaw-storage",
        worker_name="dag-team-dev",
        local_dir=local_dir,
        shared_dir=workspace_dir / "shared",
        global_shared_dir=workspace_dir / "global-shared",
    )


def _mock_hiclaw_worker(monkeypatch, payload):
    monkeypatch.setattr("shutil.which", lambda name: "/usr/local/bin/hiclaw" if name == "hiclaw" else None)

    def fake_run(cmd, **kwargs):
        assert cmd == ["/usr/local/bin/hiclaw", "get", "workers", "dag-team-dev", "-o", "json"]
        return subprocess.CompletedProcess(
            cmd,
            0,
            stdout=json.dumps(payload),
            stderr="",
        )

    monkeypatch.setattr(subprocess, "run", fake_run)


def test_resolve_shared_path_uses_team_remote_from_hiclaw_cli(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    _mock_hiclaw_worker(monkeypatch, {"name": "dag-team-dev", "team": "dag-team"})

    resolved = sync.resolve_shared_path("shared/tasks/st-01/result.md")

    assert resolved.kind == "shared"
    assert resolved.subpath == "tasks/st-01/result.md"
    assert resolved.local == sync.shared_dir / "tasks" / "st-01" / "result.md"
    assert resolved.remote == "hiclaw/hiclaw-storage/teams/dag-team/shared/tasks/st-01/result.md"


def test_resolve_shared_path_uses_global_remote_for_standalone_worker(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    _mock_hiclaw_worker(monkeypatch, {"name": "dag-team-dev", "team": "", "role": "worker"})

    resolved = sync.resolve_shared_path("shared/tasks/st-01/result.md")

    assert resolved.remote == "hiclaw/hiclaw-storage/shared/tasks/st-01/result.md"


def test_resolve_shared_path_fails_closed_when_hiclaw_cli_is_missing(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    (sync.local_dir / "SOUL.md").write_text("You are the Team Leader of `wrong-team`.")
    monkeypatch.setattr("shutil.which", lambda name: None)

    with pytest.raises(RuntimeError, match="cannot resolve worker storage scope"):
        sync.resolve_shared_path("shared/tasks/st-01/result.md")


def test_is_team_leader_uses_hiclaw_role(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    (sync.local_dir / "AGENTS.md").write_text("No prompt text should define role.")
    _mock_hiclaw_worker(
        monkeypatch,
        {"name": "dag-team-dev", "team": "dag-team", "role": "team_leader"},
    )

    assert sync._is_team_leader() is True


def test_resolve_shared_path_rejects_parent_segments(tmp_path):
    sync = _sync(tmp_path)

    with pytest.raises(ValueError, match="must not contain"):
        sync.resolve_shared_path("shared/tasks/../secret")


def test_push_shared_path_rejects_global_shared(tmp_path):
    sync = _sync(tmp_path)
    target = sync.global_shared_dir / "tasks" / "st-01" / "result.md"
    target.parent.mkdir(parents=True)
    target.write_text("done")

    with pytest.raises(ValueError, match="read-only"):
        sync.push_shared_path("global-shared/tasks/st-01/result.md")


@pytest.mark.asyncio
async def test_filesync_dry_run_returns_resolved_local_path(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    monkeypatch.setenv("HICLAW_WORKER_NAME", "dag-team-dev")
    monkeypatch.setenv("HICLAW_FS_ENDPOINT", "http://minio:9000")
    monkeypatch.setenv("HICLAW_FS_ACCESS_KEY", "minio")
    monkeypatch.setenv("HICLAW_FS_SECRET_KEY", "password")
    _mock_hiclaw_worker(monkeypatch, {"name": "dag-team-dev", "team": "dag-team"})

    response = await filesync(
        action="pull",
        path="shared/tasks/st-01/",
        dryRun=True,
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["dryRun"] is True
    assert payload["action"] == "pull"
    assert payload["kind"] == "shared"
    assert payload["localPath"].endswith(".copaw/workspaces/default/shared/tasks/st-01")


@pytest.mark.asyncio
async def test_filesync_accepts_action_payload_and_json_string_exclude(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    monkeypatch.setenv("HICLAW_WORKER_NAME", "dag-team-dev")
    monkeypatch.setenv("HICLAW_FS_ENDPOINT", "http://minio:9000")
    monkeypatch.setenv("HICLAW_FS_ACCESS_KEY", "minio")
    monkeypatch.setenv("HICLAW_FS_SECRET_KEY", "password")
    _mock_hiclaw_worker(monkeypatch, {"name": "dag-team-dev", "team": "dag-team"})

    response = await filesync(
        action="push",
        payload={
            "path": "shared/tasks/st-01/",
            "exclude": json.dumps(["spec.md", "meta.json", "base/"]),
        },
        dryRun=True,
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["path"] == "shared/tasks/st-01/"
    assert payload["exclude"] == ["spec.md", "meta.json", "base/"]


@pytest.mark.asyncio
async def test_filesync_rejects_invalid_action(tmp_path, monkeypatch):
    monkeypatch.setenv("COPAW_WORKING_DIR", str(tmp_path / "worker" / ".copaw"))
    monkeypatch.setenv("HICLAW_WORKER_NAME", "dag-team-dev")
    monkeypatch.setenv("HICLAW_FS_ENDPOINT", "http://minio:9000")
    monkeypatch.setenv("HICLAW_FS_ACCESS_KEY", "minio")
    monkeypatch.setenv("HICLAW_FS_SECRET_KEY", "password")

    response = await filesync(action="complete_task", path="shared/tasks/st-01/")
    payload = _response_json(response)

    assert payload["ok"] is False
    assert "action must be one of" in payload["error"]
