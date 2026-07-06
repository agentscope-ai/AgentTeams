"""Shared helpers for CoPaw-native HiClaw tool modules.

Extracted from the verbatim-duplicated definitions in taskflow.py,
projectflow.py, message.py, and filesync.py. Behavior-preserving: the
functions below are byte-identical to the originals they replace.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

from agentscope.message import TextBlock
from agentscope.tool import ToolResponse

from copaw_worker.task import FileSystemTaskStore, TaskflowError


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


def _workspace_dir() -> Path:
    configured = os.getenv("COPAW_WORKING_DIR")
    if configured:
        return Path(configured) / "workspaces" / "default"

    cwd = Path.cwd()
    if cwd.name == "default" and cwd.parent.name == "workspaces":
        return cwd
    if cwd.name == ".copaw":
        return cwd / "workspaces" / "default"
    return cwd


def _store() -> FileSystemTaskStore:
    return FileSystemTaskStore(_workspace_dir())


def _coerce_payload(payload: dict[str, Any] | str | None) -> dict[str, Any]:
    if isinstance(payload, str):
        try:
            payload = json.loads(payload)
        except json.JSONDecodeError as exc:
            raise TaskflowError(f"payload must be a JSON object: {exc.msg}") from exc
    if payload is None:
        return {}
    if not isinstance(payload, dict):
        raise TaskflowError("payload must be an object")
    return payload


def _required_str(payload: dict[str, Any], key: str) -> str:
    value = payload.get(key)
    if not isinstance(value, str) or not value.strip():
        raise TaskflowError(f"payload.{key} is required")
    return value.strip()


def _optional_str(payload: dict[str, Any], key: str) -> str | None:
    value = payload.get(key)
    if value is None:
        return None
    if not isinstance(value, str):
        raise TaskflowError(f"payload.{key} must be a string")
    return value
