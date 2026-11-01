import asyncio
import hashlib
import json
import os
import subprocess
import types
from pathlib import Path

import pytest

from qwenpaw_worker.sync import FileSync, push_local, push_loop


def _sync(tmp_path: Path) -> FileSync:
    return FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="agentteams-storage",
        worker_name="worker-a",
        local_dir=tmp_path / "agents" / "worker-a",
        shared_dir=tmp_path / "shared",
        remote_prefix="agents/worker-a",
        shared_prefix="shared",
    )


def test_mirror_all_restores_worker_and_shared_storage(tmp_path: Path, monkeypatch) -> None:
    sync = _sync(tmp_path)
    commands = []

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    sync.mirror_all()

    assert commands == [
        (
            "mirror",
            "agentteams/agentteams-storage/agents/worker-a/",
            f"{sync.local_dir}/",
            "--overwrite",
            "--exclude",
            "credentials/**",
        ),
        (
            "mirror",
            "agentteams/agentteams-storage/shared/",
            f"{sync.shared_dir}/",
            "--overwrite",
            "--exclude",
            "credentials/**",
        ),
    ]


def test_pull_runtime_config_downloads_controller_projection(tmp_path: Path, monkeypatch) -> None:
    sync = _sync(tmp_path)
    commands = []
    target = sync.local_dir / "runtime" / "runtime.yaml"

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)

    def fake_mc(*args, **kwargs):
        commands.append((args, kwargs))
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("member:\n  runtime: qwenpaw\n", encoding="utf-8")
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    assert sync.pull_runtime_config(target) is True

    assert target.read_text(encoding="utf-8").startswith("member:")
    assert commands == [
        (
            (
                "cp",
                "agentteams/agentteams-storage/agents/worker-a/runtime/runtime.yaml",
                str(target),
            ),
            {"check": False},
        )
    ]


@pytest.mark.parametrize("storage_provider", ["minio", None])
def test_ensure_alias_sets_static_alias_for_minio_in_k8s_mode(
    tmp_path: Path, monkeypatch, storage_provider: str | None
) -> None:
    sync = FileSync(
        endpoint="minio:9000",
        access_key="access-key",
        secret_key="secret-key",
        bucket="agentteams-storage",
        worker_name="worker-a",
        local_dir=tmp_path / "agents" / "worker-a",
        shared_dir=tmp_path / "shared",
    )
    commands = []
    monkeypatch.setenv("AGENTTEAMS_RUNTIME", "k8s")
    if storage_provider is None:
        monkeypatch.delenv("AGENTTEAMS_STORAGE_PROVIDER", raising=False)
    else:
        monkeypatch.setenv("AGENTTEAMS_STORAGE_PROVIDER", storage_provider)
    monkeypatch.delenv(f"MC_HOST_{sync.mc_alias}", raising=False)

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    sync.ensure_alias()

    assert sync._alias_set is True
    assert commands == [
        (
            "alias",
            "set",
            "agentteams",
            "http://minio:9000",
            "access-key",
            "secret-key",
        )
    ]


def test_ensure_alias_requires_mc_host_for_oss_in_k8s_mode(tmp_path: Path, monkeypatch) -> None:
    sync = FileSync(
        endpoint="oss.example.test",
        access_key="access-key",
        secret_key="secret-key",
        bucket="agentteams-storage",
        worker_name="worker-a",
        local_dir=tmp_path / "agents" / "worker-a",
        shared_dir=tmp_path / "shared",
    )
    monkeypatch.setenv("AGENTTEAMS_RUNTIME", "k8s")
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PROVIDER", "oss")
    monkeypatch.delenv(f"MC_HOST_{sync.mc_alias}", raising=False)

    with pytest.raises(RuntimeError, match=f"MC_HOST_{sync.mc_alias}"):
        sync.ensure_alias()


