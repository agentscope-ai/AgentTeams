"""S2 signal emission (INT-3, #1394 aligned) tests for TeamHarness server.py.

Covers the Python side of the shared contract:
- id_rule (v1.1): the cross-language fixed vector (Go's signal_events_test.go
  pins the same value) + distinct-project / distinct-iteration /
  pause-resume-pause / retry regressions
- list_events: types / taskId / since filters + cursor pagination
- record_loop_iteration: SIG-DECISION + conditional SIG-VERDICT
- submit_task / accept_task_result: SIG-COMPLETE
- idempotency: an emit-level retry of the same operation is one object on
  disk, while a later operation (advanced state bytes) gets a new object
"""

from __future__ import annotations

import base64
import hashlib
import importlib.util
import json
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest


REPO_ROOT = Path(__file__).resolve().parents[3]
MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"
# P0 compatibility copy shipped in the portable bundle (DEDUPE.md). It is a
# whole-tree copy of MCP_DIR and must stay behaviourally identical, not only
# byte-identical to the live tree at sync time.
BUNDLE_MCP_DIR = REPO_ROOT / "plugins" / "agentteams-teamharness" / "mcp"
CONTRACT_PATH = REPO_ROOT / "plugins" / "teamharness" / "contracts" / "signal-mapping.json"

# id_rule v1.2 fixed input: the post-write ref state bytes `{"project_id":
# "p1"}`, whose sha256 hex is the fingerprint field of the id input.
FIXED_REF_BYTES = b'{"project_id": "p1"}'
FIXED_FINGERPRINT = hashlib.sha256(FIXED_REF_BYTES).hexdigest()

# Same vector as signal_events_test.go (signalIDFixedVector): first 16 hex
# of sha256 of "alpha-team|p1|t1||SIG-DECISION|project_paused||" +
# FIXED_FINGERPRINT + "|0|1" (attempt=0, audit_seq=1). Both suites pin the
# exact same value for the exact same 10-tuple, so an id_rule drift on
# either side fails both.
FIXED_VECTOR = "ev_afb958c6b34a7132"

EVENT_FIELDS = {
    "event_id", "task_id", "loop_id", "event_type", "is_signal",
    "source_event", "ref", "fingerprint", "iteration_index", "payload",
    "occurred_at",
}


def _load_server() -> Any:
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_signal_test", MCP_DIR / "server.py"
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


def _team(server: Any) -> str:
    return str(server._runtime_team_name() or "").strip() or "global"


def _events_dir(server: Any, workspace: Path) -> Path:
    return workspace / "shared" / "events" / _team(server)


def _read_events(server: Any, workspace: Path) -> list[dict[str, Any]]:
    directory = _events_dir(server, workspace)
    if not directory.is_dir():
        return []
    return [json.loads(path.read_text(encoding="utf-8")) for path in sorted(directory.glob("*.json"))]


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


def _write_project(workspace: Path, project_id: str, meta: dict[str, Any]) -> None:
    _write_json(workspace / "shared" / "projects" / project_id / "meta.json", meta)


def _write_task(workspace: Path, task_id: str, meta: dict[str, Any]) -> None:
    _write_json(workspace / "shared" / "tasks" / task_id / "meta.json", meta)


