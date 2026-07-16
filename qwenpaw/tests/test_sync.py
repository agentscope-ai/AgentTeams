import os
import subprocess
from pathlib import Path

from qwenpaw_worker.sync import FileSync, push_local


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


def test_ensure_alias_skips_static_alias_in_k8s_mode(tmp_path: Path, monkeypatch) -> None:
    sync = FileSync(
        endpoint="https://oss.example.test",
        access_key="access-key",
        secret_key="secret-key",
        bucket="agentteams-storage",
        worker_name="worker-a",
        local_dir=tmp_path / "agents" / "worker-a",
        shared_dir=tmp_path / "shared",
    )
    commands = []
    monkeypatch.setenv("AGENTTEAMS_RUNTIME", "k8s")

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync, "_mc", fake_mc)

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