def test_ensure_alias_uses_existing_mc_host_in_k8s_mode(tmp_path: Path, monkeypatch) -> None:
    sync = FileSync(
        endpoint="oss.example.test",
        access_key="access-key",
        secret_key="secret-key",
        bucket="agentteams-storage",
        worker_name="worker-a",
        local_dir=tmp_path / "agents" / "worker-a",
        shared_dir=tmp_path / "shared",
    )
    commands = []
    monkeypatch.setenv("AGENTTEAMS_RUNTIME", "k8s")
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PROVIDER", "oss")
    monkeypatch.setenv(f"MC_HOST_{sync.mc_alias}", "https://ak:sk@oss.example.test")
    monkeypatch.setattr(
        sync,
        "_mc",
        lambda *args, **_kwargs: commands.append(args),
    )

    sync.ensure_alias()

    assert sync._alias_set is True
    assert commands == []


def test_storage_alias_derives_from_agentteams_storage_prefix(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", "agentteams/agentteams-storage")

    sync = _sync(tmp_path)

    assert sync.mc_alias == "agentteams"
    assert sync._object_path("agents/worker-a/file.txt") == "agentteams/agentteams-storage/agents/worker-a/file.txt"


def test_push_local_uploads_worker_files_but_skips_controller_owned_state(tmp_path: Path, monkeypatch) -> None:
    sync = _sync(tmp_path)
    uploads = []

    files = {
        "SOUL.md": "worker soul",
        ".qwenpaw/workspaces/default/AGENTS.md": "runtime prompt",
        ".qwenpaw/workspaces/default/TEAMS.md": "team prompt",
        ".qwenpaw/workspaces/default/config/mcporter.json": '{"mcpServers":{}}',
        ".qwenpaw/plugins/teamharness/plugin.py": "installed plugin",
        ".qwenpaw/skill_pool/teamharness-communication/SKILL.md": "skill",
        ".qwenpaw/workspaces/default/skills/custom/SKILL.md": "workspace skill",
        ".qwenpaw/agent-packages/current/AGENTS.md": "agent package",
        ".qwenpaw/qwenpaw.log": "log",
        ".qwenpaw/logs/qwenpaw-worker.log": "worker log",
        ".qwenpaw/workspaces/default/tool_result/result.json": "{}",
        ".qwenpaw/workspaces/default/file_store/a.bin": "file",
        "runtime/runtime.yaml": "controller owned runtime config",
        "credentials/token": "secret",
        "shared/tasks/t-1/result.md": "team shared",
        "global-shared/reference.md": "global shared",
    }
    for rel, content in files.items():
        path = sync.local_dir / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_cat_bytes", lambda _key: None)

    def fake_mc(*args, **_kwargs):
        if args[0] == "cp":
            uploads.append(args[2])
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    pushed = push_local(sync, since=0)

    assert set(pushed) == {
        "SOUL.md",
        ".qwenpaw/workspaces/default/AGENTS.md",
        ".qwenpaw/workspaces/default/TEAMS.md",
        ".qwenpaw/workspaces/default/config/mcporter.json",
        ".qwenpaw/plugins/teamharness/plugin.py",
        ".qwenpaw/skill_pool/teamharness-communication/SKILL.md",
        ".qwenpaw/workspaces/default/skills/custom/SKILL.md",
        ".qwenpaw/agent-packages/current/AGENTS.md",
    }
    assert "runtime/runtime.yaml" not in pushed
    assert not any("credentials" in item for item in pushed)
    assert not any("logs/" in item for item in pushed)
    assert not any("shared/" in item for item in pushed)
    assert set(uploads) == {
        "agentteams/agentteams-storage/agents/worker-a/SOUL.md",
        "agentteams/agentteams-storage/agents/worker-a/.qwenpaw/workspaces/default/AGENTS.md",
        "agentteams/agentteams-storage/agents/worker-a/.qwenpaw/workspaces/default/TEAMS.md",
        "agentteams/agentteams-storage/agents/worker-a/.qwenpaw/workspaces/default/config/mcporter.json",
        "agentteams/agentteams-storage/agents/worker-a/.qwenpaw/plugins/teamharness/plugin.py",
        "agentteams/agentteams-storage/agents/worker-a/.qwenpaw/skill_pool/teamharness-communication/SKILL.md",
        "agentteams/agentteams-storage/agents/worker-a/.qwenpaw/workspaces/default/skills/custom/SKILL.md",
        "agentteams/agentteams-storage/agents/worker-a/.qwenpaw/agent-packages/current/AGENTS.md",
    }


def test_push_paths_persists_selected_migration_files_in_order(tmp_path: Path, monkeypatch) -> None:
    sync = _sync(tmp_path)
    session = sync.local_dir / ".qwenpaw" / "workspaces" / "default" / "chats.json"
    marker = sync.local_dir / ".qwenpaw" / ".copaw-migrated"
    session.parent.mkdir(parents=True)
    session.write_text("legacy-session", encoding="utf-8")
    marker.write_text("copaw-to-qwenpaw\n", encoding="utf-8")
    uploads = []

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_cat_bytes", lambda _key: None)

    def fake_mc(*args, **_kwargs):
        uploads.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    assert sync.push_paths([session, marker]) == [
        ".qwenpaw/workspaces/default/chats.json",
        ".qwenpaw/.copaw-migrated",
    ]
    assert [args[2] for args in uploads] == [
        "agentteams/agentteams-storage/agents/worker-a/.qwenpaw/workspaces/default/chats.json",
        "agentteams/agentteams-storage/agents/worker-a/.qwenpaw/.copaw-migrated",
    ]


def test_push_directories_mirrors_migration_roots(tmp_path: Path, monkeypatch) -> None:
    sync = _sync(tmp_path)
    working_dir = sync.local_dir / ".qwenpaw"
    secret_dir = sync.local_dir / ".qwenpaw.secret"
    working_dir.mkdir(parents=True)
    secret_dir.mkdir()
    commands = []

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    assert sync.push_directories([working_dir, secret_dir]) == [".qwenpaw", ".qwenpaw.secret"]
    assert commands == [
        (
            "mirror",
            f"{working_dir}/",
            "agentteams/agentteams-storage/agents/worker-a/.qwenpaw/",
            "--overwrite",
        ),
        (
            "mirror",
            f"{secret_dir}/",
            "agentteams/agentteams-storage/agents/worker-a/.qwenpaw.secret/",
            "--overwrite",
        ),
    ]


def test_push_local_does_not_remove_remote_files_missing_from_local_state(tmp_path: Path, monkeypatch) -> None:
    sync = _sync(tmp_path)
    commands = []

    current = sync.local_dir / ".qwenpaw" / "workspaces" / "default" / "AGENTS.md"
    current.parent.mkdir(parents=True, exist_ok=True)
    current.write_text("runtime prompt", encoding="utf-8")

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_cat_bytes", lambda key: b"runtime prompt" if key.endswith("/AGENTS.md") else None)

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    assert push_local(sync, since=0) == []
    assert not any(args[0] == "rm" for args in commands)
    assert not any(args[0] == "find" for args in commands)


def test_push_local_skips_older_and_unchanged_files(tmp_path: Path, monkeypatch) -> None:
    sync = _sync(tmp_path)
    uploads = []

    old_file = sync.local_dir / "old.txt"
    old_file.parent.mkdir(parents=True, exist_ok=True)
    old_file.write_text("old", encoding="utf-8")
    os.utime(old_file, (1, 1))

    same_file = sync.local_dir / "same.txt"
    same_file.write_text("same", encoding="utf-8")

    changed_file = sync.local_dir / "changed.txt"
    changed_file.write_text("changed", encoding="utf-8")

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_cat_bytes", lambda key: b"same" if key.endswith("/same.txt") else None)

    def fake_mc(*args, **_kwargs):
        if args[0] == "cp":
            uploads.append(args[2])
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    assert push_local(sync, since=100) == ["changed.txt"]
    assert uploads == ["agentteams/agentteams-storage/agents/worker-a/changed.txt"]


def test_push_local_compares_small_binary_files_as_bytes(tmp_path: Path, monkeypatch) -> None:
    sync = _sync(tmp_path)
    uploads = []

    same_file = sync.local_dir / "same.bin"
    same_file.parent.mkdir(parents=True, exist_ok=True)
    same_file.write_bytes(b"\xff\x00same")

    changed_file = sync.local_dir / "changed.bin"
    changed_file.write_bytes(b"\xff\x00changed")

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)

    def fake_cat_bytes(key):
        if key.endswith("/same.bin"):
            return b"\xff\x00same"
        if key.endswith("/changed.bin"):
            return b"\xff\x00old"
        return None

    monkeypatch.setattr(sync, "_cat_bytes", fake_cat_bytes)

    def fake_mc(*args, **_kwargs):
        if args[0] == "cp":
            uploads.append(args[2])
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    assert push_local(sync, since=0) == ["changed.bin"]
    assert uploads == ["agentteams/agentteams-storage/agents/worker-a/changed.bin"]


