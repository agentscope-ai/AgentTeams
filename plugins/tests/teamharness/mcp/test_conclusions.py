# -*- coding: utf-8 -*-
"""TH-9: the completion report must carry a conclusions array (3-5 items).

- fewer than 3: warning on the tool result, still accepted (default);
  rejected outright when AGENTTEAMS_TASK_CONCLUSIONS_STRICT is on;
- more than 5: all persisted on the task meta, the completion event shows
  the first 5 plus a pointer to shared/tasks/{task-id}/meta.json;
- conclusions are excluded from the result digest, so a digest-exact
  retry may carry a repaired list;
- a conclusions-less submission keeps the legacy byte-exact event text.

The tests enter through the assembled server (refs proxies keep
server-attribute monkeypatches visible) and fake only the storage
sync/publish and the Matrix HTTP send; the real
``_task_completion_notification`` + ``_send_task_completion_notification``
run in between.
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


PROJECT_ID = "conc-project"
ROOM = "!concroom:matrix.local"
LEADER = "@lead:matrix.local"
HUMAN = "@luo:matrix.local"
WORKER = "@worker-a:matrix.local"


class _FakeResponse:
    def __init__(self, payload: dict[str, Any]) -> None:
        self._payload = json.dumps(payload).encode("utf-8")

    def read(self) -> bytes:
        return self._payload

    def __enter__(self) -> "_FakeResponse":
        return self

    def __exit__(self, *exc: Any) -> bool:
        return False


class _SendCapture:
    """Stands in for urlopen on the Matrix send path."""

    def __init__(self) -> None:
        self.requests: list[tuple[str, dict[str, Any]]] = []

    def urlopen(self, request: Any, timeout: Any = None) -> _FakeResponse:
        url = request.full_url if hasattr(request, "full_url") else str(request)
        body = json.loads(request.data.decode("utf-8") or "{}")
        self.requests.append((url, body))
        return _FakeResponse({"event_id": f"$evt-{len(self.requests)}"})


def _write_project_and_task(workspace: Path, task_id: str) -> str:
    project = {
        "project_id": PROJECT_ID,
        "title": "Conclusions contract",
        "status": "active",
        "tasks": [
            {
                "task_id": task_id,
                "title": "Produce a result",
                "assigned_to": WORKER,
                "depends_on": [],
                "status": "in_progress",
            }
        ],
        "requester_report": {"pending": False, "sent_at": "2026-11-03T08:00:00Z"},
    }
    task = {
        "task_id": task_id,
        "project_id": PROJECT_ID,
        "room_id": ROOM,
        "status": "in_progress",
        "assigned_to": WORKER,
    }
    server._write_json(workspace / "shared" / "projects" / PROJECT_ID / "meta.json", project)
    server._write_json(workspace / "shared" / "tasks" / task_id / "meta.json", task)
    (workspace / "shared" / "tasks" / task_id / "result.md").write_text(
        "# Result\n\nDone.", encoding="utf-8"
    )
    return task_id


def _prepare(workspace: Path, monkeypatch: pytest.MonkeyPatch) -> _SendCapture:
    monkeypatch.setattr(server, "_pull_project", lambda *a, **k: True)
    monkeypatch.setattr(server, "_pull_task", lambda *a, **k: True)
    monkeypatch.setattr(server, "_sync_task", lambda *a, **k: True)
    monkeypatch.setattr(server, "_sync_project", lambda *a, **k: True)
    monkeypatch.setattr(server, "_publish_task_artifacts", lambda *a, **k: [])
    monkeypatch.setattr(
        server,
        "_validate_assignee_membership",
        lambda room, assignee: {"ok": True, "member": True},
    )
    monkeypatch.setattr(server.notify, "_team_leader_matrix_id", lambda: LEADER)
    monkeypatch.setattr(server.notify, "_team_human_matrix_ids", lambda: [HUMAN])
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.local")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    monkeypatch.delenv("AGENTTEAMS_TASK_CONCLUSIONS_STRICT", raising=False)
    capture = _SendCapture()
    monkeypatch.setattr(server.urllib.request, "urlopen", capture.urlopen)
    return capture


def _submit(workspace: Path, task_id: str, **payload: Any) -> dict[str, Any]:
    payload.setdefault("status", "SUCCESS")
    payload.setdefault("summary", "The result is ready.")
    return server._taskflow(
        {
            "role": "worker",
            "action": "submit_task",
            "workspaceDir": str(workspace),
            "payload": {"taskId": task_id, **payload},
        }
    )


def _read_task(workspace: Path, task_id: str) -> dict[str, Any]:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    return json.loads(path.read_text(encoding="utf-8"))


def _event_text(capture: _SendCapture) -> str:
    url, body = capture.requests[-1]
    assert "/send/m.room.message/" in url
    return body["body"]


def test_three_conclusions_no_warning_and_event_block(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    task_id = _write_project_and_task(tmp_path, "conc-task-3")
    conclusions = ["The deliverable is written.", "Criteria 1-2 pass.", "Nothing left open."]

    submitted = _submit(tmp_path, task_id, conclusions=conclusions)

    assert submitted["ok"] is True
    assert "warnings" not in submitted
    assert submitted["notification"]["sent"] is True
    assert _read_task(tmp_path, task_id)["conclusions"] == conclusions
    assert _event_text(capture) == (
        f"{LEADER} TASK_COMPLETED: {task_id} - "
        f"Result: shared/tasks/{task_id}/result.md"
        f"\n- Worker: {WORKER}"
        "\nThe result is ready."
        "\n- Conclusions:"
        f"\n  1. {conclusions[0]}"
        f"\n  2. {conclusions[1]}"
        f"\n  3. {conclusions[2]}"
    )
    mentions = capture.requests[-1][1].get("m.mentions", {}).get("user_ids", [])
    assert LEADER in mentions and HUMAN in mentions


def test_two_conclusions_warns_but_accepts(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    task_id = _write_project_and_task(tmp_path, "conc-task-2")
    conclusions = ["First.", "Second."]

    submitted = _submit(tmp_path, task_id, conclusions=conclusions)

    assert submitted["ok"] is True
    assert len(submitted["warnings"]) == 1
    assert "3-5 conclusions" in submitted["warnings"][0]
    assert "got 2" in submitted["warnings"][0]
    assert _read_task(tmp_path, task_id)["conclusions"] == conclusions
    text = _event_text(capture)
    assert text.endswith(
        f"\n- Conclusions:\n  1. {conclusions[0]}\n  2. {conclusions[1]}"
    )
    assert "more conclusions" not in text


def test_no_conclusions_warns_and_keeps_legacy_byte_identity(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    task_id = _write_project_and_task(tmp_path, "conc-task-none")

    submitted = _submit(tmp_path, task_id)

    assert submitted["ok"] is True
    assert "got 0" in submitted["warnings"][0]
    assert "conclusions" not in _read_task(tmp_path, task_id)
    assert _event_text(capture) == (
        f"{LEADER} TASK_COMPLETED: {task_id} - "
        f"Result: shared/tasks/{task_id}/result.md"
        f"\n- Worker: {WORKER}"
        "\nThe result is ready."
    )


def test_six_conclusions_truncates_event_with_pointer(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    task_id = _write_project_and_task(tmp_path, "conc-task-6")
    conclusions = [f"Point {i}." for i in range(1, 7)]

    submitted = _submit(tmp_path, task_id, conclusions=conclusions)

    assert submitted["ok"] is True
    assert len(submitted["warnings"]) == 1
    assert "carries 6" in submitted["warnings"][0]
    # The full list stays on the task meta.
    assert _read_task(tmp_path, task_id)["conclusions"] == conclusions
    text = _event_text(capture)
    assert "  5. Point 5." in text
    assert "Point 6." not in text
    assert text.endswith(
        f"  5. Point 5.\n  …1 more conclusions in shared/tasks/{task_id}/meta.json"
    )


def test_strict_mode_rejects_two_conclusions_before_any_write(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    monkeypatch.setenv("AGENTTEAMS_TASK_CONCLUSIONS_STRICT", "1")
    task_id = _write_project_and_task(tmp_path, "conc-task-strict")

    result = _submit(tmp_path, task_id, conclusions=["Only one.", "Two."])

    assert result["ok"] is False
    assert "AGENTTEAMS_TASK_CONCLUSIONS_STRICT" in result["error"]
    assert "conclusions" in result["error"]
    task = _read_task(tmp_path, task_id)
    assert task["status"] == "in_progress"
    assert "submission_id" not in task
    assert "conclusions" not in task
    assert capture.requests == []


def test_strict_mode_accepts_three_conclusions(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    monkeypatch.setenv("AGENTTEAMS_TASK_CONCLUSIONS_STRICT", "true")
    task_id = _write_project_and_task(tmp_path, "conc-task-strict-ok")

    submitted = _submit(
        tmp_path, task_id, conclusions=["A.", "B.", "C."]
    )

    assert submitted["ok"] is True
    assert "warnings" not in submitted


def test_strict_mode_rejects_missing_conclusions(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    monkeypatch.setenv("AGENTTEAMS_TASK_CONCLUSIONS_STRICT", "yes")
    task_id = _write_project_and_task(tmp_path, "conc-task-strict-none")

    result = _submit(tmp_path, task_id)

    assert result["ok"] is False
    assert "AGENTTEAMS_TASK_CONCLUSIONS_STRICT" in result["error"]
    assert "got 0" in result["error"]
    assert capture.requests == []


def test_resubmit_exact_retry_reuses_event_and_updates_conclusions(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    task_id = _write_project_and_task(tmp_path, "conc-task-retry")
    first = ["Old A.", "Old B.", "Old C."]

    submitted = _submit(tmp_path, task_id, conclusions=first)
    assert submitted["ok"] is True
    assert submitted.get("reused") is not True
    first_txn = capture.requests[-1][0]
    assert submitted["notification"]["sent"] is True

    repaired = ["New A.", "New B.", "New C."]
    resubmitted = _submit(tmp_path, task_id, conclusions=repaired)

    assert resubmitted["ok"] is True
    assert resubmitted["reused"] is True
    assert resubmitted["notification"]["reused"] is True
    assert resubmitted["notification"]["eventId"] == submitted["notification"]["eventId"]
    # Digest-exact retry: same Matrix transaction id, no second send.
    assert capture.requests[-1][0] == first_txn
    assert len(capture.requests) == 1
    # The repaired list (digest-excluded) is persisted.
    assert _read_task(tmp_path, task_id)["conclusions"] == repaired


def test_normalization_drops_empty_and_caps_items(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    task_id = _write_project_and_task(tmp_path, "conc-task-norm")
    long_item = "x" * 600

    submitted = _submit(
        tmp_path,
        task_id,
        conclusions=["  padded  ", "", None, long_item],
    )

    assert submitted["ok"] is True
    normalized = _read_task(tmp_path, task_id)["conclusions"]
    assert normalized == ["padded", long_item[:500] + "..."]
    # 2 items after normalization -> warning (not a rejection).
    assert "got 2" in submitted["warnings"][0]


def test_conclusions_not_a_list_is_rejected(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    task_id = _write_project_and_task(tmp_path, "conc-task-badtype")

    result = _submit(tmp_path, task_id, conclusions="not a list")

    assert result["ok"] is False
    assert "conclusions must be a list" in result["error"]
    assert _read_task(tmp_path, task_id)["status"] == "in_progress"
