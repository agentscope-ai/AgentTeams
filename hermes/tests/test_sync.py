"""Tests for Hermes worker file sync behavior."""

from __future__ import annotations

import json
import subprocess

from hermes_worker import sync as sync_module
from hermes_worker.sync import FileSync, push_local


def test_mirror_all_falls_back_to_startup_files_when_prefix_missing(
    tmp_path, monkeypatch
) -> None:
    sync = FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="agentteams-storage",
        worker_name="dag-team-dev",
        local_dir=tmp_path / "worker",
    )
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
            raise subprocess.CalledProcessError(
                1,
                args,
                output="",
                stderr="mc.bin: <ERROR> Object does not exist.",
            )
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync_module, "_mc", fake_mc)

    sync.mirror_all()

    assert json.loads((sync.local_dir / "openclaw.json").read_text()) == {
        "team_id": "dag-team"
    }
    assert (
        "mirror",
        "agentteams/agentteams-storage/teams/dag-team/shared/",
        f"{sync.local_dir / 'shared'}/",
        "--overwrite",
    ) in commands


def test_push_local_persists_user_plugins_but_skips_image_overlay(
    tmp_path, monkeypatch
) -> None:
    """Runtime plugins are worker state; only the image overlay is excluded."""
    sync = FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="agentteams-storage",
        worker_name="worker-a",
        local_dir=tmp_path / "worker",
    )
    plugins = sync.local_dir / ".hermes" / "plugins"
    image_plugin = plugins / "platforms" / "matrix" / "adapter.py"
    custom_plugin = plugins / "custom-review" / "plugin.yaml"
    custom_adapter = plugins / "custom-review" / "adapter.py"
    for path, content in (
        (image_plugin, "image-owned"),
        (custom_plugin, "name: custom-review"),
        (custom_adapter, "def register(ctx): pass"),
    ):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")

    commands = []
    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)
    monkeypatch.setattr(sync, "_cat", lambda _key: None)

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(sync_module, "_mc", fake_mc)

    pushed = push_local(sync)

    assert ".hermes/plugins/custom-review/plugin.yaml" in pushed
    assert ".hermes/plugins/custom-review/adapter.py" in pushed
    assert ".hermes/plugins/platforms/matrix/adapter.py" not in pushed


def test_push_local_preserves_matrix_crypto_database_with_uncheckpointed_wal(tmp_path, monkeypatch):
    """A recreated worker needs the committed crypto state, including WAL rows."""
    import os
    import shutil
    import sqlite3

    sync = FileSync(endpoint="http://minio:9000", access_key="minio", secret_key="synthetic",
                    bucket="agentteams-storage", worker_name="worker-a", local_dir=tmp_path / "worker")
    db = sync.local_dir / ".hermes/platforms/matrix/store/crypto.db"
    db.parent.mkdir(parents=True)
    remote = tmp_path / "restored-crypto.db"
    connection = sqlite3.connect(db)
    try:
        connection.execute("PRAGMA journal_mode=WAL")
        connection.execute("PRAGMA wal_autocheckpoint=0")
        connection.execute("CREATE TABLE sessions (id TEXT PRIMARY KEY)")
        connection.commit()
        connection.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        connection.execute("INSERT INTO sessions VALUES ('persisted-session')")
        connection.commit()
        # The main DB is old; a committed WAL change alone must trigger backup.
        os.utime(db, (1, 1))
        monkeypatch.setattr(sync, "_ensure_alias", lambda: None)
        monkeypatch.setattr(sync, "_cat", lambda _key: None)
        copies = []
        def fake_mc(*args, **_kwargs):
            copies.append(args)
            if args[0] == "cp":
                shutil.copyfile(args[1], remote)
            return subprocess.CompletedProcess(args, 0, stdout="", stderr="")
        monkeypatch.setattr(sync_module, "_mc", fake_mc)
        pushed = push_local(sync, since=2)
        assert ".hermes/platforms/matrix/store/crypto.db" in pushed
        assert len(copies) == 1
        with sqlite3.connect(remote) as restored:
            assert restored.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
            assert restored.execute("SELECT id FROM sessions").fetchone() == ("persisted-session",)
    finally:
        connection.close()
