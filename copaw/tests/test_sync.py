import asyncio
import json
import logging
import subprocess

import pytest

from copaw_worker.sync import BridgeRuntimeError, FileSync, _mc, push_local, sync_loop


@pytest.fixture
def anyio_backend():
    return "asyncio"


def _sync(tmp_path):
    return FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="agentteams-storage",
        worker_name="dag-team-dev",
        local_dir=tmp_path / "worker",
    )


def test_mc_failure_redacts_alias_credentials_and_logs_stderr(monkeypatch, caplog):
    raw_secret = "raw-secret-value"
    raw_access = "raw-access-key"

    monkeypatch.setattr("copaw_worker.sync.shutil.which", lambda _name: "/usr/local/bin/mc")

    def fail_run(cmd, **_kwargs):
        raise subprocess.CalledProcessError(
            1,
            cmd,
            output="",
            stderr="AccessDenied: invalid access key",
        )

    monkeypatch.setattr("copaw_worker.sync.subprocess.run", fail_run)
    caplog.set_level(logging.INFO)

    with pytest.raises(subprocess.CalledProcessError) as exc_info:
        _mc("alias", "set", "hiclaw", "https://oss.example.com", raw_access, raw_secret)

    assert raw_access not in caplog.text
    assert raw_secret not in caplog.text
    assert raw_access not in str(exc_info.value)
    assert raw_secret not in str(exc_info.value)
    assert "AccessDenied: invalid access key" in caplog.text
    assert exc_info.value.stderr == "AccessDenied: invalid access key"


def test_cat_missing_object_is_debug_only(tmp_path, monkeypatch, caplog):
    sync = _sync(tmp_path)
    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)

    def fake_mc(*_args, **_kwargs):
        return subprocess.CompletedProcess(
            _args,
            1,
            stdout="",
            stderr="mc.bin: <ERROR> Object does not exist.",
        )

    monkeypatch.setattr("copaw_worker.sync._mc", fake_mc)
    caplog.set_level(logging.WARNING)

    assert sync._cat("agents/worker/config/mcporter.json") is None
    assert "Object does not exist" not in caplog.text


def test_cat_non_missing_failure_warns(tmp_path, monkeypatch, caplog):
    sync = _sync(tmp_path)
    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)

    def fake_mc(*_args, **_kwargs):
        return subprocess.CompletedProcess(
            _args,
            1,
            stdout="",
            stderr="AccessDenied: denied",
        )

    monkeypatch.setattr("copaw_worker.sync._mc", fake_mc)
    caplog.set_level(logging.WARNING)

    assert sync._cat("agents/worker/openclaw.json") is None
    assert "mc cat failed" in caplog.text
    assert "AccessDenied: denied" in caplog.text


def test_mirror_all_restores_worker_prefix_and_shared_without_credentials(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    commands = []

    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)
    monkeypatch.setattr(
        sync,
        "_get_worker_info",
        lambda: {"name": "dag-team-dev", "team": "dag-team", "role": "worker"},
    )
    monkeypatch.setattr(sync, "_get_team_id", lambda: "dag-team")

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr("copaw_worker.sync._mc", fake_mc)

    sync.mirror_all()

    mirror_commands = [cmd for cmd in commands if cmd[0] == "mirror"]
    assert mirror_commands == [
        (
            "mirror",
            "agentteams/agentteams-storage/agents/dag-team-dev/",
            f"{sync.local_dir}/",
            "--overwrite",
            "--exclude",
            "credentials/**",
        ),
        (
            "mirror",
            "agentteams/agentteams-storage/teams/dag-team/shared/",
            f"{sync.shared_dir}/",
            "--overwrite",
        ),
    ]


