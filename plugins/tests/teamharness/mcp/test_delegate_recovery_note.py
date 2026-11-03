"""TH18: delegate_task injects a recovery note for tasks assigned by an
earlier session of the dispatch process.

delegate_task runs with the leader role, so ``taskflow._SESSION_STARTED_AT``
(computed once at import time) is the start of the MCP process issuing the
dispatch — NOT of the receiving Worker's host: when delegate_task
(re)dispatches a task whose assignment predates the current session, the
session that assigned it is gone, and the notification must carry the fixed
recovery note after the spec preview. The task meta records ``recoveryNoteAt``
ONLY once the note has actually been delivered (a failed send must not count
as injected, so a same-session retry re-sends the note) — and the note IS
re-injected after the next restart of the dispatch process.

The session start is monkeypatched on ``server.taskflow._SESSION_STARTED_AT``
with a value derived from the current UTC second (no real sleep or
wall-clock waiting); the fake Matrix homeserver below is capture-only, so
every assertion runs against the REAL notification text assembled by
room/notify.py.
"""

from __future__ import annotations

import datetime
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import sys
import threading
from pathlib import Path
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


WORKER_A = "@alice:example.test"
WORKER_B = "@bob:example.test"
ROOM = "!taskroom:example.test"

NOTE = (
    "The MCP process issuing this dispatch (re)started after this task was\n"
    "assigned, so the session that assigned it is gone. Inspect the current\n"
    "worktree, git history and external state (task record, PRs, sent messages)\n"
    "before continuing. Do not repeat an already completed external operation.\n"
    "Finish the original assignment and report its status."
)


def _fake_clock() -> dict[str, str]:
    """before < start < after, anchored on the current UTC second: the
    monkeypatched session start is a point the wall clock has already
    passed, so the real recoveryNoteAt stamp written during the test is
    always >= start without any sleeping."""
    now = datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)

    def fmt(moment: datetime.datetime) -> str:
        return moment.isoformat().replace("+00:00", "Z")

    return {
        "before": fmt(now - datetime.timedelta(hours=1)),
        "start": fmt(now),
        "after": fmt(now + datetime.timedelta(hours=1)),
    }


def _start_session(monkeypatch: pytest.MonkeyPatch, clock: dict[str, str]) -> None:
    """Pretend the MCP process (re)started at clock["start"]."""
    monkeypatch.setattr(server.taskflow, "_SESSION_STARTED_AT", clock["start"])


def _write_task(
    workspace: Path,
    task_id: str = "recov-project-01",
    project_id: str = "recov-project",
    *,
    assignee: str = WORKER_A,
    **meta: Any,
) -> None:
    project = {
        "project_id": project_id,
        "title": "Recovery note contract",
        "status": "active",
        "tasks": [
            {
                "task_id": task_id,
                "title": "Produce a result",
                "assigned_to": assignee,
                "depends_on": [],
                "status": "prepared",
            }
        ],
    }
    task = {
        "task_id": task_id,
        "project_id": project_id,
        "room_id": ROOM,
        "status": "prepared",
        "assigned_to": assignee,
        **meta,
    }
    server._write_json(workspace / "shared" / "projects" / project_id / "meta.json", project)
    server._write_json(workspace / "shared" / "tasks" / task_id / "meta.json", task)


def _delegate(
    workspace: Path,
    task_id: str,
    assigned_to: str,
    spec: str = "Do the thing.",
) -> dict[str, Any]:
    arguments = {
        "role": "leader",
        "action": "delegate_task",
        "workspaceDir": str(workspace),
        "payload": {
            "projectId": "recov-project",
            "taskId": task_id,
            "roomId": ROOM,
            "assignedTo": assigned_to,
            "title": "Produce a result",
            "spec": spec,
        },
    }
    response = server.call_tool("taskflow", arguments)
    return json.loads(response["content"][0]["text"])


@pytest.fixture
def matrix_capture(monkeypatch: pytest.MonkeyPatch) -> dict[str, Any]:
    """Capture-only homeserver: the REAL _send_delegate_notification runs
    against it and every assignment event body is recorded verbatim."""
    state: dict[str, Any] = {"bodies": []}

    class _Handler(BaseHTTPRequestHandler):
        def do_PUT(self) -> None:  # noqa: N802
            length = int(self.headers.get("Content-Length") or 0)
            state["bodies"].append(json.loads(self.rfile.read(length).decode("utf-8")))
            payload = json.dumps({"event_id": f"$evt-{len(state['bodies'])}"}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *args: Any) -> None:  # silence
            pass

    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", f"http://127.0.0.1:{httpd.server_address[1]}")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    monkeypatch.setattr(server, "_pull_project", lambda *_args, **_kwargs: True)
    monkeypatch.setattr(server, "_sync_task", lambda *_args, **_kwargs: True)
    monkeypatch.setattr(server, "_validate_assignee_membership", lambda *_args, **_kwargs: {"ok": True})
    try:
        yield state
    finally:
        httpd.shutdown()


