"""WorkerConfig parsed from CLI args and AgentTeams env."""

from __future__ import annotations

import os
from pathlib import Path
from typing import Optional


def _clean_prefix(value: str) -> str:
    return value.strip().strip("/")


def _relative_storage_prefix(value: str, bucket: str) -> str:
    prefix = _clean_prefix(value)
    if not prefix:
        return ""
    parts = prefix.split("/")
    if parts[0] == bucket:
        return "/".join(parts[1:])
    if len(parts) >= 2 and parts[1] == bucket:
        return "/".join(parts[2:])
    return prefix


def _join_prefix(root: str, suffix: str) -> str:
    root = _clean_prefix(root)
    suffix = _clean_prefix(suffix)
    return f"{root}/{suffix}" if root else suffix


class WorkerConfig:
    def __init__(
        self,
        worker_name: str,
        fs_endpoint: str,
        fs_access_key: str,
        fs_secret_key: str,
        fs_bucket: str = "agentteams-storage",
        install_dir: Optional[Path] = None,
        console_port: int = 8088,
        worker_cr_name: Optional[str] = None,
        shared_dir: Optional[Path] = None,
        storage_prefix: Optional[str] = None,
        shared_prefix: Optional[str] = None,
        runtime_config_path: Optional[Path] = None,
        runtime_config_poll_interval: float = 5,
    ) -> None:
        self.worker_name = worker_name
        self.worker_cr_name = worker_cr_name or worker_name
        self.agent_role = os.environ.get("AGENTTEAMS_WORKER_ROLE") or os.environ.get("AGENTTEAMS_AGENT_ROLE") or "worker"
        self.agent_name = worker_name
        self.fs_endpoint = fs_endpoint
        self.fs_access_key = fs_access_key
        self.fs_secret_key = fs_secret_key
        self.fs_bucket = fs_bucket
        self.install_dir = install_dir or Path(
            os.environ.get("QWENPAW_INSTALL_DIR", "/root/agentteams-fs/agents"),
        )
        self.shared_dir_override = shared_dir
        storage_root = _relative_storage_prefix(
            os.environ.get("AGENTTEAMS_STORAGE_PREFIX", ""),
            self.fs_bucket,
        )
        self.storage_prefix = _relative_storage_prefix(
            storage_prefix,
            self.fs_bucket,
        ) if storage_prefix else _join_prefix(storage_root, f"agents/{self.worker_name}")
        configured_shared_prefix = os.environ.get("AGENTTEAMS_SHARED_STORAGE_PREFIX", "")
        self.shared_prefix = _relative_storage_prefix(
            shared_prefix,
            self.fs_bucket,
        ) if shared_prefix else (
            _relative_storage_prefix(configured_shared_prefix, self.fs_bucket)
            if configured_shared_prefix
            else _join_prefix(storage_root, "shared")
        )
        self.console_port = console_port
        self.runtime_config_path_override = runtime_config_path
        self.runtime_config_poll_interval = runtime_config_poll_interval

    @property
    def worker_home(self) -> Path:
        return self.install_dir / self.worker_name

    @property
    def qwenpaw_working_dir(self) -> Path:
        return self.worker_home / ".qwenpaw"

    @property
    def default_workspace_dir(self) -> Path:
        return self.qwenpaw_working_dir / "workspaces" / "default"

    @property
    def shared_dir(self) -> Path:
        if self.shared_dir_override is not None:
            return self.shared_dir_override
        return self.install_dir.parent / "shared"

    @property
    def runtime_config_path(self) -> Path:
        if self.runtime_config_path_override is not None:
            return self.runtime_config_path_override
        configured = os.environ.get("AGENTTEAMS_MEMBER_RUNTIME_CONFIG", "").strip()
        if configured:
            return Path(configured)
        return self.worker_home / "runtime" / "runtime.yaml"