def test_mirror_all_falls_back_to_startup_files_when_prefix_missing(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    commands = []

    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        if args[0] == "mirror" and args[1].endswith("/agents/dag-team-dev/"):
            raise subprocess.CalledProcessError(
                1,
                args,
                output="",
                stderr="mc.bin: <ERROR> Object does not exist.",
            )
        if args[0] == "cat" and args[1].endswith(
            "/agents/dag-team-dev/openclaw.json"
        ):
            return subprocess.CompletedProcess(
                args,
                0,
                stdout='{"team_id":"dag-team"}',
                stderr="",
            )
        if args[0] == "cat":
            return subprocess.CompletedProcess(
                args,
                1,
                stdout="",
                stderr="mc.bin: <ERROR> Object does not exist.",
            )
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr("copaw_worker.sync._mc", fake_mc)

    sync.mirror_all()

    assert json.loads((sync.local_dir / "openclaw.json").read_text()) == {
        "team_id": "dag-team"
    }
    assert (
        "mirror",
        "agentteams/agentteams-storage/teams/dag-team/shared/",
        f"{sync.shared_dir}/",
        "--overwrite",
    ) in commands


def test_mirror_all_restores_global_shared_for_team_leader(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    commands = []

    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)
    monkeypatch.setattr(
        sync,
        "_get_worker_info",
        lambda: {"name": "dag-team-dev", "team": "dag-team", "role": "team_leader"},
    )
    monkeypatch.setattr(sync, "_get_team_id", lambda: "dag-team")

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr("copaw_worker.sync._mc", fake_mc)

    sync.mirror_all()

    mirror_commands = [cmd for cmd in commands if cmd[0] == "mirror"]
    assert mirror_commands == [
        (
            "mirror",
            "agentteams/agentteams-storage/agents/dag-team-dev/",
            f"{sync.local_dir}/",
            "--overwrite",
            "--exclude",
            "credentials/**",
        ),
        (
            "mirror",
            "agentteams/agentteams-storage/teams/dag-team/shared/",
            f"{sync.shared_dir}/",
            "--overwrite",
        ),
        (
            "mirror",
            "agentteams/agentteams-storage/shared/",
            f"{sync.global_shared_dir}/",
            "--overwrite",
        ),
    ]


def test_pull_and_push_shared_paths_are_explicit_minio_operations(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    commands = []

    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_get_shared_remote", lambda: "agentteams/agentteams-storage/teams/dag-team/shared/")

    local_report = sync.shared_dir / "tasks" / "st-01" / "report.md"
    local_report.parent.mkdir(parents=True)
    local_report.write_text("done")

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr("copaw_worker.sync._mc", fake_mc)

    sync.pull_shared_path("shared/tasks/st-01/")
    sync.push_shared_path("shared/tasks/st-01/", exclude=["base/", "*.tmp"])

    assert commands == [
        (
            "mirror",
            "agentteams/agentteams-storage/teams/dag-team/shared/tasks/st-01/",
            f"{sync.shared_dir / 'tasks' / 'st-01'}/",
            "--overwrite",
        ),
        (
            "mirror",
            f"{sync.shared_dir / 'tasks' / 'st-01'}/",
            "agentteams/agentteams-storage/teams/dag-team/shared/tasks/st-01/",
            "--overwrite",
            "--exclude",
            "base/",
            "--exclude",
            "*.tmp",
        ),
    ]


def test_pull_shared_path_falls_back_to_mirror_for_remote_directory_prefix(
    tmp_path,
    monkeypatch,
):
    sync = _sync(tmp_path)
    commands = []

    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_get_shared_remote", lambda: "agentteams/agentteams-storage/teams/dag-team/shared/")

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        if args[0] == "cp":
            raise subprocess.CalledProcessError(
                1,
                args,
                output="",
                stderr="the --recursive flag is required",
            )
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr("copaw_worker.sync._mc", fake_mc)

    sync.pull_shared_path("shared/projects/project-20260512-001122")

    assert commands == [
        (
            "cp",
            "agentteams/agentteams-storage/teams/dag-team/shared/projects/project-20260512-001122",
            str(sync.shared_dir / "projects" / "project-20260512-001122"),
        ),
        (
            "mirror",
            "agentteams/agentteams-storage/teams/dag-team/shared/projects/project-20260512-001122/",
            f"{sync.shared_dir / 'projects' / 'project-20260512-001122'}/",
            "--overwrite",
        ),
    ]


def test_pull_all_refreshes_config_mcporter_and_skills_without_shared(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    local_config = {
        "channels": {
            "matrix": {
                "accessToken": "local-token",
                "groupAllowFrom": ["@old:mx"],
            }
        }
    }
    (sync.local_dir).mkdir(parents=True, exist_ok=True)
    (sync.local_dir / "openclaw.json").write_text(
        json.dumps(local_config, indent=2),
        encoding="utf-8",
    )

    remote_config = {
        "channels": {
            "matrix": {
                "accessToken": "remote-token",
                "groupAllowFrom": ["@new:mx"],
            }
        }
    }
    commands = []

    def fake_cat(key):
        if key.endswith("/openclaw.json"):
            return json.dumps(remote_config)
        if key.endswith("/config/mcporter.json"):
            return '{"mcpServers":{}}'
        return None

    def fake_ls(prefix):
        if prefix.endswith("/skills/"):
            return ["github/SKILL.md"]
        return []

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_cat", fake_cat)
    monkeypatch.setattr(sync, "_ls", fake_ls)
    monkeypatch.setattr("copaw_worker.sync._mc", fake_mc)

    changed = sync.pull_all()

    assert set(changed) == {"openclaw.json", "config/mcporter.json", "skills/github/"}
    written = json.loads((sync.local_dir / "openclaw.json").read_text())
    assert written["channels"]["matrix"]["accessToken"] == "local-token"
    assert written["channels"]["matrix"]["groupAllowFrom"] == ["@new:mx"]
    assert (sync.local_dir / "config" / "mcporter.json").read_text() == '{"mcpServers":{}}'
    assert commands == [
        (
            "mirror",
            "agentteams/agentteams-storage/agents/dag-team-dev/skills/github/",
            f"{sync.local_dir / 'skills' / 'github'}/",
            "--overwrite",
        )
    ]


def test_push_local_preserves_user_data_but_skips_manager_and_mirrored_state(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    pushed_destinations = []

    files = {
        "openclaw.json": "{}",
        "config/mcporter.json": "{}",
        ".copaw/workspaces/default/config/mcporter.json": "{}",
        ".copaw/workspaces/default/skills/github/SKILL.md": "runtime projection",
        "skills/github/SKILL.md": "standard skill",
        "shared/tasks/st-01/report.md": "team shared",
        "global-shared/reference.md": "global shared",
        ".copaw/workspaces/default/shared/tasks/st-01/report.md": "runtime shared",
        ".copaw/workspaces/default/global-shared/reference.md": "runtime global shared",
        "memory/note.txt": "remember this",
        "memory/shared/note.txt": "user data with shared path segment",
        "AGENTS.md": "worker prompt",
    }
    for rel, content in files.items():
        path = sync.local_dir / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_cat", lambda _key: None)

    def fake_mc(*args, **_kwargs):
        if args[0] == "cp":
            pushed_destinations.append(args[2])
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr("copaw_worker.sync._mc", fake_mc)

    pushed = push_local(sync, since=0)

    assert set(pushed) == {
        "AGENTS.md",
        "memory/note.txt",
        "memory/shared/note.txt",
        "skills/github/SKILL.md",
    }
    assert set(pushed_destinations) == {
        "agentteams/agentteams-storage/agents/dag-team-dev/AGENTS.md",
        "agentteams/agentteams-storage/agents/dag-team-dev/memory/note.txt",
        "agentteams/agentteams-storage/agents/dag-team-dev/memory/shared/note.txt",
        "agentteams/agentteams-storage/agents/dag-team-dev/skills/github/SKILL.md",
    }


class _RecordingHealth:
    def __init__(self):
        self.updates = []

    def update(self, component, healthiness, message="", details=None):
        self.updates.append((component, healthiness, message, details or {}))


async def _run_push_loop_once(sync, health, monkeypatch, exc):
    from copaw_worker import sync as sync_module

    def fake_push_local(*_args, **_kwargs):
        raise exc

    monkeypatch.setattr(sync_module, "push_local", fake_push_local)
    task = asyncio.create_task(sync_module.push_loop(sync, check_interval=0.01, health=health))
    await asyncio.sleep(0.03)
    task.cancel()
    try:
        await task
    except asyncio.CancelledError:
        pass


@pytest.mark.anyio
async def test_sync_loop_calls_change_callback(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    health = _RecordingHealth()
    calls = []

    def fake_pull_all():
        return ["openclaw.json"]

    async def on_pull(changed):
        calls.append(changed)

    monkeypatch.setattr(sync, "pull_all", fake_pull_all)
    task = asyncio.create_task(
        sync_loop(sync, interval=0.01, on_pull=on_pull, health=health)
    )
    await asyncio.sleep(0.03)
    task.cancel()
    try:
        await task
    except asyncio.CancelledError:
        pass

    assert calls
    assert calls[0] == ["openclaw.json"]
    assert (
        "sync",
        "healthy",
        "runtime config pull completed",
        {"operation": "sync_loop"},
    ) in health.updates


@pytest.mark.anyio
async def test_push_loop_reports_success_as_sync_and_bridge_healthy(tmp_path, monkeypatch):
    from copaw_worker import sync as sync_module

    sync = _sync(tmp_path)
    health = _RecordingHealth()

    monkeypatch.setattr(sync_module, "push_local", lambda *_args, **_kwargs: [])
    task = asyncio.create_task(sync_module.push_loop(sync, check_interval=0.01, health=health))
    await asyncio.sleep(0.03)
    task.cancel()
    try:
        await task
    except asyncio.CancelledError:
        pass

    assert ("bridge", "healthy", "runtime-to-standard bridge completed", {"operation": "bridge_runtime_to_standard"}) in health.updates
    assert ("sync", "healthy", "runtime file persistence completed", {"operation": "push_loop"}) in health.updates


@pytest.mark.anyio
async def test_push_loop_reports_storage_failure_as_sync_health(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    health = _RecordingHealth()

    await _run_push_loop_once(sync, health, monkeypatch, RuntimeError("oss unavailable"))

    assert health.updates
    component, healthiness, message, details = health.updates[0]
    assert component == "sync"
    assert healthiness == "unhealthy"
    assert message == "runtime file persistence failed: oss unavailable"
    assert details["operation"] == "push_loop"
    assert details["error_type"] == "RuntimeError"


@pytest.mark.anyio
async def test_push_loop_reports_bridge_failure_as_bridge_health(tmp_path, monkeypatch):
    sync = _sync(tmp_path)
    health = _RecordingHealth()

    await _run_push_loop_once(
        sync,
        health,
        monkeypatch,
        BridgeRuntimeError("runtime bridge failed"),
    )

    assert health.updates
    component, healthiness, message, details = health.updates[0]
    assert component == "bridge"
    assert healthiness == "unhealthy"
    assert message == "runtime-to-standard bridge failed: runtime bridge failed"
    assert details["operation"] == "bridge_runtime_to_standard"
    assert details["error_type"] == "BridgeRuntimeError"