def test_signal_id_fixed_vector_and_rule(server: Any) -> None:
    # Cross-language fixed vector (same 9-tuple and same value as the Go
    # signal_events_test.go pins).
    assert (
        server._signal_event_id("alpha-team", "p1", "t1", "", "SIG-DECISION", "project_paused", None, FIXED_FINGERPRINT, 0, 1)
        == FIXED_VECTOR
    )
    # attempt changes the id: a retry of a different attempt is a different event.
    assert (
        server._signal_event_id("alpha-team", "p1", "t1", "", "SIG-DECISION", "project_paused", None, FIXED_FINGERPRINT, 1, 1)
        != FIXED_VECTOR
    )
    # submit vs accept of the same task hash distinct source_events — no collision.
    assert server._signal_event_id("alpha-team", "p1", "task1", "", "SIG-COMPLETE", "task_submitted", None, FIXED_FINGERPRINT) != server._signal_event_id(
        "alpha-team", "p1", "task1", "", "SIG-COMPLETE", "task_accepted", None, FIXED_FINGERPRINT
    )

    # P1 regressions (id_rule v1.2): distinct logical events never share an
    # id; a retry of the same operation keeps its id.
    same_bytes = hashlib.sha256(b'{"status":"paused"}').hexdigest()
    # Two projects, same operation, even identical state bytes.
    assert server._signal_event_id("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", None, same_bytes) != (
        server._signal_event_id("alpha-team", "p2", "", "", "SIG-DECISION", "project_paused", None, same_bytes)
    )
    # Two iterations of the same loop (different post-write bytes).
    fp_i1 = hashlib.sha256(b'{"iteration": 1}').hexdigest()
    fp_i2 = hashlib.sha256(b'{"iteration": 2}').hexdigest()
    assert server._signal_event_id("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", 1, fp_i1) != (
        server._signal_event_id("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", 2, fp_i2)
    )
    # pause -> resume -> pause: three distinct events (pause #2 differs via
    # the committed audit_seq, resume via source_event).
    pause1 = server._signal_event_id("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", None, fp_i1, 0, 1)
    resume = server._signal_event_id("alpha-team", "p1", "", "", "SIG-DECISION", "project_resumed", None, fp_i2, 0, 2)
    # Same second, byte-identical post-write state; only audit_seq separates it.
    pause2 = server._signal_event_id("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", None, fp_i1, 0, 3)
    assert len({pause1, resume, pause2}) == 3
    # Retry of the same operation: same identity fields + same state bytes
    # + same audit_seq → same id (consumer dedupe relies on this).
    assert server._signal_event_id("alpha-team", "p1", "", "", "SIG-DECISION", "project_paused", None, fp_i1, 0, 1) == pause1


def test_contract_matches_py_emitters(server: Any) -> None:
    contract = json.loads(CONTRACT_PATH.read_text(encoding="utf-8"))
    assert len(contract["events"]) == 14
    levels = set(contract["levels"])
    by_source_event: dict[str, dict[str, Any]] = {}
    for row in contract["events"]:
        for source_event in row["source_events"]:
            by_source_event[source_event] = row

    for emitter, (source_event, event_type, _origin) in server._SIGNAL_EMITTERS.items():
        row = by_source_event.get(source_event)
        assert row is not None, f"contract row for source_event {source_event} missing"
        assert f"py:{emitter}" in row["emitters"], f"{emitter}: py:{emitter} not in contract row emitters {row['emitters']}"
        assert row["event_type"] == event_type
        assert row["level"] in levels


