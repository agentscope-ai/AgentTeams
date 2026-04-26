"""CoPaw-native filesync tool for HiClaw shared files."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

from agentscope.message import TextBlock
from agentscope.tool import ToolResponse

from copaw_worker.sync import FileSync


class FilesyncToolError(ValueError):
    """Expected user-facing error from the filesync tool."""


def _response(payload: dict[str, Any]) -> ToolResponse:
    return ToolResponse(
        content=[
            TextBlock(
                type="text",
                text=json.dumps(payload, ensure_ascii=False),
            ),
        ],
    )


def _ok(**payload: Any) -> ToolResponse:
    return _response({"ok": True, **payload})


def _error(message: str, **payload: Any) -> ToolResponse:
    return _response({"ok": False, "error": message, **payload})


def _copaw_working_dir() -> Path:
    configured = os.getenv("COPAW_WORKING_DIR")
    if configured:
        return Path(configured)

    cwd = Path.cwd()
    if cwd.name == "default" and cwd.parent.name == "workspaces":
        return cwd.parent.parent
    if cwd.name == ".copaw":
        return cwd
    return cwd / ".copaw"


def _create_sync() -> FileSync:
    worker_name = os.getenv("HICLAW_WORKER_NAME") or os.getenv("COPAW_WORKER_NAME")
    minio_endpoint = os.getenv("HICLAW_FS_ENDPOINT") or os.getenv("COPAW_MINIO_ENDPOINT")
    minio_access_key = os.getenv("HICLAW_FS_ACCESS_KEY") or os.getenv("COPAW_MINIO_ACCESS_KEY")
    minio_secret_key = os.getenv("HICLAW_FS_SECRET_KEY") or os.getenv("COPAW_MINIO_SECRET_KEY")
    minio_bucket = (
        os.getenv("HICLAW_FS_BUCKET")
        or os.getenv("COPAW_MINIO_BUCKET")
        or "hiclaw-storage"
    )

    missing = [
        name
        for name, value in (
            ("HICLAW_WORKER_NAME", worker_name),
            ("HICLAW_FS_ENDPOINT", minio_endpoint),
            ("HICLAW_FS_ACCESS_KEY", minio_access_key),
            ("HICLAW_FS_SECRET_KEY", minio_secret_key),
        )
        if not value
    ]
    if missing:
        raise FilesyncToolError(f"missing filesync environment: {', '.join(missing)}")

    working_dir = _copaw_working_dir()
    workspace_dir = working_dir / "workspaces" / "default"
    return FileSync(
        endpoint=str(minio_endpoint),
        access_key=str(minio_access_key),
        secret_key=str(minio_secret_key),
        bucket=str(minio_bucket),
        worker_name=str(worker_name),
        secure=str(minio_endpoint).startswith("https://"),
        local_dir=working_dir.parent,
        shared_dir=workspace_dir / "shared",
        global_shared_dir=workspace_dir / "global-shared",
    )


def _coerce_payload(payload: dict[str, Any] | str | None) -> dict[str, Any]:
    if payload is None:
        return {}
    if isinstance(payload, str):
        try:
            payload = json.loads(payload)
        except json.JSONDecodeError as exc:
            raise FilesyncToolError(f"payload must be a JSON object: {exc.msg}") from exc
    if not isinstance(payload, dict):
        raise FilesyncToolError("payload must be an object")
    return payload


def _normalize_exclude(exclude: list[str] | str | None) -> list[str]:
    if not exclude:
        return []
    if isinstance(exclude, str):
        try:
            exclude = json.loads(exclude)
        except json.JSONDecodeError as exc:
            raise FilesyncToolError(f"exclude must be a JSON array: {exc.msg}") from exc
    if not isinstance(exclude, list):
        raise FilesyncToolError("exclude must be a list")
    normalized: list[str] = []
    for item in exclude:
        text = str(item).strip()
        if text:
            normalized.append(text)
    return normalized


async def filesync(
    action: str,
    payload: dict[str, Any] | str | None = None,
    path: str | None = None,
    exclude: list[str] | str | None = None,
    dryRun: bool = False,
) -> ToolResponse:
    """Pull, push, stat, or list HiClaw shared files."""
    if isinstance(payload, str) and path is None and payload.strip().startswith(
        ("shared/", "global-shared/"),
    ):
        path = payload
        payload = None
    resolved_path = path
    try:
        payload_data = _coerce_payload(payload)
        if "path" in payload_data:
            resolved_path = payload_data.get("path")
        if "exclude" in payload_data:
            exclude = payload_data.get("exclude")

        if action not in {"pull", "push", "stat", "list"}:
            raise FilesyncToolError("action must be one of: pull, push, stat, list")
        if not isinstance(resolved_path, str) or not resolved_path.strip():
            raise FilesyncToolError("path is required")
        resolved_path = resolved_path.strip()

        sync = _create_sync()
        resolved = sync.resolve_shared_path(resolved_path)
        excludes = _normalize_exclude(exclude)
        payload: dict[str, Any] = {
            "action": action,
            "path": resolved_path,
            "localPath": str(resolved.local),
            "kind": resolved.kind,
        }
        if action == "push":
            payload["exclude"] = excludes

        if dryRun:
            return _ok(dryRun=True, **payload)

        if action == "pull":
            sync.pull_shared_path(resolved_path)
            return _ok(pulled=True, **payload)

        if action == "push":
            sync.push_shared_path(resolved_path, exclude=excludes)
            return _ok(pushed=True, **payload)

        if action == "stat":
            sync.stat_shared_path(resolved_path)
            return _ok(exists=True, **payload)

        _, entries = sync.list_shared_path(resolved_path)
        return _ok(entries=entries, **payload)
    except FilesyncToolError as exc:
        return _error(str(exc), action=action, path=resolved_path)
    except Exception as exc:  # pragma: no cover - defensive runtime boundary
        return _error(f"filesync failed: {exc}", action=action, path=resolved_path)
