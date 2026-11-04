"""INT-5: structured illegal-transition validation + SIG-MISMATCH emission.

Covers the transition-validation hardening on top of the existing
_assert_transition/_transition_task layer (no rebuild):
- legal transitions keep working (status + history written, no event)
- illegal transition -> IllegalTransitionError carrying from/to/action,
  state unchanged, structured error dict at the taskflow dispatch layer
- terminal states (the 4 values of TERMINAL_TASK_STATUSES) rejected with a
  "terminal state" message; unknown states get a distinct "unexpected state"
  message (no "terminal")
- SIG-MISMATCH emitted on rejection (deterministic id, idempotent re-reject)
- static audit: every task/task_meta status write sits in the whitelisted
  functions (_transition_task entry + _accept_task_result declared guard
  paths) — the detector self-proves it catches an injected bypass
- round 2 (public entry): the pre-``_transition_task`` guards (terminal
  immutability via _require_task_mutable, the ack/submit/report_progress
  ordering pre-checks, the cancel terminal guard) reject through the same
  structured surface as the table check — tests enter from ``_taskflow``
  and assert the ``illegal_transition`` dict (from/to) plus SIG-MISMATCH
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
import re
import sys
from pathlib import Path
from typing import Any

import pytest


REPO_ROOT = Path(__file__).resolve().parents[3]
MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"
SERVER_PATH = MCP_DIR / "server.py"

# W1-a2 (issue #1451 P6): the split moved _accept_task_result (the second
# whitelisted status-write site) into project/flow.py while _transition_task
# stays pinned in server.py. The static audit therefore scans the
# concatenated sources of both files; the detector and whitelist are
# unchanged.
SCAN_PATHS = (MCP_DIR / "server.py", MCP_DIR / "project" / "flow.py")

EVENT_FIELDS = {
    "event_id", "task_id", "loop_id", "event_type", "is_signal",
    "source_event", "ref", "fingerprint", "iteration_index", "payload",
    "occurred_at",
}

# Status writes are only legal inside the single transition entry
# (_transition_task) and the accept reconciliation paths (#1183 guard).
STATUS_WRITE_RE = re.compile(r'(?<!\w)(?:task|task_meta)\["status"\]\s*=')
WHITELIST_FUNCS = {"_transition_task", "_accept_task_result"}


def _load_server() -> Any:
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_transition_test", SERVER_PATH
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(MCP_DIR))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(MCP_DIR))
    return module


@pytest.fixture()
def server() -> Any:
    return _load_server()


def _arguments(workspace: Path, action: str, **payload: Any) -> dict[str, Any]:
    return {"workspaceDir": str(workspace), "action": action, "payload": payload}


def _write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding="utf-8")


def _write_task(workspace: Path, task_id: str, meta: dict[str, Any]) -> None:
    _write_json(workspace / "shared" / "tasks" / task_id / "meta.json", meta)


def _team(server: Any) -> str:
    return str(server._runtime_team_name() or "").strip() or "global"


class FakeFilesync:
    """Records _filesync calls (pull/push) instead of touching the network."""

    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    def __call__(self, arguments: dict[str, Any]) -> dict[str, Any]:
        self.calls.append(dict(arguments))
        return {
            "ok": True,
            "tool": "filesync",
            "action": arguments.get("action"),
            "path": arguments.get("path"),
        }


def test_legal_transition_passes(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    workspace = tmp_path / "agent"
    task_id = "int5-task-legal"
    _write_task(workspace, task_id, {"task_id": task_id, "status": "assigned", "project_id": ""})
    args = _arguments(workspace, "ack_task", taskId=task_id)
    task = server._load_task(args, task_id)

    old = server._transition_task(args, task, task_id, "in_progress", "ack_task")

    assert old == "assigned"
    assert task["status"] == "in_progress"
    on_disk = server._load_task(args, task_id)
    assert on_disk["status"] == "in_progress"
    entry = on_disk["history"][0]
    assert entry["from"] == "assigned"
    assert entry["to"] == "in_progress"
    assert entry["action"] == "ack_task"
    # A legal transition must not emit any signal event.
    events_dir = workspace / "shared" / "events"
    assert not events_dir.exists() or not any(events_dir.rglob("*.json"))


def test_illegal_transition_rejected(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    workspace = tmp_path / "agent"
    task_id = "int5-task-illegal"
    # "bogus" is neither in the transition table nor terminal: cancel reaches
    # the _assert_transition unknown-state branch through the dispatch layer
    # (the per-action pre-checks do not intercept it).
    _write_task(workspace, task_id, {"task_id": task_id, "status": "bogus", "project_id": ""})
    args = _arguments(workspace, "cancel_task", taskId=task_id, reason="audit probe")
    task = server._load_task(args, task_id)
    before = server._read_json(server._task_state_path(args, task_id))

    # Validation layer: structured exception with the from/to/action triple.
    with pytest.raises(server.IllegalTransitionError) as excinfo:
        server._transition_task(args, task, task_id, "cancelled", "cancel_task")
    exc = excinfo.value
    assert isinstance(exc, ValueError)  # internal protocol unchanged
    assert exc.from_status == "bogus"
    assert exc.to_status == "cancelled"
    assert exc.action == "cancel_task"
    assert str(exc).startswith("illegal_transition:")
    # State unchanged (in memory and on disk).
    assert task["status"] == "bogus"
    assert server._read_json(server._task_state_path(args, task_id)) == before

    # Dispatch layer: the same rejection returns the structured error dict.
    result = server._taskflow(args)
    assert result["ok"] is False
    assert result["tool"] == "taskflow"
    assert result["action"] == "cancel_task"
    assert result["error"] == "illegal_transition"
    assert result["from"] == "bogus"
    assert result["to"] == "cancelled"
    assert result["detail"].startswith("illegal_transition: cancel_task:")
    assert "unexpected state bogus" in result["detail"]
    assert "terminal" not in result["detail"]


def test_terminal_state_immutable(server: Any) -> None:
    # The 4 values of TERMINAL_TASK_STATUSES — PARTIAL/INTERRUPTED are not
    # terminal task statuses (#1183 moved PARTIAL out of the accepted set).
    for terminal in ("completed", "revision", "blocked", "cancelled"):
        with pytest.raises(server.IllegalTransitionError) as excinfo:
            server._assert_transition(terminal, "in_progress", "ack_task")
        message = str(excinfo.value)
        assert "terminal state" in message
        assert "(immutable)" in message
        assert terminal in message

    # Unknown (non-terminal) state: distinct message, no "terminal".
    with pytest.raises(server.IllegalTransitionError) as excinfo:
        server._assert_transition("no_such_state", "in_progress", "ack_task")
    message = str(excinfo.value)
    assert "terminal" not in message
    assert "unexpected state" in message

    # Same-state re-entry stays a legal no-op, terminal states included.
    server._assert_transition("completed", "completed", "ack_task")


def test_mismatch_signal_emitted(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    fake = FakeFilesync()
    monkeypatch.setattr(server, "_filesync", fake)
    workspace = tmp_path / "agent"
    task_id = "int5-task-mismatch"
    _write_task(workspace, task_id, {"task_id": task_id, "status": "in_progress", "project_id": ""})
    args = _arguments(workspace, "delegate_task", taskId=task_id)
    task = server._load_task(args, task_id)

    # in_progress -> assigned is not in the transition table.
    with pytest.raises(server.IllegalTransitionError):
        server._transition_task(args, task, task_id, "assigned", "delegate_task")

    team = _team(server)
    meta_bytes = (workspace / "shared" / "tasks" / task_id / "meta.json").read_bytes()
    fingerprint = hashlib.sha256(meta_bytes).hexdigest()
    expected_id = server._signal_event_id(
        team, "", task_id, "", "SIG-MISMATCH", "mismatch_recorded", None, fingerprint, 0, 0,
        "delegate_task|in_progress|assigned",
    )
    events_dir = workspace / "shared" / "events" / team
    files = sorted(events_dir.glob("*.json"))
    assert [f.name for f in files] == [f"{expected_id}.json"]  # deterministic id
    data = json.loads(files[0].read_text(encoding="utf-8"))
    assert set(data) == EVENT_FIELDS  # exactly the 11 schema fields
    assert data["event_id"] == expected_id
    assert data["event_type"] == "SIG-MISMATCH"
    assert data["is_signal"] is True
    assert data["task_id"] == task_id
    assert data["loop_id"] is None
    assert data["iteration_index"] is None
    assert data["source_event"] == {"origin": "mismatch_recorded", "request_id": None}
    assert data["ref"] == f"shared/tasks/{task_id}/meta.json"
    assert data["fingerprint"] == hashlib.sha256(
        (workspace / "shared" / "tasks" / task_id / "meta.json").read_bytes()
    ).hexdigest()
    assert data["payload"] == {
        "from_status": "in_progress",
        "to_status": "assigned",
        "action": "delegate_task",
    }

    # Re-rejecting the same logical event is idempotent: no second object,
    # no second mirror push.
    task2 = server._load_task(args, task_id)
    with pytest.raises(server.IllegalTransitionError):
        server._transition_task(args, task2, task_id, "assigned", "delegate_task")
    assert [f.name for f in sorted(events_dir.glob("*.json"))] == [f"{expected_id}.json"]
    pushes = [c for c in fake.calls if c.get("action") == "push"]
    assert [c["path"] for c in pushes] == [f"shared/events/{team}/{expected_id}.json"]


def _function_at_line(lines: list[str], index: int) -> str | None:
    """Top-level function owning the line at (0-based) index."""
    owner: str | None = None
    for i, line in enumerate(lines):
        if i > index:
            break
        match = re.match(r"^def (\w+)", line)
        if match:
            owner = match.group(1)
    return owner


def _status_write_violations(source: str) -> list[tuple[int, str, str | None]]:
    """(line_no, text, owner) for status writes outside the whitelist."""
    lines = source.splitlines()
    violations: list[tuple[int, str, str | None]] = []
    for i, line in enumerate(lines):
        if not STATUS_WRITE_RE.search(line):
            continue
        owner = _function_at_line(lines, i)
        if owner not in WHITELIST_FUNCS:
            violations.append((i + 1, line.strip(), owner))
    return violations


def test_status_write_coverage_audit() -> None:
    source = "".join(path.read_text(encoding="utf-8") + "\n" for path in SCAN_PATHS)
    lines = source.splitlines()
    write_lines = [i + 1 for i, line in enumerate(lines) if STATUS_WRITE_RE.search(line)]
    assert write_lines, "expected status write points in the scanned sources"
    # Every write point sits in a whitelisted function.
    assert _status_write_violations(source) == []
    # QC self-proof: an injected bypass write is detected (would go red).
    probe = (
        source
        + "\n\ndef _audit_probe_bypass(task: dict[str, Any]) -> None:\n"
        + '    task["status"] = "submitted"  # probe\n'
    )
    violations = _status_write_violations(probe)
    assert len(violations) == 1
    assert violations[0][2] == "_audit_probe_bypass"


# ---------------------------------------------------------------------------
# Round 2: the pre-_transition_task guards (terminal immutability,
# ack/submit/report_progress ordering, cancel terminal) must reject through
# the same structured surface as the table check.  Tests enter from the
# public _taskflow dispatch — not from _assert_transition — because the
# regression is exactly that these guards used to return a bare error
# string (no from/to, no SIG-MISMATCH) before reaching _transition_task.
# ---------------------------------------------------------------------------


def _single_mismatch_event(
    workspace: Path,
    server: Any,
    task_id: str,
    action: str,
    from_status: str,
    to_status: str,
) -> dict[str, Any]:
    """Exactly one event exists and it is the deterministic SIG-MISMATCH.

    The rejection discriminator (action|from|to) is part of the event
    identity (id_rule v1.2 rejection family): a rejection commits no
    state, so the ref bytes alone cannot distinguish two different
    rejected operations on the same task."""
    team = _team(server)
    events_dir = workspace / "shared" / "events" / team
    files = sorted(events_dir.glob("*.json"))
    meta_bytes = (workspace / "shared" / "tasks" / task_id / "meta.json").read_bytes()
    fingerprint = hashlib.sha256(meta_bytes).hexdigest()
    expected_id = server._signal_event_id(
        team, "", task_id, "", "SIG-MISMATCH", "mismatch_recorded", None, fingerprint, 0, 0,
        f"{action}|{from_status}|{to_status}",
    )
    assert [f.name for f in files] == [f"{expected_id}.json"]
    return json.loads(files[0].read_text(encoding="utf-8"))


def test_ack_terminal_via_taskflow_is_structured(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Public ack_task on a completed task: structured illegal_transition
    (from/to) + SIG-MISMATCH — not a bare error string.  Retry is
    idempotent (same deterministic id, no second object)."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    workspace = tmp_path / "agent"
    task_id = "int5r2-ack-completed"
    _write_task(workspace, task_id, {"task_id": task_id, "status": "completed", "project_id": ""})
    args = _arguments(workspace, "ack_task", taskId=task_id)

    result = server._taskflow(args)

    assert result["ok"] is False
    assert result["tool"] == "taskflow"
    assert result["error"] == "illegal_transition"
    assert result["from"] == "completed"
    assert result["to"] == "in_progress"
    assert "terminal state completed" in result["detail"]
    # State unchanged on disk.
    assert server._read_json(server._task_state_path(args, task_id))["status"] == "completed"
    # SIG-MISMATCH emitted for the rejection.
    event = _single_mismatch_event(workspace, server, task_id, "ack_task", "completed", "in_progress")
    assert event["event_type"] == "SIG-MISMATCH"
    assert event["payload"] == {
        "from_status": "completed",
        "to_status": "in_progress",
        "action": "ack_task",
    }
    # Retry: same rejection, same event id, no second object.
    result2 = server._taskflow(args)
    assert result2 == result
    team = _team(server)
    files = sorted((workspace / "shared" / "events" / team).glob("*.json"))
    assert len(files) == 1


def test_ack_planned_via_taskflow_is_structured(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """The ack ordering pre-check (planned -> delegate first) now rejects
    with the structured error + SIG-MISMATCH.  Note planned -> in_progress
    IS a legal table edge (reached via delegate_task) — this is an
    action-level rule, which is why it lives in the guard, not the table."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    workspace = tmp_path / "agent"
    task_id = "int5r2-ack-planned"
    _write_task(workspace, task_id, {"task_id": task_id, "status": "planned", "project_id": ""})
    args = _arguments(workspace, "ack_task", taskId=task_id)

    result = server._taskflow(args)

    assert result["ok"] is False
    assert result["error"] == "illegal_transition"
    assert result["from"] == "planned"
    assert result["to"] == "in_progress"
    assert "ack_task: task is 'planned'; delegate_task it first" in result["detail"]
    event = _single_mismatch_event(workspace, server, task_id, "ack_task", "planned", "in_progress")
    assert event["payload"] == {
        "from_status": "planned",
        "to_status": "in_progress",
        "action": "ack_task",
    }
    assert server._read_json(server._task_state_path(args, task_id))["status"] == "planned"


