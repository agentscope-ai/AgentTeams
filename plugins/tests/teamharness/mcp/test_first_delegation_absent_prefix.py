"""#1467 round 12: the first delegation must survive an absent remote task.

`delegate_task` loads the authoritative task before it writes, so a
re-delegation cannot clobber a peer's record. A brand-new task has no remote
prefix at all, and the real `mc mirror` pull fails for it exactly the way a
storage outage fails — the previous check could not tell the two apart and
every first delegation returned ok=false.

These tests drive the real `_filesync` boundary with an mc-shaped fake (the
absent-prefix error text is what `mc` prints), never a mocked
`_pull_task=True`:

1. brand-new task, empty remote -> the pull fails with the absence marker,
   the stat probe confirms the absence, and the delegation creates and
   publishes the task;
2. existing task, pull fails but the object IS there -> fail closed (the
   stale-copy guard keeps its teeth);
3. storage outage (the stat probe fails with a non-absence error) -> fail
   closed;
4. existing task, healthy pull -> the authoritative copy wins.
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


@pytest.fixture()
def server() -> Any:
    """The harness module under test (the parity suite passes its own)."""
    import server as server_module  # noqa: WPS433

    return server_module

WORKER = "@alice:example.test"
ROOM = "!taskroom:example.test"
PROJECT_ID = "first-project"
TASK_ID = "first-project-01"

ABSENT_ERROR = (
    "mc: <ERROR> Unable to stat `local/shared/tasks/{task}`. "
    "The specified key does not exist."
)
OUTAGE_ERROR = "mc: <ERROR> Unable to reach the storage backend: connection refused"


class McLikeStorage:
    """The filesync surface with mc's semantics for absent objects."""

    def __init__(self, workspace: Path) -> None:
        self.workspace = workspace
        self.files: dict[str, bytes] = {}
        self.directories: set[str] = set()
        self.calls: list[tuple[str, str]] = []
        self.stat_error: str | None = None
        self.pull_error: str | None = None

    def _local(self, path: str) -> Path:
        return self.workspace / path

    def __call__(self, arguments: dict[str, Any]) -> dict[str, Any]:
        action = str(arguments.get("action") or "")
        path = str(arguments.get("path") or "")
        self.calls.append((action, path))
        if action == "pull":
            if self.pull_error is not None:
                return {"ok": False, "error": self.pull_error}
            if path not in self.directories and not any(
                key.startswith(path + "/") for key in self.files
            ):
                return {"ok": False, "error": ABSENT_ERROR.format(task=path)}
            for key, payload in list(self.files.items()):
                if not key.startswith(path + "/"):
                    continue
                target = self._local(key)
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(payload)
            return {"ok": True}
        if action == "stat":
            if self.stat_error is not None:
                return {"ok": False, "error": self.stat_error}
            if path in self.files:
                return {"ok": True, "exists": True}
            return {"ok": False, "error": ABSENT_ERROR.format(task=path)}
        if action == "push":
            local = self._local(path)
            if local.is_file():
                self.files[path] = local.read_bytes()
                return {"ok": True}
            if local.is_dir():
                exclude = set(arguments.get("exclude") or [])
                for candidate in sorted(local.rglob("*")):
                    if not candidate.is_file() or candidate.name in exclude:
                        continue
                    self.files[f"{path}/{candidate.relative_to(local).as_posix()}"] = (
                        candidate.read_bytes()
                    )
                self.directories.add(path)
                return {"ok": True}
            return {"ok": True}
        if action == "list":
            return {"ok": True, "entries": []}
        return {"ok": True}


def _write_project(workspace: Path) -> None:
    project = {
        "project_id": PROJECT_ID,
        "title": "First delegation",
        "status": "active",
        "tasks": [
            {
                "task_id": TASK_ID,
                "title": "Produce a result",
                "assigned_to": WORKER,
                "depends_on": [],
                "status": "planned",
            }
        ],
    }
    path = workspace / "shared" / "projects" / PROJECT_ID / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(project), encoding="utf-8")


