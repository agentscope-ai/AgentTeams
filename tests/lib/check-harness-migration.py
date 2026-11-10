"""Exercise the migration actually installed in a shared QwenPaw Harness."""
import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
from unittest.mock import Mock

from qwenpaw_worker.worker import Worker


for failure in (None, "push_directories", "push_paths"):
    with tempfile.TemporaryDirectory(prefix="at-manager-migration-") as directory:
        home = Path(directory)
        legacy = home / ".copaw"
        secret = home / ".copaw.secret"
        target = home / ".qwenpaw"
        target_secret = home / ".qwenpaw.secret"
        os.environ["QWENPAW_SECRET_DIR"] = str(target_secret)
        files = {
            "sessions/history.json": b'{"messages":["migration-proof"]}',
            "plugins/custom/plugin.json": b'{"plugin":"custom"}',
            "models/local/model.json": b'{"model":"local"}',
            "memory/notes.md": b"keep-memory",
            "digest/state.json": b'{"digest":"keep"}',
            "workspaces/default/SOUL.md": b"keep-personality",
        }
        for relative, content in files.items():
            path = legacy / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)
        secret.mkdir()
        (secret / "master_key").write_bytes(b"synthetic-master-key")
        (secret / "providers.json").write_bytes(b'{"provider":"synthetic"}')
        target.mkdir()
        (target / "memory").mkdir()
        (target / "memory/notes.md").write_bytes(b"stale-target")
        config = {"agents": {"profiles": {"default": {
            "workspace_dir": str(legacy / "workspaces/default")}}}}
        (legacy / "config.json").write_text(json.dumps(config))
        worker = Worker.__new__(Worker)
        worker.config = SimpleNamespace(worker_name="manager", worker_home=home,
                                        qwenpaw_working_dir=target,
                                        default_workspace_dir=target / "workspaces/default")
        worker.sync = Mock()
        marker = target / ".copaw-migrated"
        if failure:
            getattr(worker.sync, failure).side_effect = RuntimeError("fixture storage failure")
            try:
                worker._migrate_legacy_state()
                raise AssertionError("storage failure must stop migration")
            except RuntimeError as error:
                assert str(error) == "fixture storage failure"
            assert legacy.exists() and secret.exists() and not marker.exists()
            getattr(worker.sync, failure).side_effect = None
        assert worker._migrate_legacy_state() is True
        for relative, content in files.items():
            assert (target / relative).read_bytes() == content, relative
        assert (target_secret / "master_key").read_bytes() == b"synthetic-master-key"
        assert (target_secret / "providers.json").read_bytes() == b'{"provider":"synthetic"}'
        migrated = json.loads((target / "config.json").read_text())
        assert migrated["agents"]["profiles"]["default"]["workspace_dir"] == str(target / "workspaces/default")
        assert marker.exists() and not legacy.exists() and not secret.exists()
        assert worker._migrate_legacy_state() is False
        calls = [call[0] for call in worker.sync.mock_calls]
        assert calls.index("push_directories") < calls.index("push_paths")
        print("PASS shared Manager migration: data, secrets, paths, persistence, retry and idempotency", failure)
