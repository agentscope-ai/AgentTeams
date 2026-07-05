from pathlib import Path

from qwenpaw_worker.config import WorkerConfig


def _config(tmp_path: Path, **kwargs) -> WorkerConfig:
    return WorkerConfig(
        worker_name="worker-a",
        fs_endpoint="http://minio:9000",
        fs_access_key="key",
        fs_secret_key="secret",
        fs_bucket="agentteams-storage",
        install_dir=tmp_path / "agents",
        **kwargs,
    )


def test_worker_config_defaults_to_worker_layout(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.delenv("AGENTTEAMS_STORAGE_PREFIX", raising=False)
    monkeypatch.delenv("AGENTTEAMS_SHARED_STORAGE_PREFIX", raising=False)
    monkeypatch.delenv("AGENTTEAMS_MEMBER_RUNTIME_CONFIG", raising=False)
    monkeypatch.delenv("AGENTTEAMS_WORKER_ROLE", raising=False)
    monkeypatch.delenv("AGENTTEAMS_AGENT_ROLE", raising=False)
    monkeypatch.delenv("AGENT_WORKSPACE", raising=False)

    config = _config(tmp_path)

    assert config.worker_cr_name == "worker-a"
    assert config.agent_role == "worker"
    assert config.agent_name == "worker-a"
    assert config.worker_home == tmp_path / "agents" / "worker-a"
    assert config.qwenpaw_working_dir == config.worker_home / ".qwenpaw"
    assert config.default_workspace_dir == config.qwenpaw_working_dir / "workspaces" / "default"
    assert config.shared_dir == tmp_path / "shared"
    assert config.storage_prefix == "agents/worker-a"
    assert config.shared_prefix == "shared"
    assert config.runtime_config_path == tmp_path / "agents" / "worker-a" / "runtime" / "runtime.yaml"


def test_worker_config_reads_controller_role_env(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setenv("AGENTTEAMS_WORKER_ROLE", "team_leader")

    config = _config(tmp_path)

    assert config.agent_role == "team_leader"


def test_worker_config_ignores_agent_workspace_env(tmp_path: Path, monkeypatch) -> None:
    workspace = tmp_path / "custom-workspace"
    monkeypatch.setenv("AGENT_WORKSPACE", str(workspace))

    config = _config(tmp_path)

    assert config.default_workspace_dir == config.qwenpaw_working_dir / "workspaces" / "default"


def test_worker_config_uses_controller_storage_prefix_without_bucket_duplication(
    tmp_path: Path,
    monkeypatch,
) -> None:
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", "agentteams/agentteams-storage")
    monkeypatch.delenv("AGENTTEAMS_SHARED_STORAGE_PREFIX", raising=False)

    config = _config(tmp_path)

    assert config.storage_prefix == "agents/worker-a"
    assert config.shared_prefix == "shared"


def test_worker_config_supports_relative_controller_storage_prefix(
    tmp_path: Path,
    monkeypatch,
) -> None:
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", "teams/demo")
    monkeypatch.delenv("AGENTTEAMS_SHARED_STORAGE_PREFIX", raising=False)

    config = _config(tmp_path)

    assert config.storage_prefix == "teams/demo/agents/worker-a"
    assert config.shared_prefix == "teams/demo/shared"


def test_worker_config_allows_explicit_runtime_contract_paths(
    tmp_path: Path,
    monkeypatch,
) -> None:
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", "teams/demo")
    monkeypatch.setenv("AGENTTEAMS_SHARED_STORAGE_PREFIX", "teams/override/shared")
    runtime_config = tmp_path / "runtime.yaml"

    config = _config(
        tmp_path,
        storage_prefix="agents/custom-worker",
        shared_prefix=None,
        runtime_config_path=runtime_config,
    )

    assert config.storage_prefix == "agents/custom-worker"
    assert config.shared_prefix == "teams/override/shared"
    assert config.runtime_config_path == runtime_config