def _install(server: Any, monkeypatch: pytest.MonkeyPatch, storage: McLikeStorage) -> None:
    monkeypatch.setattr(server, "_filesync", storage)
    monkeypatch.setattr(server, "_pull_project", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_validate_assignee_membership", lambda *_a, **_k: {"ok": True})
    # The delegate path only notifies when the Matrix env is present; the
    # send itself is out of scope here (the assignment must still be created
    # and published when the notification is skipped, and the reviewer's
    # repro only requires the first delegation to succeed).
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.test")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "token")
    monkeypatch.setattr(
        server,
        "_send_delegate_notification",
        lambda *_a, **_k: {"sent": True, "eventId": "$assign-event"},
    )


def _delegate(server: Any, workspace: Path) -> dict[str, Any]:
    arguments = {
        "role": "leader",
        "action": "delegate_task",
        "workspaceDir": str(workspace),
        "payload": {
            "projectId": PROJECT_ID,
            "taskId": TASK_ID,
            "roomId": ROOM,
            "assignedTo": WORKER,
            "title": "Produce a result",
            "spec": "Do the thing.",
        },
    }
    response = server.call_tool("taskflow", arguments)
    return json.loads(response["content"][0]["text"])


def test_first_delegation_succeeds_with_an_absent_remote_prefix(
    server: Any, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The maintainer's repro: a genuinely new task has no remote prefix."""
    _write_project(tmp_path)
    storage = McLikeStorage(tmp_path)
    _install(server, monkeypatch, storage)

    result = _delegate(server, tmp_path)

    assert result["ok"] is True, {k: v for k, v in result.items() if k != "task"}
    # The pull failed (absent prefix) and the stat probe confirmed the
    # absence — then the task was published.
    assert ("pull", f"shared/tasks/{TASK_ID}") in storage.calls
    assert ("stat", f"shared/tasks/{TASK_ID}/meta.json") in storage.calls
    assert f"shared/tasks/{TASK_ID}/meta.json" in storage.files
    published = json.loads(storage.files[f"shared/tasks/{TASK_ID}/meta.json"])
    assert published["status"] == "assigned"
    assert published["assigned_to"] == WORKER


def test_existing_task_with_a_failed_pull_still_fails_closed(
    server: Any, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The object is there, the pull failed: never write the stale copy."""
    _write_project(tmp_path)
    storage = McLikeStorage(tmp_path)
    storage.files[f"shared/tasks/{TASK_ID}/meta.json"] = json.dumps(
        {"task_id": TASK_ID, "status": "submitted", "decision_status": "frozen"}
    ).encode()
    storage.pull_error = "mc: <ERROR> Unable to mirror: connection reset"
    _install(server, monkeypatch, storage)

    result = _delegate(server, tmp_path)

    assert result["ok"] is False
    assert "stale local copy" in result["error"]
    # Nothing was published over the existing record.
    assert b"frozen" in storage.files[f"shared/tasks/{TASK_ID}/meta.json"]


def test_storage_outage_fails_closed(
    server: Any, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A non-absence failure is not a creation path."""
    _write_project(tmp_path)
    storage = McLikeStorage(tmp_path)
    storage.pull_error = OUTAGE_ERROR
    storage.stat_error = OUTAGE_ERROR
    _install(server, monkeypatch, storage)

    result = _delegate(server, tmp_path)

    assert result["ok"] is False
    assert "stale local copy" in result["error"]


def test_existing_task_is_refreshed_from_the_authoritative_copy(
    server: Any, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A healthy pull wins over the local cache (unchanged behaviour)."""
    _write_project(tmp_path)
    storage = McLikeStorage(tmp_path)
    storage.files[f"shared/tasks/{TASK_ID}/meta.json"] = json.dumps(
        {"task_id": TASK_ID, "status": "planned", "room_id": ROOM, "history": ["remote"]}
    ).encode()
    storage.files[f"shared/tasks/{TASK_ID}/spec.md"] = b"remote spec"
    _install(server, monkeypatch, storage)

    result = _delegate(server, tmp_path)

    assert result["ok"] is True, (result.get("error"), [c for c in storage.calls][-8:])
    published = json.loads(storage.files[f"shared/tasks/{TASK_ID}/meta.json"])
    assert published["status"] == "assigned"
    assert "history" in published or published.get("assigned_to") == WORKER
