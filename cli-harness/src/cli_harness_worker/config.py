"""WorkerConfig: parsed from CLI args / env vars."""
from __future__ import annotations

from pathlib import Path

from cli_harness_worker.adapters import DEFAULT_RUNTIME, normalize_runtime


class WorkerConfig:
    def __init__(
        self,
        worker_name: str,
        minio_endpoint: str,
        minio_access_key: str,
        minio_secret_key: str,
        minio_bucket: str = "agentteams-storage",
        minio_secure: bool = False,
        sync_interval: int = 300,
        install_dir: Path | None = None,
        runtime: str = "",
    ) -> None:
        self.worker_name = worker_name
        self.minio_endpoint = minio_endpoint
        self.minio_access_key = minio_access_key
        self.minio_secret_key = minio_secret_key
        self.minio_bucket = minio_bucket
        self.minio_secure = minio_secure
        self.sync_interval = sync_interval
        # Same layout as every other runtime: workspace == HOME (== MinIO
        # mirror root). The entrypoint passes --install-dir explicitly, so
        # this default only matters for direct `cli-harness-worker`
        # invocations (CI / dev).
        self.install_dir = install_dir or Path("/root/agentteams-fs/agents")
        self.runtime = normalize_runtime(runtime)

    @property
    def workspace_dir(self) -> Path:
        """Per-worker workspace root (mirror of MinIO ``agents/<name>/``)."""
        return self.install_dir / self.worker_name

    @property
    def harness_home(self) -> Path:
        """Per-worker state dir (run markers, CLI session metadata)."""
        return self.workspace_dir / ".cli-harness"
