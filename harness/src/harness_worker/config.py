"""WorkerConfig for harness-worker."""
from __future__ import annotations

from pathlib import Path


class WorkerConfig:
    def __init__(
        self,
        worker_name: str,
        minio_endpoint: str,
        minio_access_key: str,
        minio_secret_key: str,
        minio_bucket: str = "hiclaw-storage",
        minio_secure: bool = False,
        sync_interval: int = 300,
        install_dir: Path | None = None,
        harness_type: str = "claude",
    ) -> None:
        self.worker_name = worker_name
        self.minio_endpoint = minio_endpoint
        self.minio_access_key = minio_access_key
        self.minio_secret_key = minio_secret_key
        self.minio_bucket = minio_bucket
        self.minio_secure = minio_secure
        self.sync_interval = sync_interval
        self.install_dir = install_dir or Path("/root/hiclaw-fs/agents")
        self.harness_type = harness_type

    @property
    def workspace_dir(self) -> Path:
        return self.install_dir / self.worker_name

    @property
    def harness_home(self) -> Path:
        """Harness config home (~/.claude, ~/.gemini, etc.)."""
        return self.workspace_dir / ".harness"