def test_cross_restart_leftover_gets_recovery_note(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    matrix_capture: dict[str, Any],
) -> None:
    """1. assigned_at predates the session start: the note is appended
    after the spec preview, recoveryNoteAt is stamped on the meta, and the
    result reports recoveryNote=true."""
    clock = _fake_clock()
    _start_session(monkeypatch, clock)
    _write_task(tmp_path, assigned_at=clock["before"])

    result = _delegate(tmp_path, "recov-project-01", WORKER_A)

    assert result["ok"] is True
    assert result["task"]["status"] == "assigned"
    assert result["recoveryNote"] is True
    assert matrix_capture["bodies"][0]["body"] == (
        f"{WORKER_A} You are assigned task **recov-project-01**: Produce a result\n\n"
        f"Do the thing.\n\n{NOTE}"
    )
    meta = server._read_json(tmp_path / "shared" / "tasks" / "recov-project-01" / "meta.json")
    assert meta.get("recoveryNoteAt")
    assert meta["recoveryNoteAt"] >= clock["start"]


def test_same_session_redelegation_does_not_reinject(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    matrix_capture: dict[str, Any],
) -> None:
    """2. The first delegation of a cross-restart leftover injects and
    stamps recoveryNoteAt; a second delegation of the SAME task within the
    same session (a re-delegation to a new assignee, which forces a fresh
    send) must NOT carry the note again."""
    clock = _fake_clock()
    _start_session(monkeypatch, clock)
    _write_task(tmp_path, assigned_at=clock["before"])

    first = _delegate(tmp_path, "recov-project-01", WORKER_A)
    assert first["ok"] is True
    assert first["recoveryNote"] is True
    assert NOTE in matrix_capture["bodies"][0]["body"]

    second = _delegate(tmp_path, "recov-project-01", WORKER_B)
    assert second["ok"] is True
    assert second["task"]["assigned_to"] == WORKER_B
    assert second["recoveryNote"] is False
    assert len(matrix_capture["bodies"]) == 2
    assert NOTE not in matrix_capture["bodies"][1]["body"]


def test_task_created_in_this_session_gets_no_note(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    matrix_capture: dict[str, Any],
) -> None:
    """3. assigned_at after the session start: not a cross-restart
    leftover, so no note, no recoveryNoteAt stamp, recoveryNote=false."""
    clock = _fake_clock()
    _start_session(monkeypatch, clock)
    _write_task(tmp_path, assigned_at=clock["after"])

    result = _delegate(tmp_path, "recov-project-01", WORKER_A)

    assert result["ok"] is True
    assert result["recoveryNote"] is False
    assert NOTE not in matrix_capture["bodies"][0]["body"]
    meta = server._read_json(tmp_path / "shared" / "tasks" / "recov-project-01" / "meta.json")
    assert "recoveryNoteAt" not in meta


def test_previous_sessions_note_is_reinjected(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    matrix_capture: dict[str, Any],
) -> None:
    """4. recoveryNoteAt predates THIS session start (a previous session
    already injected): the note is injected again and re-stamped to this
    session."""
    clock = _fake_clock()
    _start_session(monkeypatch, clock)
    _write_task(
        tmp_path,
        assigned_at=clock["before"],
        recoveryNoteAt=clock["before"],
    )

    result = _delegate(tmp_path, "recov-project-01", WORKER_A)

    assert result["ok"] is True
    assert result["recoveryNote"] is True
    assert NOTE in matrix_capture["bodies"][0]["body"]
    meta = server._read_json(tmp_path / "shared" / "tasks" / "recov-project-01" / "meta.json")
    assert meta["recoveryNoteAt"] >= clock["start"]
    assert meta["recoveryNoteAt"] != clock["before"]


def test_spec_preview_truncation_contract_survives_the_note(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    matrix_capture: dict[str, Any],
) -> None:
    """5. A spec longer than 500 chars keeps the original preview contract
    (first 500 chars + "...") and the note is appended after the preview,
    verbatim and untruncated."""
    clock = _fake_clock()
    _start_session(monkeypatch, clock)
    spec = "x" * 600
    _write_task(tmp_path, assigned_at=clock["before"])

    result = _delegate(tmp_path, "recov-project-01", WORKER_A, spec=spec)

    assert result["ok"] is True
    assert result["recoveryNote"] is True
    preview = spec[:500] + "..."
    body = matrix_capture["bodies"][0]["body"]
    assert body == (
        f"{WORKER_A} You are assigned task **recov-project-01**: Produce a result\n\n"
        f"{preview}\n\n{NOTE}"
    )
    # The preview rule is computed over the spec alone; the note follows.
    assert body.endswith(NOTE)
    assert NOTE not in spec[:500] + "..."


