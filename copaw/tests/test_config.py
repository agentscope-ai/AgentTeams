from pathlib import Path

from copaw_worker.config import WorkerConfig


def test_default_install_dir_uses_agentteams_path(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.delenv("COPAW_INSTALL_DIR", raising=False)

    config = WorkerConfig("alice", "http://minio", "key", "secret")

    assert config.install_dir == tmp_path / ".agentteams-worker"
