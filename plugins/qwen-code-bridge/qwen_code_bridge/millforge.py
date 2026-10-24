# -*- coding: utf-8 -*-
"""Millforge (千锻) contract integration for the bridge.

Two contracts are consumed here, both small and testable:

* **M4 / M5 — task state and notification tiers.** Files are the truth
  (machine tier), the session wake is the agent tier, and the room bell is the
  human tier. Failures must always reach the human tier.
* **T5 — coding-runs field mapping.** The bridge's task record is projected
  onto the platform's coding-runs read contract so the same run can later be
  surfaced through that API without a second source of truth.
"""

from __future__ import annotations

from typing import Any

TERMINAL_FAILURES = {"failed", "timeout", "killed"}

_TIER_BY_EVENT = {
    "queued": "machine",
    "running": "agent",
    "done": "agent",
    "failed": "human",
    "timeout": "human",
    "killed": "human",
}

_STATUS_MAP = {
    "queued": "queued",
    "running": "running",
    "done": "succeeded",
    "failed": "failed",
    "timeout": "timeout",
    "killed": "killed",
}


def notify_tier(event: str) -> str:
    """M5: route an event to ``machine`` / ``agent`` / ``human``.

    Failures and abnormal terminations always go to the human tier.
    """
    kind = str(event or "").strip()
    if kind in _TIER_BY_EVENT:
        return _TIER_BY_EVENT[kind]
    return "human" if kind.startswith("failed") else "machine"


def coding_run_record(task: dict[str, Any]) -> dict[str, Any]:
    """T5: project one task record onto the coding-runs read contract."""
    state = str(task.get("state") or "")
    return {
        "task_id": task.get("task_id"),
        "run": task.get("session_id") or task.get("task_id"),
        "cli": task.get("cli"),
        "worker": None,  # bridge runs are local; the platform fills this in
        "status": _STATUS_MAP.get(state, "unknown"),
        "exit_code": task.get("exit_code"),
        "started_at": task.get("started_at"),
        "ended_at": task.get("finished_at"),
        "wall_s": task.get("wall_s"),
        "log_bytes": len(str(task.get("log_tail") or "").encode("utf-8")),
        "prompt_file": None,  # prompts are inline for the bridge
        "summary": (task.get("log_tail") or "").strip().splitlines()[-1][:200]
        if task.get("log_tail")
        else "",
        "approval_mode": task.get("approval_mode"),
        "approval_level": task.get("approval_level"),
        "notify_tier": notify_tier(state),
    }


__all__ = ["TERMINAL_FAILURES", "coding_run_record", "notify_tier"]