def test_missing_assignment_time_gets_no_note(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    matrix_capture: dict[str, Any],
) -> None:
    """1b. The meta carries neither assigned_at nor createdAt: there is no
    evidence the task predates this session, so no note is injected."""
    clock = _fake_clock()
    _start_session(monkeypatch, clock)
    _write_task(tmp_path)

    result = _delegate(tmp_path, "recov-project-01", WORKER_A)

    assert result["ok"] is True
    assert result["recoveryNote"] is False
    assert NOTE not in matrix_capture["bodies"][0]["body"]
    meta = server._read_json(tmp_path / "shared" / "tasks" / "recov-project-01" / "meta.json")
    assert "recoveryNoteAt" not in meta


def test_created_at_fallback_marks_the_leftover(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    matrix_capture: dict[str, Any],
) -> None:
    """1c. assigned_at missing but createdAt predates the session start:
    the createdAt fallback is the assignment evidence and the note is
    injected."""
    clock = _fake_clock()
    _start_session(monkeypatch, clock)
    _write_task(tmp_path, createdAt=clock["before"])

    result = _delegate(tmp_path, "recov-project-01", WORKER_A)

    assert result["ok"] is True
    assert result["recoveryNote"] is True
    assert NOTE in matrix_capture["bodies"][0]["body"]


def test_failed_send_retry_keeps_recovery_note(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """R1. A delegate send that FAILS (Matrix error) must not count as
    "already injected": the failed attempt leaves the task prepared with
    the persisted transaction nonce but NO recoveryNoteAt, and a same-
    session retry of the same logical delegation (same assignee) re-sends
    the note — reusing the persisted transaction id, so the retry's Matrix
    PUT path (txid) and body are identical to the failed attempt's."""
    clock = _fake_clock()
    _start_session(monkeypatch, clock)
    _write_task(tmp_path, assigned_at=clock["before"])

    sends: dict[str, Any] = {"puts": [], "fail_first": True}

    class _Handler(BaseHTTPRequestHandler):
        def do_PUT(self) -> None:  # noqa: N802
            length = int(self.headers.get("Content-Length") or 0)
            sends["puts"].append(
                {"path": self.path, "body": json.loads(self.rfile.read(length).decode("utf-8"))}
            )
            if sends["fail_first"]:
                sends["fail_first"] = False
                status = 500
                payload = {"errcode": "M_UNKNOWN", "error": "simulated network failure"}
            else:
                status = 200
                payload = {"event_id": f"$evt-{len(sends['puts'])}"}
            data = json.dumps(payload).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *args: Any) -> None:  # silence
            pass

    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", f"http://127.0.0.1:{httpd.server_address[1]}")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    monkeypatch.setattr(server, "_pull_project", lambda *_args, **_kwargs: True)
    monkeypatch.setattr(server, "_sync_task", lambda *_args, **_kwargs: True)
    monkeypatch.setattr(server, "_validate_assignee_membership", lambda *_args, **_kwargs: {"ok": True})
    try:
        first = _delegate(tmp_path, "recov-project-01", WORKER_A)
        assert first["ok"] is False
        assert first["retryable"] is True
        assert first["notification"]["sent"] is False

        # The failed attempt must NOT count as "already injected": the
        # prepared meta carries the retry nonce but no recoveryNoteAt.
        meta = server._read_json(tmp_path / "shared" / "tasks" / "recov-project-01" / "meta.json")
        assert meta["status"] == "prepared"
        assert meta.get("notifyTxn")
        assert "recoveryNoteAt" not in meta

        retry = _delegate(tmp_path, "recov-project-01", WORKER_A)
    finally:
        httpd.shutdown()

    assert retry["ok"] is True
    assert retry["recoveryNote"] is True
    assert len(sends["puts"]) == 2
    failed_put, retry_put = sends["puts"]
    # Same logical notification on retry: identical transaction id (the
    # homeserver deduplicates) and byte-identical body, and the retry's
    # body still carries the recovery note.
    assert failed_put["path"] == retry_put["path"]
    assert failed_put["body"] == retry_put["body"]
    assert retry_put["body"]["body"] == (
        f"{WORKER_A} You are assigned task **recov-project-01**: Produce a result\n\n"
        f"Do the thing.\n\n{NOTE}"
    )
    meta = server._read_json(tmp_path / "shared" / "tasks" / "recov-project-01" / "meta.json")
    assert meta["recoveryNoteAt"]
    assert meta["recoveryNoteAt"] >= clock["start"]
