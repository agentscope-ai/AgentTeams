import json
import shutil
import subprocess

import pytest

from copaw_worker.hooks.tools.filesync import create_sync, filesync
from copaw_worker.sync import FileSync, _team_storage_name_from_worker_team


def _response_json(response):
    content = response.content[0]
    text = content["text"] if isinstance(content, dict) else content.text
    return json.loads(text)


def _sync(tmp_path):
    local_dir = tmp_path / "worker"
    workspace_dir = local_dir / ".copaw" / "workspaces" / "default"
    return FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="agentteams-storage",
        worker_name="dag-team-dev",
        local_dir=local_dir,
        shared_dir=workspace_dir / "shared",
        global_shared_dir=workspace_dir / "global-shared",
    )


def _mock_hiclaw_worker(monkeypatch, payload, expected_name="dag-team-dev"):
    original_which = shutil.which
    monkeypatch.setattr(
        "shutil.which",
        lambda name: "/usr/local/bin/agt" if name == "agt" else original_which(name),
    )

    def fake_run(cmd, **kwargs):
        if cmd == ["/usr/local/bin/agt", "get", "workers", expected_name, "-o", "json"]:
            return subprocess.CompletedProcess(
                cmd,
                0,
                stdout=json.dumps(payload),
                stderr="",
            )
        raise AssertionError(f"unexpected command: {cmd}")

    monkeypatch.setattr(subprocess, "run", fake_run)


def test_create_sync_accepts_legacy_hiclaw_environment(tmp_path, monkeypatch):
    monkeypatch.setenv("COPAW_WORKING_DIR", str(tmp_path / "worker" / ".copaw"))
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "legacy-worker")
    monkeypatch.setenv("AGENTTEAMS_WORKER_CR_NAME", "legacy-worker-cr")
    monkeypatch.setenv("AGENTTEAMS_FS_ENDPOINT", "http://minio:9000")
    monkeypatch.setenv("AGENTTEAMS_FS_ACCESS_KEY", "minio")
    monkeypatch.setenv("AGENTTEAMS_FS_SECRET_KEY", "password")
    monkeypatch.setenv("AGENTTEAMS_FS_BUCKET", "agentteams-storage")

    sync = create_sync()

    assert sync.worker_name == "legacy-worker"
    assert sync.worker_cr_name == "legacy-worker-cr"
    assert sync.endpoint == "http://minio:9000"
    assert sync.bucket == "agentteams-storage"


def test_resolve_shared_path_strips_bucket_prefix_from_worker_team(tmp_path, monkeypatch):
    sync = FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="hiclaw-magic-cn-123",
        worker_name="dag-team-dev",
        local_dir=tmp_path / "worker",
    )
    _mock_hiclaw_worker(
        monkeypatch,
        {"name": "dag-team-dev", "team": "magic-cn-123-dag-team"},
    )

    resolved = sync.resolve_shared_path("shared/tasks/st-01/result.md")

    assert resolved.kind == "shared"
    assert resolved.subpath == "tasks/st-01/result.md"
    assert resolved.local == sync.shared_dir / "tasks" / "st-01" / "result.md"
    assert resolved.remote == "agentteams/hiclaw-magic-cn-123/teams/dag-team/shared/tasks/st-01/result.md"


def test_team_storage_name_keeps_legacy_team_without_bucket_prefix():
    assert _team_storage_name_from_worker_team("agentteams-storage", "dag-team") == "dag-team"


def test_worker_metadata_query_uses_cr_name_while_storage_uses_runtime_name(tmp_path, monkeypatch):
    sync = FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="agentteams-storage",
        worker_name="novworker02",
        worker_cr_name="nov-worker-cr",
        local_dir=tmp_path / "worker",
    )
    _mock_hiclaw_worker(
        monkeypatch,
        {"name": "nov-worker-cr", "workerName": "novworker02", "team": "dag-team"},
        expected_name="nov-worker-cr",
    )
    commands = []

    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr("copaw_worker.sync._mc", fake_mc)

    sync.mirror_all()

    assert sync._prefix == "agents/novworker02"
    assert commands[0] == (
        "mirror",
        "agentteams/agentteams-storage/agents/novworker02/",
        f"{sync.local_dir}/",
        "--overwrite",
        "--exclude",
        "credentials/**",
    )


def test_resolve_shared_path_uses_global_remote_for_standalone_worker(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    _mock_hiclaw_worker(monkeypatch, {"name": "dag-team-dev", "team": "", "role": "worker"})

    resolved = sync.resolve_shared_path("shared/tasks/st-01/result.md")

    assert resolved.remote == "agentteams/agentteams-storage/shared/tasks/st-01/result.md"


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
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "dag-team-dev")
    monkeypatch.setenv("AGENTTEAMS_FS_ENDPOINT", "http://minio:9000")
    monkeypatch.setenv("AGENTTEAMS_FS_ACCESS_KEY", "minio")
    monkeypatch.setenv("AGENTTEAMS_FS_SECRET_KEY", "password")
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
async def test_filesync_normalizes_project_directory_without_trailing_slash(
    tmp_path,
    monkeypatch,
):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "dag-team-dev")
    monkeypatch.setenv("AGENTTEAMS_FS_ENDPOINT", "http://minio:9000")
    monkeypatch.setenv("AGENTTEAMS_FS_ACCESS_KEY", "minio")
    monkeypatch.setenv("AGENTTEAMS_FS_SECRET_KEY", "password")
    _mock_hiclaw_worker(monkeypatch, {"name": "dag-team-dev", "team": "dag-team"})

    response = await filesync(
        action="pull",
        path="shared/projects/project-20260512-001122",
        dryRun=True,
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["path"] == "shared/projects/project-20260512-001122/"
    assert payload["localPath"].endswith(
        ".copaw/workspaces/default/shared/projects/project-20260512-001122"
    )


@pytest.mark.asyncio
async def test_filesync_accepts_action_payload_and_json_string_exclude(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "dag-team-dev")
    monkeypatch.setenv("AGENTTEAMS_FS_ENDPOINT", "http://minio:9000")
    monkeypatch.setenv("AGENTTEAMS_FS_ACCESS_KEY", "minio")
    monkeypatch.setenv("AGENTTEAMS_FS_SECRET_KEY", "password")
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
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "dag-team-dev")
    monkeypatch.setenv("AGENTTEAMS_FS_ENDPOINT", "http://minio:9000")
    monkeypatch.setenv("AGENTTEAMS_FS_ACCESS_KEY", "minio")
    monkeypatch.setenv("AGENTTEAMS_FS_SECRET_KEY", "password")

    response = await filesync(action="complete_task", path="shared/tasks/st-01/")
    payload = _response_json(response)

    assert payload["ok"] is False
    assert "action must be one of" in payload["error"]