def test_submit_planned_via_taskflow_is_structured(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """The submit ordering pre-check (planned -> ack first): structured
    rejection + SIG-MISMATCH."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    workspace = tmp_path / "agent"
    task_id = "int5r2-submit-planned"
    _write_task(workspace, task_id, {"task_id": task_id, "status": "planned", "project_id": ""})
    args = _arguments(
        workspace, "submit_task", taskId=task_id, status="SUCCESS", summary="done"
    )

    result = server._taskflow(args)

    assert result["ok"] is False
    assert result["error"] == "illegal_transition"
    assert result["from"] == "planned"
    assert result["to"] == "submitted"
    assert "submit_task: task is 'planned'; ack_task it first" in result["detail"]
    event = _single_mismatch_event(workspace, server, task_id, "submit_task", "planned", "submitted")
    assert event["payload"] == {
        "from_status": "planned",
        "to_status": "submitted",
        "action": "submit_task",
    }


def test_report_progress_planned_via_taskflow_is_structured(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """The report_progress ordering pre-check: structured rejection +
    SIG-MISMATCH.  Progress never changes the status, so to == from."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    workspace = tmp_path / "agent"
    task_id = "int5r2-progress-planned"
    _write_task(workspace, task_id, {"task_id": task_id, "status": "planned", "project_id": ""})
    args = _arguments(workspace, "report_progress", taskId=task_id, note="halfway")

    result = server._taskflow(args)

    assert result["ok"] is False
    assert result["error"] == "illegal_transition"
    assert result["from"] == "planned"
    assert result["to"] == "planned"
    assert "report_progress: task is 'planned'; ack_task it first" in result["detail"]
    event = _single_mismatch_event(workspace, server, task_id, "report_progress", "planned", "planned")
    assert event["payload"] == {
        "from_status": "planned",
        "to_status": "planned",
        "action": "report_progress",
    }


def test_ack_after_submit_retry_still_ok(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Retry semantics preserved: ack after submit is the idempotent no-op
    success path — no rejection, no event (the tightening only orders
    actions BEFORE their state; a late re-ack must not fail a finished job)."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    workspace = tmp_path / "agent"
    task_id = "int5r2-ack-submitted"
    _write_task(workspace, task_id, {"task_id": task_id, "status": "submitted", "project_id": ""})
    args = _arguments(workspace, "ack_task", taskId=task_id)

    result = server._taskflow(args)

    assert result["ok"] is True
    assert server._read_json(server._task_state_path(args, task_id))["status"] == "submitted"
    events_dir = workspace / "shared" / "events"
    assert not events_dir.exists() or not any(events_dir.rglob("*.json"))


def test_cancel_terminal_via_taskflow_is_structured(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """The cancel terminal guard (non-committed terminal state): structured
    rejection + SIG-MISMATCH.  The committed-cancellation retry path
    (cancelled + cancelled_at, matching reason) is untouched and stays ok."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    workspace = tmp_path / "agent"
    task_id = "int5r2-cancel-completed"
    _write_task(workspace, task_id, {"task_id": task_id, "status": "completed", "project_id": ""})
    args = _arguments(workspace, "cancel_task", taskId=task_id, reason="cleanup")

    result = server._taskflow(args)

    assert result["ok"] is False
    assert result["error"] == "illegal_transition"
    assert result["from"] == "completed"
    assert result["to"] == "cancelled"
    assert "terminal state completed" in result["detail"]
    event = _single_mismatch_event(workspace, server, task_id, "cancel_task", "completed", "cancelled")
    assert event["payload"] == {
        "from_status": "completed",
        "to_status": "cancelled",
        "action": "cancel_task",
    }


def test_delegate_terminal_via_taskflow_is_structured(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """The delegate guard on a terminal task meta: structured rejection
    (to = the assignment target 'assigned') + SIG-MISMATCH."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    workspace = tmp_path / "agent"
    task_id = "int5r2-delegate-completed"
    args = _arguments(
        workspace,
        "delegate_task",
        projectId="p1",
        taskId=task_id,
        roomId="!room:matrix.local",
        assignedTo="@worker:matrix.local",
    )
    _write_json(server._project_state_path(args, "p1"), {"project_id": "p1", "status": "active"})
    _write_task(workspace, task_id, {"task_id": task_id, "status": "completed", "project_id": "p1"})

    result = server._taskflow(args)

    assert result["ok"] is False
    assert result["error"] == "illegal_transition"
    assert result["from"] == "completed"
    assert result["to"] == "assigned"
    assert "terminal state completed" in result["detail"]
    event = _single_mismatch_event(workspace, server, task_id, "delegate_task", "completed", "assigned")
    assert event["payload"] == {
        "from_status": "completed",
        "to_status": "assigned",
        "action": "delegate_task",
    }


def test_distinct_rejections_keep_distinct_event_identities(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """P2 regression (review 10/23): different rejected actions on the same
    task state are distinct events (id_rule v1.2 rejection family).

    A rejection commits no state, so the task bytes — and therefore the ref
    fingerprint — are identical across ack/submit/report_progress on the
    same planned task. The rejection discriminator (action|from|to) is what
    keeps the three events apart; retrying one of them stays idempotent.
    """
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    workspace = tmp_path / "agent"
    task_id = "int5r2-distinct-rejections"
    _write_task(workspace, task_id, {"task_id": task_id, "status": "planned", "project_id": ""})
    meta_path = workspace / "shared" / "tasks" / task_id / "meta.json"

    cases = [
        ("ack_task", {}, ("ack_task", "planned", "in_progress")),
        (
            "submit_task",
            {"status": "SUCCESS", "summary": "done"},
            ("submit_task", "planned", "submitted"),
        ),
        ("report_progress", {"note": "halfway"}, ("report_progress", "planned", "planned")),
    ]
    for action, extra, _triple in cases:
        args = _arguments(workspace, action, taskId=task_id, **extra)
        result = server._taskflow(args)
        assert result["ok"] is False
        assert result["error"] == "illegal_transition"
        assert result["action"] == action

    # The first load normalises the task meta; from then on the bytes are
    # frozen — no rejection writes state.
    frozen = meta_path.read_bytes()
    assert json.loads(frozen.decode("utf-8"))["status"] == "planned"

    team = _team(server)
    fingerprint = hashlib.sha256(frozen).hexdigest()
    expected_ids = [
        server._signal_event_id(
            team,
            "",
            task_id,
            "",
            "SIG-MISMATCH",
            "mismatch_recorded",
            None,
            fingerprint,
            0,
            0,
            "|".join(triple),
        )
        for _action, _extra, triple in cases
    ]

    files = sorted((workspace / "shared" / "events" / team).glob("*.json"))
    assert [f.name for f in files] == sorted(f"{i}.json" for i in expected_ids)
    assert len(set(expected_ids)) == 3  # distinct rejections never share an id
    payloads = [json.loads(f.read_text(encoding="utf-8"))["payload"] for f in files]
    assert {p["action"] for p in payloads} == {"ack_task", "submit_task", "report_progress"}
    assert meta_path.read_bytes() == frozen

    # Retrying the first rejection re-lands on the same id: no fourth event.
    args = _arguments(workspace, "ack_task", taskId=task_id)
    assert server._taskflow(args)["error"] == "illegal_transition"
    files2 = sorted((workspace / "shared" / "events" / team).glob("*.json"))
    assert [f.name for f in files2] == [f.name for f in files]
    assert meta_path.read_bytes() == frozen