def test_emit_idempotent_local(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    fake = FakeFilesync()
    monkeypatch.setattr(server, "_filesync", fake)
    workspace = tmp_path / "agent"
    args = _arguments(workspace, "noop")

    first = server._emit_signal_event(
        args, "record_loop_iteration", task_id="", project_id="p1", iteration_index=1,
        ref="shared/projects/p1/meta.json", ref_bytes=FIXED_REF_BYTES,
        payload={"project_id": "p1"},
    )
    assert first is True
    # Emit-level retry of the same operation: same post-write bytes (→ same
    # fingerprint) with a changed payload — must be a no-op, original kept.
    second = server._emit_signal_event(
        args, "record_loop_iteration", task_id="", project_id="p1", iteration_index=1,
        ref="shared/projects/p1/meta.json", ref_bytes=FIXED_REF_BYTES,
        payload={"project_id": "p1", "note": "retry"},
    )
    assert second is False

    # A later iteration of the same loop (the stored state advanced):
    # different post-write bytes → a different event object, and the first
    # object stays untouched.
    state2 = b'{"project_id": "p1", "current_iteration": 2}'
    third = server._emit_signal_event(
        args, "record_loop_iteration", task_id="", project_id="p1", iteration_index=2,
        ref="shared/projects/p1/meta.json", ref_bytes=state2,
        payload={"project_id": "p1", "iteration": 2},
    )
    assert third is True

    files = sorted(_events_dir(server, workspace).glob("*.json"))
    assert len(files) == 2
    events = [json.loads(f.read_text(encoding="utf-8")) for f in files]
    by_iter = {e["iteration_index"]: e for e in events}
    data = by_iter[1]
    assert set(data) == EVENT_FIELDS  # exactly the 11 schema fields
    assert data["event_id"] in {f.stem for f in files}
    assert data["fingerprint"] == FIXED_FINGERPRINT
    assert data["payload"] == {"project_id": "p1"}  # original kept, retry dropped
    data2 = by_iter[2]
    assert data2["event_id"] in {f.stem for f in files}
    assert data2["iteration_index"] == 2
    assert data2["fingerprint"] == hashlib.sha256(state2).hexdigest()
    # One mirror push per written object (first and third emit only).
    assert [c for c in fake.calls if c.get("action") == "push"] == [
        {
            "workspaceDir": str(workspace),
            "action": "push",
            "payload": {},
            "path": f"shared/events/{_team(server)}/{data['event_id']}.json",
        },
        {
            "workspaceDir": str(workspace),
            "action": "push",
            "payload": {},
            "path": f"shared/events/{_team(server)}/{data2['event_id']}.json",
        },
    ]


def test_emit_two_projects_distinct_keys(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    """P1 regression: two projects' pause of the same shape must not share
    one event key (the original cross-language collision)."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    workspace = tmp_path / "agent"
    args = _arguments(workspace, "noop")
    # Identical state bytes on both sides — project identity alone must
    # keep the keys apart.
    state = b'{"status": "paused"}'
    for project_id in ("p1", "p2"):
        emitted = server._emit_signal_event(
            args, "record_loop_iteration", task_id="", project_id=project_id, iteration_index=None,
            ref=f"shared/projects/{project_id}/meta.json", ref_bytes=state,
            payload={"project_id": project_id},
        )
        assert emitted is True
    files = sorted(_events_dir(server, workspace).glob("*.json"))
    assert len(files) == 2
    by_project = {json.loads(p.read_text(encoding="utf-8"))["payload"]["project_id"]: p.stem for p in files}
    assert set(by_project) == {"p1", "p2"}
    assert len(set(by_project.values())) == 2


def test_record_loop_iteration_two_rounds_distinct_events(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """P1 regression: iteration 2 of a loop must not be deduped as a repeat
    of iteration 1 — the real _projectflow path writes the loop state on
    every call, so the post-write bytes (and thus the ids) differ."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setattr(server, "_sync_project", lambda *a, **k: True)
    loop = {"goal": "g", "stop_condition": "s", "max_iterations": 3, "current_iteration": 0,
            "status": "running", "tasks": [], "history": []}
    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {"project_id": "p1", "title": "P1", "status": "active",
                                      "plan_type": "loop", "loop": loop})

    for iteration, decision in ((1, "continue"), (2, "continue")):
        res = server._projectflow(_arguments(workspace, "record_loop_iteration", projectId="p1",
                                             iteration=iteration, decision=decision,
                                             summary=f"s{iteration}"))
        assert res["ok"] is True

    evs = _read_events(server, workspace)
    assert len(evs) == 2
    assert sorted(e["iteration_index"] for e in evs) == [1, 2]
    assert len({e["event_id"] for e in evs}) == 2
    assert all(e["task_id"] == "" for e in evs)
    assert all(e["payload"]["project_id"] == "p1" for e in evs)


def test_list_events_filters_and_pagination(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    workspace = tmp_path / "agent"
    team = _team(server)
    directory = _events_dir(server, workspace)
    directory.mkdir(parents=True, exist_ok=True)

    def seed(event_id: str, event_type: str, task_id: str, occurred_at: str, payload: dict[str, Any]) -> None:
        (directory / f"{event_id}.json").write_text(json.dumps({
            "event_id": event_id,
            "task_id": task_id,
            "loop_id": None,
            "event_type": event_type,
            "is_signal": True,
            "source_event": {"origin": "submit", "request_id": None},
            "ref": "shared/projects/p1/meta.json",
            "fingerprint": "0" * 64,
            "iteration_index": None,
            "payload": payload,
            "occurred_at": occurred_at,
        }), encoding="utf-8")

    seed("ev_01", "SIG-DECISION", "t1", "2026-10-01T00:00:01Z", {"n": 1})
    seed("ev_02", "SIG-DECISION", "t2", "2026-10-01T00:00:02Z", {"n": 2})
    seed("ev_03", "SIG-DECISION", "t1", "2026-10-01T00:00:03Z", {"n": 3})
    seed("ev_04", "SIG-COMPLETE", "t1", "2026-10-01T00:00:04Z", {"n": 4})
    seed("ev_05", "SIG-VERDICT", "", "2026-10-01T00:00:05Z", {"n": 5})

    def query(**payload: Any) -> dict[str, Any]:
        return server._projectflow(_arguments(workspace, "list_events", team=team, **payload))

    # No filters: all 5, in (occurred_at, event_id) order.
    res = query()
    assert res["ok"] is True
    assert [e["event_id"] for e in res["events"]] == ["ev_01", "ev_02", "ev_03", "ev_04", "ev_05"]
    assert res["count"] == 5 and res["next_cursor"] is None

    # types filter.
    assert [e["event_id"] for e in query(types=["SIG-DECISION"])["events"]] == ["ev_01", "ev_02", "ev_03"]
    assert [e["event_id"] for e in query(types=["SIG-COMPLETE", "SIG-VERDICT"])["events"]] == ["ev_04", "ev_05"]

    # taskId filter (task_id match).
    assert [e["event_id"] for e in query(taskId="t1")["events"]] == ["ev_01", "ev_03", "ev_04"]

    # since filter (inclusive).
    assert [e["event_id"] for e in query(since="2026-10-01T00:00:03Z")["events"]] == ["ev_03", "ev_04", "ev_05"]

    # Pagination: limit=2, then follow the cursor.
    page1 = query(limit=2)
    assert [e["event_id"] for e in page1["events"]] == ["ev_01", "ev_02"]
    assert page1["next_cursor"] is not None
    page2 = query(limit=2, cursor=page1["next_cursor"])
    assert [e["event_id"] for e in page2["events"]] == ["ev_03", "ev_04"]
    page3 = query(limit=2, cursor=page2["next_cursor"])
    assert [e["event_id"] for e in page3["events"]] == ["ev_05"]
    assert page3["next_cursor"] is None

    # A corrupt cursor is rejected, not guessed at.
    bad = query(cursor="not-a-valid-cursor")
    assert bad["ok"] is False
    assert "cursor" in bad["error"]


def test_list_events_cursor_accepts_both_base64_forms(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    # #1433 round 2: the cursor is opaque across the two read surfaces —
    # the Go endpoint emits unpadded base64url (RawURLEncoding) while this
    # side emits padded (urlsafe_b64encode); a page must resume from the
    # same anchor under BOTH forms.
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    workspace = tmp_path / "agent"
    team = _team(server)
    directory = _events_dir(server, workspace)
    directory.mkdir(parents=True, exist_ok=True)

    def seed(event_id: str, occurred_at: str) -> None:
        (directory / f"{event_id}.json").write_text(json.dumps({
            "event_id": event_id,
            "task_id": "",
            "loop_id": None,
            "event_type": "SIG-DECISION",
            "is_signal": True,
            "source_event": {"origin": "submit", "request_id": None},
            "ref": "shared/projects/p1/meta.json",
            "fingerprint": "0" * 64,
            "iteration_index": None,
            "payload": {},
            "occurred_at": occurred_at,
        }), encoding="utf-8")

    seed("ev_01", "2026-10-01T00:00:01Z")
    seed("ev_02", "2026-10-01T00:00:02Z")
    seed("ev_03", "2026-10-01T00:00:03Z")

    # The exact bytes Go's encodeSignalCursor builds for this anchor.
    anchor = "2026-10-01T00:00:02Z\x00ev_02"
    encoded = base64.urlsafe_b64encode(anchor.encode("utf-8")).decode("ascii")
    padded = encoded  # this side's (Python) form
    unpadded = encoded.rstrip("=")  # Go's form (RawURLEncoding)
    assert len(anchor) % 4 != 0, "vector must actually differ between the two forms"

    def query(**payload: Any) -> dict[str, Any]:
        return server._projectflow(_arguments(workspace, "list_events", team=team, **payload))

    # Padded form (existing behavior must not regress).
    res_padded = query(cursor=padded)
    assert res_padded["ok"] is True
    assert [e["event_id"] for e in res_padded["events"]] == ["ev_03"]

    # Unpadded form (Go-shaped) resumes from the same anchor.
    res_unpadded = query(cursor=unpadded)
    assert res_unpadded["ok"] is True
    assert res_unpadded == res_padded

    # Invalid cursors are still rejected, not guessed at.
    no_separator = base64.urlsafe_b64encode(b"2026-10-01T00:00:02Zev_02").decode("ascii")
    for bad in ("not-a-valid-cursor", no_separator):
        res = query(cursor=bad)
        assert res["ok"] is False
        assert "cursor" in res["error"]


def test_record_loop_iteration_emits(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setattr(server, "_sync_project", lambda *a, **k: True)
    loop = {"goal": "g", "stop_condition": "s", "max_iterations": 3, "current_iteration": 0,
            "status": "running", "tasks": [], "history": []}

    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {"project_id": "p1", "title": "P1", "status": "active", "plan_type": "loop", "loop": loop})
    res = server._projectflow(_arguments(workspace, "record_loop_iteration", projectId="p1", iteration=1,
                                         decision="continue", summary="s1"))
    assert res["ok"] is True
    evs = _read_events(server, workspace)
    assert len(evs) == 1
    ev = evs[0]
    assert ev["event_type"] == "SIG-DECISION"
    assert ev["task_id"] == ""  # project-level event
    assert ev["loop_id"] is None  # main baseline has no loop_id field
    assert ev["iteration_index"] == 1
    assert ev["source_event"] == {"origin": "decision_lock", "request_id": None}
    assert ev["payload"]["decision"] == "continue"
    assert ev["payload"]["iteration"] == 1

    # Conditional SIG-VERDICT: a verdict dict (or evidenceRef) rides a second event.
    workspace2 = tmp_path / "agent2"
    _write_project(workspace2, "p2", {"project_id": "p2", "title": "P2", "status": "active", "plan_type": "loop", "loop": loop})
    res2 = server._projectflow(_arguments(workspace2, "record_loop_iteration", projectId="p2", iteration=1,
                                          decision="stop_success", summary="done",
                                          verdict={"score": 9}, evidenceRef="shared/projects/p2/eval.md"))
    assert res2["ok"] is True
    evs2 = _read_events(server, workspace2)
    # Order-agnostic: the two event objects are separate files.
    assert sorted(e["event_type"] for e in evs2) == ["SIG-DECISION", "SIG-VERDICT"]
    verdict = next(e for e in evs2 if e["event_type"] == "SIG-VERDICT")
    assert verdict["payload"]["verdict"] == {"score": 9}
    assert verdict["payload"]["evidence_ref"] == "shared/projects/p2/eval.md"
    assert verdict["source_event"]["origin"] == "critic_verdict"


def _write_project_and_task(workspace: Path, server: Any, *, task_status: str = "in_progress") -> None:
    project = {
        "project_id": "p1",
        "title": "P1",
        "status": "active",
        "tasks": [{"task_id": "t1", "title": "T1", "assigned_to": "@worker:example.test",
                   "depends_on": [], "status": task_status}],
        "requester_report": {"pending": False, "sent_at": "2026-08-13T08:00:00Z"},
    }
    task = {"task_id": "t1", "project_id": "p1", "room_id": "!task:example.test", "status": task_status}
    server._write_json(workspace / "shared" / "projects" / "p1" / "meta.json", project)
    server._write_json(workspace / "shared" / "tasks" / "t1" / "meta.json", task)


def test_accept_submit_emits_complete(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    workspace = tmp_path / "agent"
    fake = FakeFilesync()
    monkeypatch.setattr(server, "_filesync", fake)
    monkeypatch.setattr(server, "_pull_project", lambda *a, **k: True)
    monkeypatch.setattr(server, "_publish_task_artifacts", lambda *a, **k: [])
    monkeypatch.setattr(server, "_publish_project_artifacts", lambda *a, **k: [])
    monkeypatch.setattr(server, "_sync_task", lambda *a, **k: True)
    monkeypatch.setattr(server, "_sync_project", lambda *a, **k: True)
    _write_project_and_task(workspace, server)

    submit = server._taskflow({
        "role": "worker",
        "workspaceDir": str(workspace),
        "action": "submit_task",
        "payload": {
            "taskId": "t1",
            "status": "SUCCESS",
            "summary": "done",
            "deliverables": ["shared/tasks/t1/result.md"],
        },
    })
    assert submit["ok"] is True
    team = _team(server)
    assert [e["event_type"] for e in _read_events(server, workspace)] == ["SIG-COMPLETE"]
    assert submit["task"]["submission_id"]

    accept = server._projectflow({
        "role": "leader",
        "workspaceDir": str(workspace),
        "action": "accept_task_result",
        "payload": {
            "projectId": "p1",
            "taskId": "t1",
            "submissionId": submit["task"]["submission_id"],
            "resultStatus": "SUCCESS",
            "summary": "done",
        },
    })
    assert accept["ok"] is True
    evs = _read_events(server, workspace)
    # Two SIG-COMPLETE events (submit + accept) with distinct deterministic ids.
    assert [e["event_type"] for e in evs] == ["SIG-COMPLETE", "SIG-COMPLETE"]
    assert len({e["event_id"] for e in evs}) == 2
    assert all(e["task_id"] == "t1" for e in evs)
    # The mirror pushes targeted the event objects, not the task/project.
    pushes = [c for c in fake.calls if c.get("action") == "push"]
    assert len(pushes) == 2
    assert all(p["path"].startswith(f"shared/events/{team}/") for p in pushes)


def _public_taskflow(server: Any, arguments: dict[str, Any]) -> dict[str, Any]:
    """Call through the public call_tool path (MCP envelope)."""
    result = server.call_tool("taskflow", arguments)
    block = (result.get("content") or [{}])[0]
    return json.loads(block.get("text") or "{}")


def _submit_payload() -> dict[str, Any]:
    return {
        "taskId": "t1",
        "status": "SUCCESS",
        "summary": "done",
        "deliverables": ["shared/tasks/t1/result.md"],
    }


def _submit_args(workspace: Path) -> dict[str, Any]:
    return {
        "role": "worker",
        "workspaceDir": str(workspace),
        "action": "submit_task",
        "payload": _submit_payload(),
    }


def _prepare_submit_env(server: Any, monkeypatch: Any, workspace: Path) -> None:
    fake = FakeFilesync()
    monkeypatch.setattr(server, "_filesync", fake)
    monkeypatch.setattr(server, "_pull_project", lambda *a, **k: True)
    monkeypatch.setattr(server, "_publish_task_artifacts", lambda *a, **k: [])
    monkeypatch.setattr(server, "_publish_project_artifacts", lambda *a, **k: [])
    monkeypatch.setattr(server, "_sync_task", lambda *a, **k: True)
    monkeypatch.setattr(server, "_sync_project", lambda *a, **k: True)


def test_submit_retry_after_nonzero_project_audit_seq_is_one_event(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    workspace = tmp_path / "agent"
    _prepare_submit_env(server, monkeypatch, workspace)
    _write_project_and_task(workspace, server)
    project_path = workspace / "shared/projects/p1/meta.json"
    project = json.loads(project_path.read_text(encoding="utf-8"))
    project["audit_seq"] = 3  # committed revision already nonzero
    _write_json(project_path, project)

    first = _public_taskflow(server, _submit_args(workspace))
    assert first["ok"] is True and first.get("reused") is not True

    # Capture what the first submit committed, before the retry: the raw
    # task meta bytes (the input the event fingerprint hashes) and the raw
    # bytes of the single event object on disk.
    task_meta_path = workspace / "shared" / "tasks" / "t1" / "meta.json"
    task_meta_bytes = task_meta_path.read_bytes()
    event_files = sorted(_events_dir(server, workspace).glob("*.json"))
    assert len(event_files) == 1, [f.name for f in event_files]
    event_bytes = event_files[0].read_bytes()

    second = _public_taskflow(server, _submit_args(workspace))
    assert second["ok"] is True and second["reused"] is True
    assert second["task"]["submission_id"] == first["task"]["submission_id"]

    events = _read_events(server, workspace)
    assert len(events) == 1, [e["event_id"] for e in events]
    assert events[0]["event_type"] == "SIG-COMPLETE"
    # The fingerprint is the sha256 of the committed task meta bytes.
    assert events[0]["fingerprint"] == hashlib.sha256(task_meta_bytes).hexdigest()
    # result_digest hashes the canonical task-result payload instead — a
    # different input, so it can never equal the event fingerprint.
    assert second["task"].get("result_digest")
    assert events[0]["fingerprint"] != second["task"].get("result_digest")
    # The retry changed nothing on disk: same task meta bytes, and the
    # single SIG-COMPLETE event object byte-identical.
    assert task_meta_path.read_bytes() == task_meta_bytes
    assert [p.name for p in sorted(_events_dir(server, workspace).glob("*.json"))] == [event_files[0].name]
    assert event_files[0].read_bytes() == event_bytes


def test_submit_retry_after_project_revision_advances_is_one_event(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    workspace = tmp_path / "agent"
    _prepare_submit_env(server, monkeypatch, workspace)
    _write_project_and_task(workspace, server)

    first = _public_taskflow(server, _submit_args(workspace))
    assert first["ok"] is True
    project_path = workspace / "shared/projects/p1/meta.json"
    project = json.loads(project_path.read_text(encoding="utf-8"))
    project["audit_seq"] = 2  # another intervention advances the project
    _write_json(project_path, project)
    second = _public_taskflow(server, _submit_args(workspace))
    assert second["ok"] is True and second["reused"] is True

    events = _read_events(server, workspace)
    assert len(events) == 1, [e["event_id"] for e in events]
    assert events[0]["event_type"] == "SIG-COMPLETE"


def test_go_cursor_is_accepted_through_both_installed_entries(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """The shipped bundle entry must decode a Go-produced cursor too (#1433).

    ``plugins/agentteams-teamharness/mcp`` is a P0 compatibility copy
    (``DEDUPE.md``). The drift test proves byte equality at sync time; this
    test proves the *behaviour* through the bundle's own entry point, so a
    future change that touches only the live tree is caught even before a
    re-sync (and a stale copy that still rejects Go cursors fails here).
    The bundle runs in a fresh process: ``mcp.*`` submodules are cached in
    ``sys.modules``, so an in-process second import would silently reuse the
    live tree.
    """
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    workspace = tmp_path / "agent"
    team = _team(server)
    directory = _events_dir(server, workspace)
    directory.mkdir(parents=True, exist_ok=True)
    for event_id, occurred_at in (
        ("ev_01", "2026-10-01T00:00:01Z"),
        ("ev_02", "2026-10-01T00:00:02Z"),
        ("ev_03", "2026-10-01T00:00:03Z"),
    ):
        (directory / f"{event_id}.json").write_text(json.dumps({
            "event_id": event_id,
            "task_id": "",
            "loop_id": None,
            "event_type": "SIG-DECISION",
            "is_signal": True,
            "source_event": {"origin": "submit", "request_id": None},
            "ref": "shared/projects/p1/meta.json",
            "fingerprint": "0" * 64,
            "iteration_index": None,
            "payload": {},
            "occurred_at": occurred_at,
        }), encoding="utf-8")

    # Exactly what Go's encodeSignalCursor emits: unpadded base64url.
    anchor = "2026-10-01T00:00:02Z\x00ev_02"
    unpadded = base64.urlsafe_b64encode(anchor.encode("utf-8")).decode("ascii").rstrip("=")

    live_args = _arguments(workspace, "list_events", team=team, cursor=unpadded)
    live_result = server._projectflow(live_args)
    assert live_result["ok"] is True

    script = f"""
import importlib.util, json, sys
mcp_dir = {json.dumps(str(BUNDLE_MCP_DIR))}
sys.path.insert(0, mcp_dir)
spec = importlib.util.spec_from_file_location("bundle_server_under_test", mcp_dir + "/server.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
args = {json.dumps(live_args)}
print(json.dumps(module._projectflow(args)))
"""
    proc = subprocess.run(
        [sys.executable, "-c", script],
        capture_output=True,
        text=True,
        timeout=180,
    )
    assert proc.returncode == 0, proc.stderr
    bundle_result = json.loads(proc.stdout)
    assert bundle_result.get("ok") is True, bundle_result
    # Both entries resume from the same Go cursor with the same page.
    assert json.dumps(bundle_result, sort_keys=True) == json.dumps(live_result, sort_keys=True)
