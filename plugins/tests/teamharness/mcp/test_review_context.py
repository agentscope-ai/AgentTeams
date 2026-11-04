# -*- coding: utf-8 -*-
"""TH-12: build_review_context returns exactly two ref classes.

- S2 signal refs: only events carrying a closed SIG-* event type for
  the requested task; group history, non-signal events, other tasks'
  events, and undecodable files are filtered out;
- artifact refs: only the task's deliverables + result_path that stay
  under shared/tasks/{task_id}/;
- a task with no refs returns an empty set, not an error;
- the tool is read-only and local (no Matrix access needed).
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


TEAM = "th12"
TASK = "task-123"

SIGNAL_REF_KEYS = {
    "event_id",
    "event_type",
    "ref",
    "fingerprint",
    "source_event",
    "payload",
    "occurred_at",
}


def _event(event_id: str, event_type: str, task_id: str, occurred_at: str, **extra: Any) -> dict[str, Any]:
    event = {
        "event_id": event_id,
        "task_id": task_id,
        "loop_id": None,
        "event_type": event_type,
        "is_signal": True,
        "source_event": {"origin": "submit", "request_id": None},
        "ref": f"shared/tasks/{task_id}/meta.json",
        "fingerprint": "f" * 64,
        "iteration_index": None,
        "payload": {},
        "occurred_at": occurred_at,
    }
    event.update(extra)
    return event


def _write_event(events_dir: Path, name: str, data: Any) -> None:
    events_dir.mkdir(parents=True, exist_ok=True)
    if isinstance(data, dict):
        text = json.dumps(data, ensure_ascii=False)
    else:
        text = data
    (events_dir / name).write_text(text, encoding="utf-8")


def _write_task_meta(workspace: Path, task_id: str, meta: dict[str, Any]) -> None:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False), encoding="utf-8")


def _prepare(monkeypatch: pytest.MonkeyPatch, with_workspace: bool = True) -> Path:
    for key in (
        "TEAMHARNESS_RUNTIME_CONFIG",
        "TEAMHARNESS_SHARED_DIR",
        "AGENTTEAMS_SHARED_DIR",
        "QWENPAW_WORKING_DIR",
        "COPAW_WORKING_DIR",
    ):
        monkeypatch.delenv(key, raising=False)
    workspace = Path("/tmp/th12-review-workspace")
    if with_workspace:
        monkeypatch.setenv("TEAMHARNESS_SHARED_DIR", str(workspace / "shared"))
    return workspace


def _call(arguments: dict[str, Any]) -> dict[str, Any]:
    response = server.call_tool("build_review_context", arguments)
    return json.loads(response["content"][0]["text"])


def test_noisy_task_returns_only_signal_and_artifact_refs(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(monkeypatch)
    events_dir = tmp_path / "shared" / "events" / TEAM
    _write_event(events_dir, "ev_complete_1.json", _event(
        "ev_aaa111", "SIG-COMPLETE", TASK, "2026-11-03T08:00:00Z",
        payload={"summary": "done"},
    ))
    _write_event(events_dir, "ev_attention_1.json", _event(
        "ev_bbb222", "SIG-ATTENTION", TASK, "2026-11-03T09:00:00Z",
    ))
    # Noise: group chat history without a signal event_type.
    _write_event(events_dir, "ev_chat_noise.json", {
        "event_id": "ev_noise1",
        "task_id": TASK,
        "event_type": "CHAT",
        "is_signal": False,
        "payload": {"body": "lunch?"},
        "occurred_at": "2026-11-03T08:30:00Z",
    })
    _write_event(events_dir, "ev_no_type.json", {
        "event_id": "ev_noise2",
        "task_id": TASK,
        "occurred_at": "2026-11-03T08:31:00Z",
    })
    # Noise: a real signal, but for another task.
    _write_event(events_dir, "ev_other_task.json", _event(
        "ev_ccc333", "SIG-COMPLETE", "task-999", "2026-11-03T08:40:00Z",
    ))
    # Noise: undecodable file.
    _write_event(events_dir, "ev_broken.json", "{not json")

    _write_task_meta(tmp_path, TASK, {
        "task_id": TASK,
        "status": "completed",
        "deliverables": [
            "shared/tasks/task-123/result.md",
            "shared/tasks/task-123/output/report.md",
        ],
        "result_path": "shared/tasks/task-123/result.md",
    })

    context = _call({"taskId": TASK, "team": TEAM, "workspaceDir": str(tmp_path)})

    assert context["ok"] is True
    assert context["tool"] == "build_review_context"
    assert context["task_id"] == TASK
    assert context["team"] == TEAM
    # Exactly the two signal refs, ordered by (occurred_at, event_id).
    assert [s["event_id"] for s in context["signals"]] == ["ev_aaa111", "ev_bbb222"]
    assert {s["event_type"] for s in context["signals"]} == {"SIG-COMPLETE", "SIG-ATTENTION"}
    for signal in context["signals"]:
        assert set(signal) == SIGNAL_REF_KEYS
    # Exactly the artifact refs: deliverables + result_path (deduped).
    assert context["artifacts"] == [
        "shared/tasks/task-123/result.md",
        "shared/tasks/task-123/output/report.md",
    ]
    # No chat/group noise of any kind leaks into the context.
    dumped = json.dumps(context)
    assert "lunch?" not in dumped
    assert "ev_noise1" not in dumped
    assert "task-999" not in dumped


def test_no_refs_returns_empty_set_not_error(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(monkeypatch)

    context = _call({"taskId": "task-empty", "team": TEAM, "workspaceDir": str(tmp_path)})

    assert context["ok"] is True
    assert context["signals"] == []
    assert context["artifacts"] == []


def test_artifacts_outside_task_prefix_are_filtered(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(monkeypatch)
    _write_task_meta(tmp_path, TASK, {
        "task_id": TASK,
        "status": "completed",
        "deliverables": [
            "shared/tasks/task-999/borrowed.md",
            "reports/summary.md",
            "../escape.md",
        ],
        "result_path": "shared/other/result.md",
    })

    context = _call({"taskId": TASK, "team": TEAM, "workspaceDir": str(tmp_path)})

    assert context["ok"] is True
    assert context["signals"] == []
    assert context["artifacts"] == []


def test_env_workspace_resolution(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    # The workspace comes from TEAMHARNESS_SHARED_DIR (the production
    # shape: skills never pass workspaceDir), not from the arguments.
    workspace = tmp_path
    for key in (
        "TEAMHARNESS_RUNTIME_CONFIG",
        "AGENTTEAMS_SHARED_DIR",
        "QWENPAW_WORKING_DIR",
        "COPAW_WORKING_DIR",
    ):
        monkeypatch.delenv(key, raising=False)
    monkeypatch.setenv("TEAMHARNESS_SHARED_DIR", str(workspace / "shared"))
    events_dir = workspace / "shared" / "events" / TEAM
    _write_event(events_dir, "ev_complete_1.json", _event(
        "ev_aaa111", "SIG-COMPLETE", TASK, "2026-11-03T08:00:00Z",
    ))

    context = _call({"taskId": TASK, "team": TEAM})

    assert context["ok"] is True
    assert [s["event_id"] for s in context["signals"]] == ["ev_aaa111"]


def test_no_workspace_returns_empty_set(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(monkeypatch, with_workspace=False)

    context = _call({"taskId": TASK, "team": TEAM})

    assert context["ok"] is True
    assert context["signals"] == []
    assert context["artifacts"] == []


def test_invalid_task_id_rejected(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(monkeypatch)

    context = _call({"taskId": "bad id!", "team": TEAM, "workspaceDir": str(tmp_path)})

    assert context["ok"] is False
    assert context["tool"] == "build_review_context"
    assert "taskId must be a safe id" in context["error"]


def test_tool_registered_in_tools_list(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(monkeypatch)

    names = [tool["name"] for tool in server.list_tools()]

    assert "build_review_context" in names
    schema = server.TOOL_SCHEMAS["build_review_context"]
    assert "properties" in schema["inputSchema"]
