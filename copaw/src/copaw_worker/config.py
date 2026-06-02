"""WorkerConfig: parsed from CLI args / env vars."""
from __future__ import annotations

import os
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
        sync_interval: int = 60,
        install_dir: Path | None = None,
        console_port: int = 8088,
        worker_port: int | None = None,
        worker_cr_name: str | None = None,
    ) -> None:
        self.worker_name = worker_name
        self.worker_cr_name = worker_cr_name or worker_name
        self.minio_endpoint = minio_endpoint
        self.minio_access_key = minio_access_key
        self.minio_secret_key = minio_secret_key
        self.minio_bucket = minio_bucket
        self.minio_secure = minio_secure
        self.install_dir = install_dir or Path(
            os.environ.get("COPAW_INSTALL_DIR", Path.home() / ".hiclaw-worker")
        )
        self.console_port = console_port
        self.worker_port = worker_port or (console_port + 1)
        self.sync_interval = sync_interval
