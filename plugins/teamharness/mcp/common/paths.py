# workspace/project/task path computation (W1-a2: real module)
from __future__ import annotations

import os

from pathlib import Path

from typing import Any

from .config import _load_runtime_config, _section


def _storage_root_prefix() -> str:
    return os.getenv("AGENTTEAMS_STORAGE_PREFIX", "").strip().strip("/")


def _with_storage_root(prefix: str) -> str:
    clean = (prefix or "").strip().strip("/")
    root = _storage_root_prefix()
    if not root:
        return clean
    if not clean:
        return root
    if clean == root or clean.startswith(f"{root}/"):
        return clean
    return f"{root}/{clean}"


def _default_workspace_dir() -> str:
    """Derive workspace dir from environment (set by qwenpaw-worker / copaw-worker)."""
    shared_dir = os.getenv("TEAMHARNESS_SHARED_DIR", "").strip() or os.getenv("AGENTTEAMS_SHARED_DIR", "").strip()
    if shared_dir:
        return str(Path(shared_dir).parent)
    for env_key in ("QWENPAW_WORKING_DIR", "COPAW_WORKING_DIR"):
        working_dir = os.getenv(env_key, "").strip()
        if working_dir:
            return str(Path(working_dir) / "workspaces" / "default")
    return ""


def _default_shared_prefix() -> str:
    """Derive storage shared prefix from environment or runtime.yaml."""
    configured = os.getenv("AGENTTEAMS_SHARED_STORAGE_PREFIX", "").strip()
    if configured:
        return _with_storage_root(configured)
    storage = _section(_load_runtime_config(), "storage")
    prefix = str(storage.get("sharedPrefix") or "").strip()
    if prefix:
        return _with_storage_root(prefix)
    storage_prefix = _storage_root_prefix()
    if storage_prefix:
        return f"{storage_prefix}/shared"
    return "shared"


def _default_global_shared_prefix() -> str:
    """Derive global-shared storage prefix from environment or runtime.yaml."""
    storage = _section(_load_runtime_config(), "storage")
    prefix = str(storage.get("globalSharedPrefix") or "").strip()
    if prefix:
        return _with_storage_root(prefix)
    storage_prefix = _storage_root_prefix()
    if storage_prefix:
        return f"{storage_prefix}/shared"
    return "shared"


def _workspace_dir(arguments: dict[str, Any]) -> Path:
    value = str(arguments.get("workspaceDir") or "").strip()
    if not value:
        value = _default_workspace_dir()
    if not value:
        raise ValueError("workspaceDir is required")
    return Path(value).expanduser()


def _optional_workspace_dir(arguments: dict[str, Any]) -> Path | None:
    value = str(arguments.get("workspaceDir") or "").strip()
    if not value:
        value = _default_workspace_dir()
    return Path(value).expanduser() if value else None


def _project_dir(arguments: dict[str, Any], project_id: str) -> Path:
    return _workspace_dir(arguments) / "shared" / "projects" / project_id


def _task_dir(arguments: dict[str, Any], task_id: str) -> Path:
    return _workspace_dir(arguments) / "shared" / "tasks" / task_id


def _project_state_path(arguments: dict[str, Any], project_id: str) -> Path:
    return _project_dir(arguments, project_id) / "meta.json"


def _task_state_path(arguments: dict[str, Any], task_id: str) -> Path:
    return _task_dir(arguments, task_id) / "meta.json"


