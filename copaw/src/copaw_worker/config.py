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
        minio_bucket: str = "agentteams-storage",
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
        self.install_dir = install_dir or _default_install_dir()
        self.console_port = console_port
        self.worker_port = worker_port or (console_port + 1)
        self.sync_interval = sync_interval


def _default_install_dir() -> Path:
    if configured := os.environ.get("COPAW_INSTALL_DIR"):
        return Path(configured)

    current = Path.home() / ".agentteams-worker"
    legacy = Path.home() / ".hiclaw-worker"
    if legacy.exists() and not current.exists():
        legacy.rename(current)
        legacy.symlink_to(current, target_is_directory=True)
    return current
