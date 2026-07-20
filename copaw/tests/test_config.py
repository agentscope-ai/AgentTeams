from pathlib import Path

from copaw_worker.config import WorkerConfig


def test_default_install_dir_migrates_legacy_path(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.delenv("COPAW_INSTALL_DIR", raising=False)
    legacy = tmp_path / ".hiclaw-worker"
    legacy.mkdir()
    (legacy / "state.json").write_text("{}")

    config = WorkerConfig("alice", "http://minio", "key", "secret")

    assert config.install_dir == tmp_path / ".agentteams-worker"
    assert (config.install_dir / "state.json").exists()
    assert legacy.is_symlink()
    assert (legacy / "state.json").exists()
