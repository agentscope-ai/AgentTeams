# build_review_context (TH-12): review context assembled from local state only
from __future__ import annotations

import json

from pathlib import Path

from typing import Any

# Cross-group imports are top-level absolutes (see the note in
# transitions.py): mcp/ on sys.path keeps one module identity in both
# load modes.
from mcp.artifact.publish import _normalize_workspace_artifact_path, _path_is_under
from mcp.common.config import _runtime_team_name
from mcp.common.io import _read_json
from mcp.common.payload import _payload, _safe_id
from mcp.common.paths import _optional_workspace_dir, _task_state_path

# Closed SIG-* event_type set, single-sourced at
# plugins/teamharness/contracts/signal-mapping.json (v1.2). Only events
# carrying one of these types count as signal refs for review.
SIGNAL_EVENT_TYPES = frozenset({
    "SIG-COMPLETE",
    "SIG-DECISION",
    "SIG-VERDICT",
    "SIG-ATTENTION",
    "SIG-MISMATCH",
    "SIG-CONTRACT",
    "SIG-ARTIFACT",
})

# The fields that make up a signal ref: the event's identity plus the
# result fingerprint, not the raw event stream.
_SIGNAL_REF_KEYS = (
    "event_id",
    "event_type",
    "ref",
    "fingerprint",
    "source_event",
    "payload",
    "occurred_at",
)


def _review_signals(workspace_dir: Path, team: str, task_id: str) -> list[dict[str, Any]]:
    """S2 signal refs for one task, read from the local events mirror.

    Reads shared/events/{team}/*.json with the same pattern as
    projectflow list_events. Only events that carry a closed SIG-*
    event type AND belong to this task are returned, so group history,
    non-signal events, and other tasks' events never enter the review
    context.
    """
    signals: list[dict[str, Any]] = []
    events_dir = workspace_dir / "shared" / "events" / team
    if events_dir.is_dir():
        for path in sorted(events_dir.glob("*.json")):
            try:
                ev = json.loads(path.read_text(encoding="utf-8"))
            except Exception:
                continue
            if not isinstance(ev, dict) or not isinstance(ev.get("event_id"), str):
                continue
            if str(ev.get("task_id") or "") != task_id:
                continue
            if str(ev.get("event_type") or "").strip() not in SIGNAL_EVENT_TYPES:
                continue
            signals.append({key: ev.get(key) for key in _SIGNAL_REF_KEYS})
    signals.sort(key=lambda s: (str(s.get("occurred_at") or ""), str(s.get("event_id") or "")))
    return signals


def _review_artifacts(arguments: dict[str, Any], task_id: str) -> list[str]:
    """Artifact refs for one task, from its task meta: the declared
    deliverables plus result_path. Only workspace-relative paths under
    shared/tasks/{task_id}/ (the same invariant submit_task validates)
    are kept; anything else is filtered out of the review context."""
    prefix = f"shared/tasks/{task_id}"
    task = _read_json(_task_state_path(arguments, task_id))
    artifacts: list[str] = []
    raw_items: list[Any] = []
    deliverables = task.get("deliverables")
    if isinstance(deliverables, list):
        raw_items.extend(deliverables)
    result_path = str(task.get("result_path") or "").strip()
    if result_path:
        raw_items.append(result_path)
    for item in raw_items:
        try:
            normalized, _is_directory = _normalize_workspace_artifact_path(str(item))
        except ValueError:
            continue
        normalized = normalized.rstrip("/")
        if not _path_is_under(normalized, prefix):
            continue
        if normalized not in artifacts:
            artifacts.append(normalized)
    return artifacts


def build_review_context(arguments: dict[str, Any]) -> dict[str, Any]:
    """Assemble the review context for one task (TH-12).

    Returns exactly two ref classes — S2 signal refs (closed SIG-*
    events from the local events mirror) and artifact refs (the task's
    deliverables + result_path from its meta.json) — and nothing else:
    no group history, no chat noise. A task with no refs of either
    class returns an empty set, not an error. Read-only and local:
    this tool never contacts Matrix.
    """
    payload = _payload(arguments)
    raw_task_id = (
        payload.get("taskId")
        or payload.get("task_id")
        or arguments.get("taskId")
        or arguments.get("task_id")
    )
    try:
        task_id = _safe_id(raw_task_id, "taskId")
    except ValueError as exc:
        return {"ok": False, "tool": "build_review_context", "error": str(exc)}
    # Flat arguments are also accepted, like the other tools: `team` is
    # not a _payload alias, so it is read from the arguments too.
    team = str(
        payload.get("team")
        or arguments.get("team")
        or _runtime_team_name()
        or ""
    ).strip() or "global"
    workspace_dir = _optional_workspace_dir(arguments)
    base: dict[str, Any] = {
        "ok": True,
        "tool": "build_review_context",
        "task_id": task_id,
        "team": team,
        "signals": [],
        "artifacts": [],
    }
    if workspace_dir is None:
        return base
    base["signals"] = _review_signals(workspace_dir, team, task_id)
    base["artifacts"] = _review_artifacts(arguments, task_id)
    return base