def test_push_local_uploads_large_files_without_remote_content_compare(tmp_path: Path, monkeypatch) -> None:
    sync = _sync(tmp_path)
    uploads = []

    large_file = sync.local_dir / "large.zip"
    large_file.parent.mkdir(parents=True, exist_ok=True)
    large_file.write_bytes(b"0" * (21 * 1024 * 1024))

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)

    def fail_cat_bytes(_key):
        raise AssertionError("large files should not download remote content for comparison")

    monkeypatch.setattr(sync, "_cat_bytes", fail_cat_bytes)

    def fake_mc(*args, **_kwargs):
        if args[0] == "cp":
            uploads.append(args[2])
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    assert push_local(sync, since=0) == ["large.zip"]
    assert uploads == ["agentteams/agentteams-storage/agents/worker-a/large.zip"]


@pytest.mark.anyio
async def test_push_loop_starts_from_current_time_instead_of_full_scan(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    sync = _sync(tmp_path)
    since_values: list[float] = []
    sleeps = 0

    async def fake_sleep(_seconds: float) -> None:
        nonlocal sleeps
        sleeps += 1
        if sleeps > 1:
            raise asyncio.CancelledError

    async def fake_to_thread(_function, _sync, since):
        since_values.append(since)
        return []

    monkeypatch.setattr("qwenpaw_worker.sync.time.time", lambda: 123.0)
    monkeypatch.setattr("qwenpaw_worker.sync.asyncio.sleep", fake_sleep)
    monkeypatch.setattr("qwenpaw_worker.sync.asyncio.to_thread", fake_to_thread)

    await push_loop(sync, check_interval=0)

    assert since_values == [123.0]


# ---------------------------------------------------------------------------
# #1391: push manifest durability (pull protection / push conflict / status)
# ---------------------------------------------------------------------------


def _write_manifest(sync, worker_files: dict) -> None:
    sync.sync_state_path.write_text(
        json.dumps({"version": 1, "roots": {"worker": worker_files}}),
        encoding="utf-8",
    )


def test_mirror_all_preserves_unpushed_local_write(tmp_path: Path, monkeypatch) -> None:
    """Write -> restart: the startup mirror must not clobber local work."""
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("local-new", encoding="utf-8")
    _write_manifest(
        sync,
        {"state.txt": {"sha256": hashlib.sha256(b"old-pushed").hexdigest(),
                       "pushed_at": 1.0}},
    )
    monkeypatch.setattr(sync, "ensure_alias", lambda: None)

    def fake_mc(*args, **_kwargs):
        if args and args[0] == "mirror" and "agents/worker-a/" in args[1]:
            (Path(args[2]) / "state.txt").write_text("remote-old", encoding="utf-8")
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    sync.mirror_all()

    assert state.read_text(encoding="utf-8") == "local-new"


def test_first_run_mirror_still_pulls(tmp_path: Path, monkeypatch) -> None:
    """With no prior push manifest nothing is protected (fresh install)."""
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("local-placeholder", encoding="utf-8")
    monkeypatch.setattr(sync, "ensure_alias", lambda: None)

    def fake_mc(*args, **_kwargs):
        if args and args[0] == "mirror" and "agents/worker-a/" in args[1]:
            (Path(args[2]) / "state.txt").write_text("remote-old", encoding="utf-8")
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    sync.mirror_all()

    assert state.read_text(encoding="utf-8") == "remote-old"


def test_push_directories_excludes_conflicting_files(tmp_path: Path, monkeypatch) -> None:
    """A newer remote copy is not overwritten by an older local change."""
    sync = _sync(tmp_path)
    work = sync.local_dir / ".qwenpaw"
    work.mkdir(parents=True)
    conflict = work / "conflict.txt"
    conflict.write_text("local-change", encoding="utf-8")
    _write_manifest(
        sync,
        {".qwenpaw/conflict.txt": {
            "sha256": hashlib.sha256(b"base").hexdigest(), "pushed_at": 1.0}},
    )
    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_cat_bytes", lambda key: b"remote-change")
    commands = []

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    assert sync.push_directories([work]) == [".qwenpaw"]
    assert "--exclude" in commands[0]
    assert "conflict.txt" in commands[0]


def test_push_paths_records_manifest(tmp_path: Path, monkeypatch) -> None:
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    path = sync.local_dir / "state.txt"
    path.write_text("v1", encoding="utf-8")
    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(
        sync, "_mc",
        lambda *a, **k: subprocess.CompletedProcess(a, 0, stdout="", stderr=""),
    )
    monkeypatch.setattr(sync, "_cat_bytes", lambda key: None)

    assert sync.push_paths([path]) == ["state.txt"]

    state = json.loads(sync.sync_state_path.read_text(encoding="utf-8"))
    assert state["roots"]["worker"]["state.txt"]["sha256"] == (
        hashlib.sha256(b"v1").hexdigest()
    )


def test_sync_status_reports_unpushed(tmp_path: Path) -> None:
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    (sync.local_dir / "state.txt").write_text("v2", encoding="utf-8")
    _write_manifest(
        sync,
        {"state.txt": {"sha256": hashlib.sha256(b"v1").hexdigest(),
                       "pushed_at": 1.0}},
    )
    status = sync.sync_status()
    assert status["unpushed"] == [
        {"root": "worker", "path": "state.txt", "status": "modified"}
    ]
    assert status["tracked"]["worker"] == 1


def test_mirror_all_restores_after_network_error(tmp_path: Path, monkeypatch) -> None:
    """A partial mirror failure must not leave the older remote copy behind."""
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("local-new", encoding="utf-8")
    _write_manifest(
        sync,
        {"state.txt": {"sha256": hashlib.sha256(b"old-pushed").hexdigest(),
                       "pushed_at": 1.0}},
    )
    monkeypatch.setattr(sync, "ensure_alias", lambda: None)

    def fake_mc(*args, **_kwargs):
        if args and args[0] == "mirror" and "agents/worker-a/" in args[1]:
            (Path(args[2]) / "state.txt").write_text("remote-old", encoding="utf-8")
            raise subprocess.CalledProcessError(
                1, args, stderr="net error: connection refused"
            )
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    with pytest.raises(RuntimeError, match="mirror storage failed"):
        sync.mirror_all()

    assert state.read_text(encoding="utf-8") == "local-new"


def test_mirror_all_restores_after_missing_object_error(tmp_path: Path, monkeypatch) -> None:
    """The empty-prefix branch returns normally and must restore too."""
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("local-new", encoding="utf-8")
    _write_manifest(
        sync,
        {"state.txt": {"sha256": hashlib.sha256(b"old-pushed").hexdigest(),
                       "pushed_at": 1.0}},
    )
    monkeypatch.setattr(sync, "ensure_alias", lambda: None)

    def fake_mc(*args, **_kwargs):
        if args and args[0] == "mirror" and "agents/worker-a/" in args[1]:
            (Path(args[2]) / "state.txt").write_text("remote-old", encoding="utf-8")
            raise subprocess.CalledProcessError(
                1, args, stderr="Object does not exist"
            )
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    sync.mirror_all()  # must return normally, not raise

    assert state.read_text(encoding="utf-8") == "local-new"


def test_push_manifest_survives_restart(tmp_path: Path, monkeypatch) -> None:
    """A new FileSync instance (process restart) still sees pushed state."""
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("v1", encoding="utf-8")
    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(
        sync, "_mc",
        lambda *a, **k: subprocess.CompletedProcess(a, 0, stdout="", stderr=""),
    )
    monkeypatch.setattr(sync, "_cat_bytes", lambda key: None)

    assert sync.push_paths([state]) == ["state.txt"]

    # Simulate a process restart: a brand-new instance over the same dirs.
    restarted = _sync(tmp_path)
    status = restarted.sync_status()
    assert status["tracked"]["worker"] == 1
    assert status["unpushed"] == []

    # The restarted mirror must not lose the pushed file.
    monkeypatch.setattr(restarted, "ensure_alias", lambda: None)

    def fake_mc(*args, **_kwargs):
        if args and args[0] == "mirror" and "agents/worker-a/" in args[1]:
            (Path(args[2]) / "state.txt").write_text("v1", encoding="utf-8")
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(restarted, "_mc", fake_mc)

    restarted.mirror_all()

    assert state.read_text(encoding="utf-8") == "v1"


class _FakeRemote:
    """In-memory object store behind `mc`, enough for push/pull lifecycles."""

    def __init__(self) -> None:
        self.objects: dict[str, bytes] = {}

    def mc(self, *args, **_kwargs):
        if args and args[0] == "cp":
            self.objects[args[2]] = Path(args[1]).read_bytes()
        elif args and args[0] == "mirror":
            prefix, local = args[1], Path(args[2])
            for key, data in list(self.objects.items()):
                if not key.startswith(prefix):
                    continue
                target = local / key[len(prefix):]
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(data)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    def cat(self, key: str):
        return self.objects.get(key)


def test_background_push_records_manifest_and_protects_later_edit(
    tmp_path: Path, monkeypatch
) -> None:
    """push_local -> restart -> mirror, with no manifest seeded up front."""
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("pushed-old", encoding="utf-8")
    remote = _FakeRemote()

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_mc", remote.mc)
    monkeypatch.setattr(sync, "_cat_bytes", remote.cat)

    assert push_local(sync, since=0) == ["state.txt"]
    manifest = json.loads(sync.sync_state_path.read_text(encoding="utf-8"))
    assert manifest["roots"]["worker"]["state.txt"]["sha256"] == (
        hashlib.sha256(b"pushed-old").hexdigest()
    )

    # The agent edits after the background push, then the worker restarts.
    state.write_text("unpushed-new-content", encoding="utf-8")
    restarted = _sync(tmp_path)
    monkeypatch.setattr(restarted, "ensure_alias", lambda: None)
    monkeypatch.setattr(restarted, "_mc", remote.mc)
    monkeypatch.setattr(restarted, "_cat_bytes", remote.cat)

    restarted.mirror_all()

    assert state.read_text(encoding="utf-8") == "unpushed-new-content"


def test_background_push_noop_records_baseline_for_later_edit(
    tmp_path: Path, monkeypatch
) -> None:
    """Remote bytes already match: no upload, but the baseline must be recorded."""
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("same", encoding="utf-8")
    remote = _FakeRemote()
    remote.objects["agents/worker-a/state.txt"] = b"same"

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_mc", remote.mc)
    monkeypatch.setattr(sync, "_cat_bytes", remote.cat)

    assert push_local(sync, since=0) == []
    manifest = json.loads(sync.sync_state_path.read_text(encoding="utf-8"))
    assert manifest["roots"]["worker"]["state.txt"]["sha256"] == (
        hashlib.sha256(b"same").hexdigest()
    )

    state.write_text("unpushed-edit", encoding="utf-8")
    restarted = _sync(tmp_path)
    monkeypatch.setattr(restarted, "ensure_alias", lambda: None)
    monkeypatch.setattr(restarted, "_mc", remote.mc)
    monkeypatch.setattr(restarted, "_cat_bytes", remote.cat)

    restarted.mirror_all()

    assert state.read_text(encoding="utf-8") == "unpushed-edit"


def test_push_paths_noop_records_baseline_for_later_edit(
    tmp_path: Path, monkeypatch
) -> None:
    """An explicit helper whose remote copy already matches still records."""
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("same", encoding="utf-8")
    remote = _FakeRemote()
    remote.objects["agents/worker-a/state.txt"] = b"same"

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_mc", remote.mc)
    monkeypatch.setattr(sync, "_cat_bytes", remote.cat)

    assert sync.push_paths([state]) == []
    manifest = json.loads(sync.sync_state_path.read_text(encoding="utf-8"))
    assert manifest["roots"]["worker"]["state.txt"]["sha256"] == (
        hashlib.sha256(b"same").hexdigest()
    )

    state.write_text("unpushed-edit", encoding="utf-8")
    restarted = _sync(tmp_path)
    monkeypatch.setattr(restarted, "ensure_alias", lambda: None)
    monkeypatch.setattr(restarted, "_mc", remote.mc)
    monkeypatch.setattr(restarted, "_cat_bytes", remote.cat)

    restarted.mirror_all()

    assert state.read_text(encoding="utf-8") == "unpushed-edit"


def test_upload_then_local_write_keeps_new_edit_dirty(tmp_path: Path, monkeypatch) -> None:
    """A local write landing while `mc cp` runs must stay dirty.

    The manifest baseline has to be the content that actually reached the
    remote; a re-read at record time would record the unuploaded edit as
    pushed and the next mirror would replace it with the remote copy.
    """
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("uploaded-snapshot", encoding="utf-8")
    remote = _FakeRemote()

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_cat_bytes", remote.cat)

    def fake_mc(*args, **_kwargs):
        if args and args[0] == "cp":
            remote.objects[args[2]] = Path(args[1]).read_bytes()
            # A concurrent writer lands after the bytes left for the upload
            # but before the manifest entry is recorded.
            Path(args[1]).write_text(
                "new-local-edit-after-upload", encoding="utf-8"
            )
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    assert sync.push_paths([state]) == ["state.txt"]
    manifest = json.loads(sync.sync_state_path.read_text(encoding="utf-8"))
    assert manifest["roots"]["worker"]["state.txt"]["sha256"] == (
        hashlib.sha256(b"uploaded-snapshot").hexdigest()
    )
    assert sync.sync_status()["unpushed"] == [
        {"root": "worker", "path": "state.txt", "status": "modified"}
    ]

    # Process restart: the remote still only holds the uploaded content.
    restarted = _sync(tmp_path)
    monkeypatch.setattr(restarted, "ensure_alias", lambda: None)
    monkeypatch.setattr(restarted, "_mc", remote.mc)
    monkeypatch.setattr(restarted, "_cat_bytes", remote.cat)

    restarted.mirror_all()

    assert state.read_text(encoding="utf-8") == "new-local-edit-after-upload"


def test_remote_equal_then_local_write_keeps_new_edit_dirty(tmp_path: Path, monkeypatch) -> None:
    """The remote-equal no-op must record the compared bytes, not a re-read.

    A write landing between the remote comparison and the manifest record
    must stay dirty, or the next mirror overwrites it with the (identical)
    remote copy.
    """
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("uploaded-snapshot", encoding="utf-8")
    remote = _FakeRemote()
    remote.objects["agents/worker-a/state.txt"] = b"uploaded-snapshot"

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_mc", remote.mc)

    def fake_cat_bytes(key):
        data = remote.cat(key)
        if data is not None:
            # A concurrent writer lands after the remote comparison but
            # before the manifest entry is recorded.
            state.write_text("new-local-edit-after-upload", encoding="utf-8")
        return data

    monkeypatch.setattr(sync, "_cat_bytes", fake_cat_bytes)

    assert sync.push_paths([state]) == []
    manifest = json.loads(sync.sync_state_path.read_text(encoding="utf-8"))
    assert manifest["roots"]["worker"]["state.txt"]["sha256"] == (
        hashlib.sha256(b"uploaded-snapshot").hexdigest()
    )
    assert sync.sync_status()["unpushed"] == [
        {"root": "worker", "path": "state.txt", "status": "modified"}
    ]

    restarted = _sync(tmp_path)
    monkeypatch.setattr(restarted, "ensure_alias", lambda: None)
    monkeypatch.setattr(restarted, "_mc", remote.mc)
    monkeypatch.setattr(restarted, "_cat_bytes", remote.cat)

    restarted.mirror_all()

    assert state.read_text(encoding="utf-8") == "new-local-edit-after-upload"


def test_push_directories_records_pre_mirror_snapshot(tmp_path: Path, monkeypatch) -> None:
    """A write landing during `mc mirror` must stay dirty.

    Directory pushes record per-file hashes captured before the mirror
    runs; recording after the mirror would hash the unuploaded edit.
    """
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    work = sync.local_dir / "data"
    note = work / "notes.txt"
    note.parent.mkdir(parents=True)
    note.write_text("before-mirror", encoding="utf-8")
    remote = _FakeRemote()

    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_cat_bytes", remote.cat)

    def fake_mc(*args, **_kwargs):
        if args and args[0] == "mirror":
            # A concurrent writer lands while the mirror runs.
            note.write_text("after-mirror", encoding="utf-8")
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    assert sync.push_directories([work]) == ["data"]
    manifest = json.loads(sync.sync_state_path.read_text(encoding="utf-8"))
    assert manifest["roots"]["worker"]["data/notes.txt"]["sha256"] == (
        hashlib.sha256(b"before-mirror").hexdigest()
    )
    assert sync.sync_status()["unpushed"] == [
        {"root": "worker", "path": "data/notes.txt", "status": "modified"}
    ]


def test_mirror_all_excludes_unpushed_file_from_mirror(
    tmp_path: Path, monkeypatch
) -> None:
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("local-new", encoding="utf-8")
    _write_manifest(
        sync,
        {"state.txt": {"sha256": hashlib.sha256(b"old-pushed").hexdigest(),
                       "pushed_at": 1.0}},
    )
    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    commands = []

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        if args and args[0] == "mirror" and "agents/worker-a/" in args[1]:
            # mc honours --exclude; the fake reproduces that behaviour so a
            # missing exclude would expose the overwrite path.
            if "state.txt" not in args:
                state.write_text("remote-old", encoding="utf-8")
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    sync.mirror_all()

    assert state.read_text(encoding="utf-8") == "local-new"
    excluded_pairs = {
        (command[index], command[index + 1])
        for command in commands
        for index in range(len(command) - 1)
    }
    assert ("--exclude", "state.txt") in excluded_pairs


def test_mirror_all_aborts_before_destructive_mirror_without_restore_space(
    tmp_path: Path, monkeypatch
) -> None:
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    original = b"x" * (12 * 1024 * 1024)
    state.write_bytes(original)
    _write_manifest(
        sync,
        {"state.txt": {"sha256": hashlib.sha256(b"old-pushed").hexdigest(),
                       "pushed_at": 1.0}},
    )
    monkeypatch.setattr(sync, "ensure_alias", lambda: None)
    monkeypatch.setattr(
        "qwenpaw_worker.sync.shutil.disk_usage",
        lambda _path: types.SimpleNamespace(free=1024),
    )
    commands = []

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    with pytest.raises(RuntimeError, match="refusing destructive mirror"):
        sync.mirror_all()

    assert state.read_bytes() == original
    assert commands == []


def test_partial_mirror_failure_keeps_unpushed_file(
    tmp_path: Path, monkeypatch
) -> None:
    sync = _sync(tmp_path)
    sync.local_dir.mkdir(parents=True)
    state = sync.local_dir / "state.txt"
    state.write_text("local-new", encoding="utf-8")
    _write_manifest(
        sync,
        {"state.txt": {"sha256": hashlib.sha256(b"old-pushed").hexdigest(),
                       "pushed_at": 1.0}},
    )
    monkeypatch.setattr(sync, "ensure_alias", lambda: None)

    def fake_mc(*args, **_kwargs):
        if args and args[0] == "mirror" and "agents/worker-a/" in args[1]:
            assert "state.txt" in args
            raise subprocess.CalledProcessError(
                1, args, output="", stderr="No space left on device"
            )
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

    with pytest.raises(RuntimeError, match="mirror storage failed"):
        sync.mirror_all()

    assert state.read_text(encoding="utf-8") == "local-new"
