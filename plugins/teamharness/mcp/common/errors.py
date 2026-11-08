# failure-response constructors shared by project/task flows (W1-a2: real module)
from __future__ import annotations

from typing import Any

def _sync_failure_result(result: dict[str, Any], operation: str) -> dict[str, Any]:
    result["ok"] = False
    result["synced"] = False
    result["retryable"] = True
    result["statePersisted"] = True
    result["error"] = f"{operation} state persisted locally but shared-storage sync failed; retry to complete"
    result.pop("notificationNeeded", None)
    return result


def _persisted_state_failure_result(
    *,
    tool: str,
    action: str,
    error: Exception,
    task: dict[str, Any] | None = None,
    project: dict[str, Any] | None = None,
) -> dict[str, Any]:
    result: dict[str, Any] = {
        "ok": False,
        "tool": tool,
        "action": action,
        "retryable": True,
        "statePersisted": True,
        "synced": False,
        "error": f"{action} state persisted locally but follow-up state update failed: {error}; retry to complete",
    }
    if task is not None:
        result["task"] = task
    if project is not None:
        result["project"] = project
    return result


def _uncommitted_state_failure_result(*, tool: str, action: str, error: Exception) -> dict[str, Any]:
    return {
        "ok": False,
        "tool": tool,
        "action": action,
        "retryable": True,
        "statePersisted": False,
        "synced": False,
        "error": f"{action} could not persist local state: {error}; retry to complete",
    }


def _pull_failed_response(tool: str, action: str) -> dict[str, Any]:
    return {
        "ok": False,
        "tool": tool,
        "action": action,
        "error": "cannot pull authoritative project state from shared storage; retry",
        "retryable": True,
    }


