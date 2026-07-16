"""Tests for CoPaw worker file sync behavior."""

import logging
import subprocess

from copaw_worker import sync
from copaw_worker.sync import FileSync


def test_ensure_alias_skips_static_alias_in_k8s_mode(monkeypatch, tmp_path):
    calls = []

    monkeypatch.setenv("AGENTTEAMS_RUNTIME", "k8s")
    monkeypatch.setattr(sync, "_mc", lambda *args, **_kwargs: calls.append(args))

    fs = FileSync(
        endpoint="minio:9000",
        access_key="tt",
        secret_key="secret",
        bucket="hiclaw",
        worker_name="tt",
        local_dir=tmp_path,
    )

    fs._ensure_alias()

    assert fs._alias_set is True
    assert calls == []


def test_filesync_fallback_uses_copaw_working_dir_parent(monkeypatch, tmp_path):
    working_dir = tmp_path / "alice" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    fs = FileSync(
        endpoint="minio:9000",
        access_key="tt",
        secret_key="secret",
        bucket="hiclaw",
        worker_name="alice",
    )

    assert fs.local_dir == tmp_path / "alice"


def test_cat_missing_object_is_debug_only(monkeypatch, tmp_path, caplog):
    fs = FileSync(
        endpoint="minio:9000",
        access_key="tt",
        secret_key="secret",
        bucket="hiclaw",
        worker_name="tt",
        local_dir=tmp_path,
    )
    monkeypatch.setattr(fs, "_ensure_alias", lambda: None)
    monkeypatch.setattr(
        sync,
        "_mc",
        lambda *_args, **_kwargs: subprocess.CompletedProcess(
            _args,
            1,
            stdout="",
            stderr="mc.bin: <ERROR> Object does not exist.",
        ),
    )
    caplog.set_level(logging.WARNING)

    assert fs._cat("agents/tt/config/mcporter.json") is None
    assert "Object does not exist" not in caplog.text


def test_cat_non_missing_failure_warns(monkeypatch, tmp_path, caplog):
    fs = FileSync(
        endpoint="minio:9000",
        access_key="tt",
        secret_key="secret",
        bucket="hiclaw",
        worker_name="tt",
        local_dir=tmp_path,
    )
    monkeypatch.setattr(fs, "_ensure_alias", lambda: None)
    monkeypatch.setattr(
        sync,
        "_mc",
        lambda *_args, **_kwargs: subprocess.CompletedProcess(
            _args,
            1,
            stdout="",
            stderr="AccessDenied: denied",
        ),
    )
    caplog.set_level(logging.WARNING)

    assert fs._cat("agents/tt/openclaw.json") is None
    assert "mc cat failed" in caplog.text
    assert "AccessDenied: denied" in caplog.text


def test_pull_all_skips_local_skill_absent_from_minio(monkeypatch, tmp_path, caplog):
    """A self-installed skill dir (present locally, absent in MinIO) must
    survive pull_all instead of being rmtree'd, and the skip should be
    logged exactly once even across repeated pull_all calls."""
    fs = FileSync(
        endpoint="minio:9000",
        access_key="tt",
        secret_key="secret",
        bucket="hiclaw",
        worker_name="tt",
        local_dir=tmp_path,
    )

    local_only_skill = fs.local_dir / "skills" / "self-installed"
    local_only_skill.mkdir(parents=True, exist_ok=True)
    (local_only_skill / "SKILL.md").write_text("hello", encoding="utf-8")
    marker = local_only_skill / "SKILL.md"

    monkeypatch.setattr(fs, "_cat", lambda _key: None)
    monkeypatch.setattr(fs, "_ls", lambda _prefix: [])
    monkeypatch.setattr(
        sync,
        "_mc",
        lambda *_args, **_kwargs: subprocess.CompletedProcess(_args, 0, stdout="", stderr=""),
    )
    caplog.set_level(logging.INFO)

    changed = fs.pull_all()
    assert not any("self-installed" in c and "removed" in c for c in changed)
    assert local_only_skill.is_dir()
    assert marker.read_text(encoding="utf-8") == "hello"
    assert caplog.text.count("self-installed") == 1

    # A second pull_all should not skip/rmtree, and must not re-log.
    caplog.clear()
    fs.pull_all()
    assert local_only_skill.is_dir()
    assert "self-installed" not in caplog.text
