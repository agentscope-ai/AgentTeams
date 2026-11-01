"""W2 protocol guards (issue #1451 W2): decision class, decision_status,
the attention stall chain, and the review gate.

Ten-row matrix from the W2 task spec, plus the compatibility checks
around it:

  1. enforce off, delegate without decision_class -> success (field not written)
  2. enforce on,  delegate without decision_class -> missing_decision_class
  3. enforce on,  delegate with r4               -> success, persisted
  4. r1 task, no resolved approval record        -> accept -> approval_required
  5. r1 task, with a resolved approval record    -> accept -> success
  6. reviewer == submitter (same account)        -> self_review_forbidden
  7. review verdict fail                         -> accept -> review_not_passed
  8. stalls 0/1/2/3                              -> open/open/blocked/frozen
  9. frozen unfreeze without approval            -> refused, approval unfreezes
 10. contracts vs constants drift                -> ids, thresholds, values

Rows 1-3 also assert the off-switch byte identity: with the switch off no
protocol field (decision_class / decision_status / stall_count /
submitted_by) is written at all.

Round 3/4 (issue #1467) — approval authority: closing a kind=approval
record is an authorized human decision, not a mere loop close. The
authority rule (protocol-enforced only; round 4: trusted authority
only, fail closed):

  only the trusted runtime role can authorize — "human" authorizes,
  anything else (worker / leader / no role) fails closed;
  every field a tool caller controls (role, account, channel, decider,
  evidence) is provenance, never authority: the payload's human claim
  (role=human + a human channel + resolvedBy + resolveEvidence) is
  denied without the trusted role. A human integration (dashboard /
  API bridge) must run with AGENTTEAMS_AGENT_ROLE=human (or
  member.role in the runtime config).

The authorized close writes the server-derived marker
(resolvedRole="human" + resolvedActor="human:<account>") that the r1
approval gate reads: the gate requires the marker (never inferred from
the decider strings), the human channel, and a decider account that
differs from the task's assignee (self-approval never counts; record
presence alone is not an approval).

Round 5 (issue #1467) — frozen is absorbing: the only exit from
frozen is the authorized approval close (the kind=approval, trusted
human path — the single _set_decision_status call carrying
unfreeze_authority='approval'). Every other writer — a stall
re-escalation on a second record, a non-approval resolve, a tool
close — is refused at the single decision_status writer and audited
as a refused history entry (from='frozen', to='frozen',
refused=<attempted status>). The regressions: the maintainer's exact
chain, the per-kind absorbing matrix, and the choke-point refusal
audit.

Round 8 (issue #1467) — the trusted decider: the r1 approval gate
reads the decider from the server-written resolvedActor marker ONLY
(resolvedBy is display provenance and never decides; a record without
the marker is not an approval — fail closed), and the
self-approval comparison canonicalizes one way on both sides (the
shared _canonical_account: the MXID localpart and the runtimeName of
the same member are the same identity). The regressions: the
maintainer's display-field pair (still approval_required when only
resolvedBy changes), a genuine different human (accepted and
persisted), the MXID/runtimeName self-approval pair in both
spellings, and the marker-less fail-closed record.
"""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[2] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402

from mcp.common import constants  # noqa: E402


def _load_server() -> Any:
    """This file's own copy of the server (suite convention, see
    test_mcp_workspace / test_pull_project): refs name resolution is
    process-global and every harness that execs server.py rebinds it at
    exec time — so this file's monkeypatches must land on the copy the
    proxies resolve against, i.e. the copy this file loads itself."""
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_protocol_guard_test",
        MCP_DIR / "server.py",
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(MCP_DIR))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(MCP_DIR))
    return module


_own_copy_loaded = False


@pytest.fixture(autouse=True)
def bound_server() -> None:
    """Drive this file's own server copy (loaded once; no other test file
    runs between the tests of this file, so the binding holds for all of
    them)."""
    global server, _own_copy_loaded
    if not _own_copy_loaded:
        server = _load_server()
        _own_copy_loaded = True


WORKER_A = "@alice:example.test"
ROOM = "!taskroom:example.test"

PROJECT_ID = "w2-project"
TASK_ID = "w2-task-01"

SUBMISSION_ID = "sub-w2-0001"


def _call(tool: str, arguments: dict[str, Any]) -> dict[str, Any]:
    response = server.call_tool(tool, arguments)
    return json.loads(response["content"][0]["text"])


def _enforce_on(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TEAMHARNESS_PROTOCOL_ENFORCE", "1")


def _write_project(
    workspace: Path,
    *,
    task_status: str = "planned",
    project_id: str = PROJECT_ID,
    task_id: str = TASK_ID,
    assignee: str = WORKER_A,
) -> dict[str, Any]:
    project = {
        "project_id": project_id,
        "title": "W2 protocol project",
        "status": "active",
        "tasks": [
            {
                "task_id": task_id,
                "title": "W2 task",
                "assigned_to": assignee,
                "depends_on": [],
                "status": task_status,
            }
        ],
        "requester_report": {"pending": False, "sent_at": "2026-10-29T00:00:00Z"},
    }
    server._write_json(workspace / "shared" / "projects" / project_id / "meta.json", project)
    return project


def _write_task_meta(workspace: Path, task: dict[str, Any]) -> None:
    server._write_json(
        workspace / "shared" / "tasks" / task["task_id"] / "meta.json", task
    )


def _task_meta(workspace: Path, task_id: str = TASK_ID) -> dict[str, Any]:
    return server._read_json(workspace / "shared" / "tasks" / task_id / "meta.json")


def _write_submitted_task(
    workspace: Path,
    *,
    task_id: str = TASK_ID,
    project_id: str = PROJECT_ID,
    **extra: Any,
) -> dict[str, Any]:
    """A submitted task meta in the exact shape submit_task persists
    (result fence + submission identity); extra fields are layered on top."""
    _write_project(workspace, task_status="submitted", task_id=task_id, project_id=project_id)
    task: dict[str, Any] = {
        "task_id": task_id,
        "project_id": project_id,
        "status": "submitted",
        "result_status": "SUCCESS",
        "summary": "done",
        "deliverables": [],
        "submission_id": SUBMISSION_ID,
        "submitted_at": "2026-10-29T00:00:00Z",
        "continuation": {"status": "pending", "delivery_id": "dl-w2-0001"},
    }
    task.update(extra)
    _write_task_meta(workspace, task)
    return task


def _delegate(
    workspace: Path,
    *,
    task_id: str = TASK_ID,
    project_id: str = PROJECT_ID,
    assigned_to: str = WORKER_A,
    decision_class: str | None = None,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "projectId": project_id,
        "taskId": task_id,
        "roomId": ROOM,
        "assignedTo": assigned_to,
        "title": "W2 task",
        "spec": "Do the thing.",
    }
    if decision_class is not None:
        payload["decisionClass"] = decision_class
    return _call("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "workspaceDir": str(workspace),
        "payload": payload,
    })


def _submit(
    workspace: Path,
    *,
    role: str = "worker",
    account: str | None = None,
    task_id: str = TASK_ID,
    project_id: str = PROJECT_ID,
) -> dict[str, Any]:
    arguments: dict[str, Any] = {
        "role": role,
        "action": "submit_task",
        "workspaceDir": str(workspace),
        "payload": {
            "projectId": project_id,
            "taskId": task_id,
            "status": "SUCCESS",
            "summary": "done",
            "deliverables": [],
        },
    }
    if account is not None:
        arguments["account_id"] = account
    return _call("taskflow", arguments)


def _review(
    workspace: Path,
    *,
    role: str = "critic",
    account: str | None = None,
    verdict: str = "pass",
    task_id: str = TASK_ID,
) -> dict[str, Any]:
    arguments: dict[str, Any] = {
        "role": role,
        "action": "review_task",
        "workspaceDir": str(workspace),
        "payload": {
            "taskId": task_id,
            "verdict": verdict,
            "note": "looks right",
        },
    }
    if account is not None:
        arguments["account_id"] = account
    return _call("taskflow", arguments)


def _accept(
    workspace: Path,
    *,
    task_id: str = TASK_ID,
    project_id: str = PROJECT_ID,
    submission_id: str = SUBMISSION_ID,
) -> dict[str, Any]:
    return _call("projectflow", {
        "role": "leader",
        "action": "accept_task_result",
        "workspaceDir": str(workspace),
        "payload": {
            "projectId": project_id,
            "taskId": task_id,
            "submissionId": submission_id,
            "resultStatus": "SUCCESS",
            "accepted": True,
        },
    })


@pytest.fixture(autouse=True)
def clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """The role must come from the call arguments, not the runtime env,
    and the protocol switch from the test itself (default off)."""
    for var in (
        "TEAMHARNESS_PROTOCOL_ENFORCE",
        "TEAMHARNESS_RUNTIME_CONFIG",
        "AGENTTEAMS_AGENT_ROLE",
        "AGENTTEAMS_WORKER_ROLE",
        "AGENTTEAMS_WORKER_NAME",
    ):
        monkeypatch.delenv(var, raising=False)


@pytest.fixture
def storage(monkeypatch: pytest.MonkeyPatch) -> None:
    """Shared storage + signal side effects stubbed out: the tests are
    about the protocol layer, not the transport."""
    monkeypatch.setattr(server, "_pull_project", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_pull_task", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_sync_task", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_sync_project", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_publish_task_artifacts", lambda *_a, **_k: [])
    monkeypatch.setattr(server.taskflow, "_emit_signal_event", lambda *_a, **_k: True)
    monkeypatch.setattr(server.flow, "_emit_signal_event", lambda *_a, **_k: True)


@pytest.fixture
def shared_remote(monkeypatch: pytest.MonkeyPatch, storage: None) -> dict[str, Any]:
    """Two-plane shared-storage model for round-9 regressions: a hidden
    'remote' meta (what shared storage actually holds) plus the local
    meta.json the action reads. _pull_task copies remote -> local
    (mc cp mirror semantics); _sync_task captures local -> remote
    (mc mirror of the whole directory), so a write on a stale local copy
    is observable in the committed bytes."""
    planes: dict[str, Any] = {"remote": None, "committed": None, "pull_ok": True}

    def fake_pull(_arguments: Any, task_id: Any, **_kw: Any) -> bool:
        if not planes["pull_ok"]:
            return False
        if planes["remote"] is not None:
            server._write_json(server._task_state_path(_arguments, str(task_id)), planes["remote"])
        return True

    def fake_sync(_arguments: Any, task_id: Any, **kw: Any) -> bool:
        path = server._task_state_path(_arguments, str(task_id))
        planes["committed"] = server._read_json(path)
        return True

    monkeypatch.setattr(server, "_pull_task", fake_pull)
    monkeypatch.setattr(server, "_sync_task", fake_sync)
    return planes


@pytest.fixture
def delegate_env(monkeypatch: pytest.MonkeyPatch, storage: None) -> dict[str, list[Any]]:
    calls: dict[str, list[Any]] = {"notify": []}

    def notify(*_args: Any, **kwargs: Any) -> dict[str, Any]:
        calls["notify"].append(kwargs)
        return {
            "sent": True,
            "eventId": f"$delegate-{len(calls['notify']) + 1}",
            "roomId": kwargs.get("room_id", ""),
            "assignee": kwargs.get("assignee", ""),
        }

    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.test")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    monkeypatch.setattr(server, "_send_delegate_notification", notify)
    monkeypatch.setattr(
        server, "_validate_assignee_membership", lambda *_a, **_k: {"ok": True}
    )
    return calls


@pytest.fixture
def attention_env(monkeypatch: pytest.MonkeyPatch, storage: None) -> None:
    monkeypatch.setattr(
        server.taskflow,
        "_send_attention_notification",
        lambda *_a, **_k: {
            "sent": True,
            "eventId": f"$att-{_k.get('kind')}-{_k.get('attempt')}",
        },
    )
    monkeypatch.setattr(server.taskflow, "_team_leader_matrix_id", lambda: "@leader:example.test")
    monkeypatch.setattr(server.taskflow, "_team_human_matrix_ids", lambda: [])
    monkeypatch.setattr(
        server, "_validate_assignee_membership", lambda *_a, **_k: {"ok": True}
    )


def _request_attention(
    workspace: Path,
    *,
    kind: str = "decision",
    question: str = "Need a decision.",
    resolved: bool | None = None,
    role: str = "worker",
    account: str | None = None,
    resolved_via: str | None = None,
    resolved_by: str | None = None,
    resolve_evidence: dict[str, Any] | None = None,
) -> dict[str, Any]:
    payload: dict[str, Any] = {"taskId": TASK_ID, "kind": kind, "question": question}
    if resolved is not None:
        payload["resolved"] = resolved
    if resolved_via is not None:
        payload["resolvedVia"] = resolved_via
    if resolved_by is not None:
        payload["resolvedBy"] = resolved_by
    if resolve_evidence is not None:
        payload["resolveEvidence"] = resolve_evidence
    arguments: dict[str, Any] = {
        "role": role,
        "action": "request_attention",
        "workspaceDir": str(workspace),
        "payload": payload,
    }
    if account is not None:
        arguments["account_id"] = account
    return _call("taskflow", arguments)


def _write_in_progress_task(workspace: Path) -> None:
    _write_project(workspace, task_status="in_progress")
    _write_task_meta(workspace, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "in_progress",
        "room_id": ROOM,
        "assigned_to": WORKER_A,
    })


# --- Row 1: enforce off, delegate without class -> success ---------------


def test_row1_enforce_off_delegate_without_class_succeeds(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, delegate_env: dict[str, list[Any]]
) -> None:
    _write_project(tmp_path)

    result = _delegate(tmp_path)

    assert result["ok"] is True
    assert result["task"]["status"] == "assigned"
    # Off-switch byte identity: no protocol field is written at all.
    task = _task_meta(tmp_path)
    assert "decision_class" not in task
    assert "decision_status" not in task
    # Compat rule: an absent decision class reads as the r4 default.
    assert server.review._decision_class_of(task) == constants.DEFAULT_DECISION_CLASS


# --- Row 2: enforce on, delegate without class -> missing ---------------


def test_row2_enforce_on_missing_class_rejected(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, delegate_env: dict[str, list[Any]]
) -> None:
    _enforce_on(monkeypatch)
    _write_project(tmp_path)

    result = _delegate(tmp_path)

    assert result["ok"] is False
    assert result["error_code"] == "missing_decision_class"
    assert result["message"] == (
        "decision_class is required (r1..r6, see contracts/decision-classes.json)"
    )
    # No task state is created by the rejected delegation.
    assert not (tmp_path / "shared" / "tasks" / TASK_ID / "meta.json").exists()


# --- Row 3: enforce on, r4 accepted and persisted -------------------------


def test_row3_enforce_on_r4_persisted(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, delegate_env: dict[str, list[Any]]
) -> None:
    _enforce_on(monkeypatch)
    _write_project(tmp_path)

    result = _delegate(tmp_path, decision_class="r4")

    assert result["ok"] is True
    assert result["task"]["decision_class"] == "r4"
    assert _task_meta(tmp_path)["decision_class"] == "r4"


def test_enforce_on_unknown_class_rejected(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, delegate_env: dict[str, list[Any]]
) -> None:
    _enforce_on(monkeypatch)
    _write_project(tmp_path)

    result = _delegate(tmp_path, decision_class="r9")

    assert result["ok"] is False
    assert result["error_code"] == "unknown_decision_class"
    assert result["message"] == (
        "decision_class must be one of r1, r2, r3, r4, r5, r6 "
        "(see contracts/decision-classes.json)"
    )


def test_config_file_switch_and_env_override(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, delegate_env: dict[str, list[Any]]
) -> None:
    # Switch on via the runtime config file (protocol.enforce: true).
    config_path = tmp_path / "runtime-config.json"
    config_path.write_text(
        json.dumps({"protocol": {"enforce": True}}), encoding="utf-8"
    )
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(config_path))
    _write_project(tmp_path)
    result = _delegate(tmp_path)
    assert result["ok"] is False
    assert result["error_code"] == "missing_decision_class"

    # Env override "0" wins over the config file: the guard goes back off.
    monkeypatch.setenv("TEAMHARNESS_PROTOCOL_ENFORCE", "0")
    _write_project(tmp_path, task_id="w2-task-02")
    result = _delegate(tmp_path, task_id="w2-task-02")
    assert result["ok"] is True
    assert "decision_class" not in _task_meta(tmp_path, "w2-task-02")


# --- Row 4: r1 without approval record -> approval_required --------------


def test_row4_r1_without_approval_rejected(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    _enforce_on(monkeypatch)
    # The review gate passes (a pass record is present); the gate under
    # test here is the r1 approval requirement.
    _write_submitted_task(
        tmp_path,
        decision_class="r1",
        submitted_by="worker:alice",
        review={
            "verdict": "pass",
            "reviewer": "critic:bob",
            "reviewer_team": "",
            "reviewed_at": "2026-10-29T00:00:00Z",
            "note": "looks right",
        },
    )

    result = _accept(tmp_path)

    assert result["ok"] is False
    assert result["error_code"] == "approval_required"
    assert result["message"] == (
        "r1 task result requires a resolved approval record "
        "before it can be decided"
    )


# --- Row 5: r1 with a resolved approval record -> accepted ---------------


def test_row5_r1_with_approval_accepted(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    _enforce_on(monkeypatch)
    # Both W2 gates pass: a pass review record AND a resolved approval
    # record carrying the server-written trusted-authority marker
    # (round 4: resolvedRole="human" — written only by the authorized
    # close path — plus a human channel and a decider account that
    # differs from the assignee) — this row isolates the approval
    # gate's success path.
    _write_submitted_task(
        tmp_path,
        decision_class="r1",
        assigned_to="@alice:example.test",
        submitted_by="worker:alice",
        review={
            "verdict": "pass",
            "reviewer": "critic:bob",
            "reviewer_team": "",
            "reviewed_at": "2026-10-29T00:00:00Z",
            "note": "looks right",
        },
        attention=[
            {
                "kind": "approval",
                "question": "Approve the result?",
                "attempt": 1,
                "resolved": True,
                "eventId": "$att-approval-1",
                "stall_count": 0,
                "resolvedVia": "im_reply",
                "resolvedBy": "human:@luo:example.test",
                "resolveEvidence": {"messageId": "$m1", "note": "approved in room"},
                "resolvedRole": "human",
                "resolvedActor": "human:@luo:example.test",
            }
        ],
    )

    result = _accept(tmp_path)

    assert result["ok"] is True
    assert result["nodeStatus"] == "completed"
    assert result["accepted"] is True


# --- Row 6: self review (same account, different role) --------------------


def test_row6_same_account_review_forbidden(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    _enforce_on(monkeypatch)
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    submitted = _submit(tmp_path, role="worker", account="alice")
    assert submitted["ok"] is True
    assert submitted["task"]["submitted_by"] == "worker:alice"

    # Same account as the submitter, different role: still self-review.
    result = _review(tmp_path, role="leader", account="alice", verdict="pass")
    assert result["ok"] is False
    assert result["error_code"] == "self_review_forbidden"
    assert result["message"] == "reviewer must differ from submitted_by"
    assert "review" not in _task_meta(tmp_path)

    # A different account is allowed and the record is persisted.
    ok = _review(tmp_path, role="leader", account="bob", verdict="pass")
    assert ok["ok"] is True
    review = _task_meta(tmp_path)["review"]
    assert review["reviewer"] == "leader:bob"
    assert review["verdict"] == "pass"


# --- Row 7: verdict fail -> accept refused --------------------------------


def test_row7_fail_verdict_blocks_accept(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="worker:alice")
    reviewed = _review(tmp_path, role="critic", account="bob", verdict="fail")
    assert reviewed["ok"] is True

    result = _accept(tmp_path)

    assert result["ok"] is False
    assert result["error_code"] == "review_not_passed"
    assert result["message"] == (
        "accept_task_result requires a passing review verdict "
        "(current verdict: 'fail')"
    )


def test_enforce_off_fail_verdict_does_not_block(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    # Switch off: the pre-W2 behaviour, a fail review is advisory only.
    _write_submitted_task(
        tmp_path,
        submitted_by="worker:alice",
        review={
            "verdict": "fail",
            "reviewer": "leader:bob",
            "reviewer_team": "",
            "reviewed_at": "2026-10-29T00:00:00Z",
            "note": "nope",
        },
    )

    result = _accept(tmp_path)

    assert result["ok"] is True
    assert result["nodeStatus"] == "completed"


# --- Blocker 1: a missing / malformed review record refuses acceptance ----


def test_accept_without_review_record_rejected(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """A submitted r4 task with NO review record must not be accepted:
    the guard reports review_missing and leaves the task 'submitted'."""
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="worker:alice")

    result = _accept(tmp_path)

    assert result["ok"] is False
    assert result["error_code"] == "review_missing"
    assert result["message"] == (
        "accept_task_result requires a review record before accepting a result "
        "(missing or malformed)"
    )
    assert _task_meta(tmp_path)["status"] == "submitted"


def test_accept_with_malformed_review_rejected(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """A review that is not a verdict-bearing record — a bare string, an
    empty dict, or a dict without a verdict — is malformed:
    review_missing, task left 'submitted'."""
    _enforce_on(monkeypatch)
    for malformed in ("fail", {}, {"reviewer": "critic:bob"}):
        _write_submitted_task(
            tmp_path, submitted_by="worker:alice", review=malformed
        )

        result = _accept(tmp_path)

        assert result["ok"] is False
        assert result["error_code"] == "review_missing"
        assert _task_meta(tmp_path)["status"] == "submitted"


def test_accept_with_pass_review_accepted(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """The positive path: a verdict-bearing pass record unblocks the
    acceptance of a submitted r4 result."""
    _enforce_on(monkeypatch)
    _write_submitted_task(
        tmp_path,
        submitted_by="worker:alice",
        review={
            "verdict": "pass",
            "reviewer": "critic:bob",
            "reviewer_team": "",
            "reviewed_at": "2026-10-29T00:00:00Z",
            "note": "looks right",
        },
    )

    result = _accept(tmp_path)

    assert result["ok"] is True
    assert result["nodeStatus"] == "completed"
    assert result["accepted"] is True
    assert _task_meta(tmp_path)["status"] == "completed"


# --- Blocker 2: a fail verdict must not block rejection -------------------


def test_rejection_with_fail_verdict_reaches_revision(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """With a fail review record the leader's accepted=false must still
    work: the result is REVISION_NEEDED / the node 'revision', no
    review_not_passed, and the task is NOT left 'submitted'."""
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="worker:alice")
    reviewed = _review(tmp_path, role="critic", account="bob", verdict="fail")
    assert reviewed["ok"] is True

    result = _call("projectflow", {
        "role": "leader",
        "action": "accept_task_result",
        "workspaceDir": str(tmp_path),
        "payload": {
            "projectId": PROJECT_ID,
            "taskId": TASK_ID,
            "submissionId": SUBMISSION_ID,
            "resultStatus": "SUCCESS",
            "accepted": False,
        },
    })

    assert result["ok"] is True
    assert "error_code" not in result
    assert result["nodeStatus"] == "revision"
    assert result["accepted"] is False
    task = _task_meta(tmp_path)
    assert task["status"] == "revision"
    # The node follows the task meta (accepted=false maps the submitted
    # SUCCESS result to REVISION_NEEDED / node 'revision').
    assert result["project"]["tasks"][0]["status"] == "revision"


# --- Blocker 3: distinct colon-containing identities must not collide -----


def test_account_of_strips_only_the_role_prefix() -> None:
    account_of = server.review._account_of
    # A known role prefix is stripped; the rest — colons included — is
    # the account (Matrix identities @user:server).
    assert account_of("worker:@alice:example.test") == "@alice:example.test"
    assert account_of("critic:@bob:example.test") == "@bob:example.test"
    assert account_of("remote-member:@carol:example.test") == "@carol:example.test"
    assert account_of("unknown:@dave:example.test") == "@dave:example.test"
    # A plain account (no colon) is unchanged.
    assert account_of("alice") == "alice"
    # An actor whose prefix is not a known role is returned unchanged —
    # never split on the last colon ('example.test' collided every
    # example.test account under the old rsplit).
    assert account_of("observer:@alice:example.test") == "observer:@alice:example.test"


def test_review_colon_identities(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """worker:@alice:example.test submitting and critic:@alice:example.test
    reviewing is still self-review (same full account); a distinct
    full account (@bob) is allowed — under the old rsplit both reduced
    to 'example.test' and were refused."""
    _enforce_on(monkeypatch)
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    submitted = _submit(tmp_path, role="worker", account="@alice:example.test")
    assert submitted["ok"] is True
    assert submitted["task"]["submitted_by"] == "worker:@alice:example.test"

    same = _review(tmp_path, role="critic", account="@alice:example.test", verdict="pass")
    assert same["ok"] is False
    assert same["error_code"] == "self_review_forbidden"
    assert same["message"] == "reviewer must differ from submitted_by"
    assert "review" not in _task_meta(tmp_path)

    ok = _review(tmp_path, role="critic", account="@bob:example.test", verdict="pass")
    assert ok["ok"] is True
    review = _task_meta(tmp_path)["review"]
    assert review["reviewer"] == "critic:@bob:example.test"
    assert review["verdict"] == "pass"


def test_review_identical_actor_string_forbidden(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """The identical actor string (same role and account) is refused by
    the exact-identity branch of the self-review guard. (A worker cannot
    be the reviewer — the role gate precedes the identity check — so the
    identical-string case is exercised with the critic identity on both
    sides.)"""
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="critic:@alice:example.test")

    result = _review(tmp_path, role="critic", account="@alice:example.test", verdict="pass")

    assert result["ok"] is False
    assert result["error_code"] == "self_review_forbidden"
    assert "review" not in _task_meta(tmp_path)


# --- Row 8: the stall chain 0/1/2/3 ---------------------------------------


def test_row8_stall_chain_escalates(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    _enforce_on(monkeypatch)
    _write_in_progress_task(tmp_path)

    created = _request_attention(tmp_path)
    assert created["ok"] is True
    record = created["task"]["attention"][0]
    assert record["stall_count"] == 0
    assert record["resolved"] is False
    assert record.get("eventId")
    assert created["task"].get("decision_status", constants.DEFAULT_DECISION_STATUS) == "open"

    bumped = _request_attention(tmp_path)
    assert bumped["ok"] is True
    assert bumped["attention"]["reused"] is True
    task = bumped["task"]
    assert len(task["attention"]) == 1, "reuse path must not append a second record"
    assert task["attention"][0]["stall_count"] == 1
    assert task.get("decision_status", constants.DEFAULT_DECISION_STATUS) == "open"
    assert "decision_status_history" not in task

    blocked = _request_attention(tmp_path)
    task = blocked["task"]
    assert task["attention"][0]["stall_count"] == constants.ATTENTION_STALL_BLOCKED
    assert task["decision_status"] == "blocked"
    history = task["decision_status_history"]
    assert len(history) == 1
    assert history[0] == {
        "ts": history[0]["ts"],
        "from": "open",
        "to": "blocked",
        "reason": "attention stalled 2 times without an answer",
        "source": "attention:decision",
    }

    frozen = _request_attention(tmp_path)
    task = frozen["task"]
    assert task["attention"][0]["stall_count"] == constants.ATTENTION_STALL_FROZEN
    assert task["decision_status"] == "frozen"
    history = task["decision_status_history"]
    assert [(h["from"], h["to"]) for h in history] == [("open", "blocked"), ("blocked", "frozen")]
    assert history[1]["reason"] == "attention stalled 3 times without an answer"
    # The persisted meta agrees (the chain writes through).
    assert _task_meta(tmp_path)["decision_status"] == "frozen"


# --- Row 9: only a resolved approval record unfreezes ---------------------


def test_row9_frozen_unfreeze_requires_approval(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    _enforce_on(monkeypatch)
    _write_in_progress_task(tmp_path)

    # Escalate the decision loop to frozen: create + 3 unanswered re-requests.
    _request_attention(tmp_path)
    _request_attention(tmp_path)
    _request_attention(tmp_path)
    frozen = _request_attention(tmp_path)
    assert frozen["task"]["decision_status"] == "frozen"

    # A non-approval answer cannot unfreeze a frozen task; the refusal
    # names the operator contract (round 4: the approval close itself
    # needs the trusted human role).
    refused = _request_attention(tmp_path, kind="decision", resolved=True)
    assert refused["ok"] is False
    assert refused["error_code"] == "frozen_unfreeze_requires_approval"
    assert refused["message"] == (
        "task is frozen; only a resolved approval record (kind=approval) "
        "can unfreeze it, and that close requires a trusted human runtime "
        "role: enable a human integration with AGENTTEAMS_AGENT_ROLE=human "
        "(or member.role=human in the runtime config); a tool caller "
        "without a trusted role cannot approve or unfreeze"
    )
    task = _task_meta(tmp_path)
    assert task["decision_status"] == "frozen"
    assert task["attention"][0]["resolved"] is False

    # Open the approval channel (a worker may raise the loop even while
    # frozen — the gate is on the close, not the raise).
    opened = _request_attention(tmp_path, kind="approval", question="Approve?")
    assert opened["ok"] is True

    # A worker close of the approval record is not an authorized human
    # decision (round 4: no trusted role, fail closed): refused, and
    # the task stays frozen.
    worker_close = _request_attention(
        tmp_path, kind="approval", resolved=True, resolved_via="tool"
    )
    assert worker_close["ok"] is False
    assert worker_close["error_code"] == "approval_requires_human"
    frozen_task = _task_meta(tmp_path)
    assert frozen_task["decision_status"] == "frozen"
    assert frozen_task["attention"][1]["resolved"] is False

    # Answer the approval from a human authority (the trusted runtime
    # role is operator-set: here 'human'): the task thaws.
    monkeypatch.setattr(server, "_runtime_role", lambda: "human")
    answered = _request_attention(
        tmp_path, kind="approval", resolved=True, resolved_via="im_reply"
    )
    assert answered["ok"] is True
    task = answered["task"]
    assert task["decision_status"] == "open"
    approval = next(r for r in task["attention"] if r["kind"] == "approval")
    assert approval["resolved"] is True
    assert approval["stall_count"] == 0
    assert approval["resolvedVia"] == "im_reply"
    # The server-derived authority marker (round 4): written by the
    # authorized close path, not taken from the payload.
    assert approval["resolvedRole"] == "human"
    assert approval["resolvedActor"] == "human:default"
    history = task["decision_status_history"]
    assert history[-1]["to"] == "open"
    assert history[-1]["reason"] == "unfrozen by approval record"
    assert history[-1]["source"] == "attention:approval"


# --- Row 10: contracts and constants must not drift -----------------------


def test_row10_contracts_match_constants() -> None:
    contracts_dir = MCP_DIR.parent / "contracts"

    classes = json.loads(
        (contracts_dir / "decision-classes.json").read_text(encoding="utf-8")
    )
    ids = [str(item.get("id") or "") for item in classes["classes"]]
    assert ids == list(constants.DECISION_CLASS_IDS), (
        "decision-classes.json class ids (order) drifted from DECISION_CLASS_IDS"
    )
    assert set(ids) == set(constants.DECISION_CLASS_IDS)

    attention = json.loads(
        (contracts_dir / "attention.json").read_text(encoding="utf-8")
    )
    assert attention["stall_thresholds"]["blocked"] == constants.ATTENTION_STALL_BLOCKED
    assert attention["stall_thresholds"]["frozen"] == constants.ATTENTION_STALL_FROZEN
    assert attention["decision_status"]["default"] == constants.DEFAULT_DECISION_STATUS
    assert list(attention["decision_status"]["values"]) == list(
        constants.DECISION_STATUS_VALUES
    )
    assert attention["switch"]["default"] is False


# --- Round 3/4: approval authority (#1467) ---------------------------------


def test_r3_worker_tool_close_denied_keeps_frozen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """Case 2: a worker raises the approval loop, drives it to frozen,
    then closes it (resolvedVia=tool, no trusted runtime role) -> denied
    approval_requires_human; the task stays frozen and the record stays
    open with no provenance written."""
    _enforce_on(monkeypatch)
    _write_in_progress_task(tmp_path)
    _request_attention(tmp_path, kind="approval", question="Approve?")
    _request_attention(tmp_path, kind="approval")
    _request_attention(tmp_path, kind="approval")
    frozen = _request_attention(tmp_path, kind="approval")
    assert frozen["task"]["decision_status"] == "frozen"

    denied = _request_attention(
        tmp_path, kind="approval", resolved=True, resolved_via="tool"
    )
    assert denied["ok"] is False
    assert denied["error_code"] == "approval_requires_human"
    assert denied["message"] == (
        "closing an approval record (and unfreezing a frozen task) "
        "requires a trusted human runtime role: enable a human "
        "integration with AGENTTEAMS_AGENT_ROLE=human (or "
        "member.role=human in the runtime config); a tool caller "
        "without a trusted role cannot approve or unfreeze"
    )
    task = _task_meta(tmp_path)
    assert task["decision_status"] == "frozen"
    record = task["attention"][0]
    assert record["resolved"] is False
    assert "resolvedActor" not in record
    assert "resolvedRole" not in record
    assert "resolvedVia" not in record


def test_r3_trusted_worker_beats_payload_human_claim(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """Case 3: the trusted runtime role is 'worker' while the payload
    claims resolvedVia=im_reply + resolvedBy=human:… -> denied
    approval_requires_human: without the trusted human runtime role the
    close fails closed, whatever the payload claims; the record stays
    open with no provenance written."""
    _enforce_on(monkeypatch)
    monkeypatch.setattr(server, "_runtime_role", lambda: "worker")
    _write_in_progress_task(tmp_path)
    _request_attention(tmp_path, kind="approval", question="Approve?")

    denied = _request_attention(
        tmp_path,
        kind="approval",
        resolved=True,
        resolved_via="im_reply",
        resolved_by="human:@luo:example.test",
        resolve_evidence={"messageId": "$m1", "note": "approved in room"},
    )
    assert denied["ok"] is False
    assert denied["error_code"] == "approval_requires_human"
    record = _task_meta(tmp_path)["attention"][0]
    assert record["resolved"] is False
    assert "resolvedActor" not in record


def test_r3_trusted_human_im_reply_unfreezes_and_records_provenance(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """Case 4: the trusted runtime role is 'human' + resolvedVia=im_reply
    + evidence -> resolves; frozen -> open; the record carries the
    provenance fields (resolvedVia / resolveEvidence) plus the
    server-written trusted-authority marker (resolvedRole="human",
    resolvedActor="human:<account>")."""
    _enforce_on(monkeypatch)
    monkeypatch.setattr(server, "_runtime_role", lambda: "human")
    _write_in_progress_task(tmp_path)
    _request_attention(tmp_path, kind="approval", question="Approve?")
    _request_attention(tmp_path, kind="approval")
    _request_attention(tmp_path, kind="approval")
    frozen = _request_attention(tmp_path, kind="approval")
    assert frozen["task"]["decision_status"] == "frozen"

    answered = _request_attention(
        tmp_path,
        kind="approval",
        resolved=True,
        resolved_via="im_reply",
        resolve_evidence={"messageId": "$m1", "note": "approved in room"},
    )
    assert answered["ok"] is True
    task = answered["task"]
    assert task["decision_status"] == "open"
    record = task["attention"][0]
    assert record["resolved"] is True
    assert record["resolvedVia"] == "im_reply"
    assert record["resolveEvidence"] == {
        "messageId": "$m1",
        "note": "approved in room",
    }
    # The trusted-authority marker is server-written: the trusted
    # runtime role plus the account behind the transition actor.
    assert record["resolvedRole"] == "human"
    assert record["resolvedActor"].startswith("human:")
    assert record["resolvedActor"] == "human:default"
    assert record["resolvedAt"]
    last = task["decision_status_history"][-1]
    assert last["from"] == "frozen"
    assert last["to"] == "open"
    assert last["reason"] == "unfrozen by approval record"
    assert last["source"] == "attention:approval"
    assert last["actor"] == "human:default"


def test_r4_payload_marker_injection_ignored(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """Case 4b: the trusted runtime role is 'worker' and the payload
    injects the trusted-authority fields themselves (resolvedRole="human"
    + resolvedActor="human:@luo:…") -> denied approval_requires_human
    and the record is untouched: the server writes the marker itself, a
    caller-supplied one is never copied, so no fabricated marker exists
    for the r1 gate to read."""
    _enforce_on(monkeypatch)
    monkeypatch.setattr(server, "_runtime_role", lambda: "worker")
    _write_in_progress_task(tmp_path)
    _request_attention(tmp_path, kind="approval", question="Approve?")

    denied = _call("taskflow", {
        "role": "worker",
        "action": "request_attention",
        "workspaceDir": str(tmp_path),
        "account_id": "@luo:example.test",
        "payload": {
            "taskId": TASK_ID,
            "kind": "approval",
            "question": "Approve?",
            "resolved": True,
            "resolvedVia": "im_reply",
            "resolvedBy": "human:@luo:example.test",
            "resolveEvidence": {"messageId": "$m1", "note": "approved in room"},
            "resolvedRole": "human",
            "resolvedActor": "human:@luo:example.test",
        },
    })
    assert denied["ok"] is False
    assert denied["error_code"] == "approval_requires_human"
    record = _task_meta(tmp_path)["attention"][0]
    assert record["resolved"] is False
    assert "resolvedRole" not in record
    assert "resolvedActor" not in record
    assert "resolvedVia" not in record


def test_r4_forged_provenance_denied_keeps_frozen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """Case 1: the maintainer's exact forged payload — no trusted runtime
    role, and the caller claims role=human + im_reply +
    resolvedBy=human:@luo:… + an invented resolveEvidence on a frozen
    approval -> denied approval_requires_human; decision_status stays
    frozen and nothing was written to the record."""
    _enforce_on(monkeypatch)
    _write_in_progress_task(tmp_path)
    _request_attention(tmp_path, kind="approval", question="Approve?")
    _request_attention(tmp_path, kind="approval")
    _request_attention(tmp_path, kind="approval")
    frozen = _request_attention(tmp_path, kind="approval")
    assert frozen["task"]["decision_status"] == "frozen"

    denied = _request_attention(
        tmp_path,
        kind="approval",
        resolved=True,
        role="human",
        account="@luo:example.test",
        resolved_via="im_reply",
        resolved_by="human:@luo:example.test",
        resolve_evidence={"messageId": "$forged-1", "note": "approved in room"},
    )
    assert denied["ok"] is False
    assert denied["error_code"] == "approval_requires_human"
    task = _task_meta(tmp_path)
    assert task["decision_status"] == "frozen"
    record = task["attention"][0]
    assert record["resolved"] is False
    for field in (
        "resolvedVia",
        "resolvedBy",
        "resolveEvidence",
        "resolvedActor",
        "resolvedRole",
    ):
        assert field not in record


@pytest.mark.parametrize("channel", ["tool", "api"])
def test_r3_payload_human_on_agent_channel_denied(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None, channel: str,
) -> None:
    """Case 5: no trusted runtime role; the payload claims role=human on
    an agent channel (tool / api) -> denied approval_requires_human:
    with trusted authority only, the channel is irrelevant — without the
    trusted human runtime role the close fails closed, whatever the
    payload claims."""
    _enforce_on(monkeypatch)
    _write_in_progress_task(tmp_path)
    _request_attention(tmp_path, kind="approval", question="Approve?")

    denied = _request_attention(
        tmp_path,
        kind="approval",
        resolved=True,
        role="human",
        account="@luo:example.test",
        resolved_via=channel,
        resolved_by="human:@luo:example.test",
        resolve_evidence={"messageId": "$m1", "note": "approved in room"},
    )
    assert denied["ok"] is False
    assert denied["error_code"] == "approval_requires_human"
    record = _task_meta(tmp_path)["attention"][0]
    assert record["resolved"] is False


def test_r3_worker_self_closed_approval_does_not_satisfy_r1(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 6: after a genuine critic pass, the worker tries to close
    its own approval record (tool, no trusted runtime role) -> the close
    is denied and the leader's accept_task_result still returns
    approval_required (the self-closed record must not satisfy the r1
    gate)."""
    _enforce_on(monkeypatch)
    _write_submitted_task(
        tmp_path,
        decision_class="r1",
        assigned_to=WORKER_A,
        submitted_by="worker:alice",
        review={
            "verdict": "pass",
            "reviewer": "critic:bob",
            "reviewer_team": "",
            "reviewed_at": "2026-10-29T00:00:00Z",
            "note": "looks right",
        },
        attention=[
            {
                "kind": "approval",
                "question": "Approve the result?",
                "attempt": 1,
                "resolved": False,
                "eventId": "$att-approval-1",
                "stall_count": 0,
            }
        ],
    )

    denied = _request_attention(
        tmp_path, kind="approval", resolved=True, resolved_via="tool"
    )
    assert denied["ok"] is False
    assert denied["error_code"] == "approval_requires_human"

    result = _accept(tmp_path)
    assert result["ok"] is False
    assert result["error_code"] == "approval_required"
    assert _task_meta(tmp_path)["status"] == "submitted"


def test_r4_r1_forged_approval_then_trusted_human(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 5 (the maintainer's r1 attack, end to end): after a genuine
    critic pass, the forged approval (payload claims human, no trusted
    runtime role) cannot be closed and accept_task_result still returns
    approval_required; the same close under the trusted runtime role
    'human' then resolves — the record carries the server-written
    marker — and the acceptance succeeds (completed)."""
    _enforce_on(monkeypatch)
    _write_submitted_task(
        tmp_path,
        decision_class="r1",
        assigned_to=WORKER_A,
        submitted_by="worker:alice",
        review={
            "verdict": "pass",
            "reviewer": "critic:bob",
            "reviewer_team": "",
            "reviewed_at": "2026-10-29T00:00:00Z",
            "note": "looks right",
        },
        attention=[
            {
                "kind": "approval",
                "question": "Approve the result?",
                "attempt": 1,
                "resolved": False,
                "eventId": "$att-approval-1",
                "stall_count": 0,
            }
        ],
    )

    forged = _request_attention(
        tmp_path,
        kind="approval",
        resolved=True,
        role="human",
        account="@luo:example.test",
        resolved_via="im_reply",
        resolved_by="human:@luo:example.test",
        resolve_evidence={"messageId": "$forged-1", "note": "approved in room"},
    )
    assert forged["ok"] is False
    assert forged["error_code"] == "approval_requires_human"

    result = _accept(tmp_path)
    assert result["ok"] is False
    assert result["error_code"] == "approval_required"
    assert _task_meta(tmp_path)["status"] == "submitted"

    monkeypatch.setattr(server, "_runtime_role", lambda: "human")
    answered = _request_attention(
        tmp_path,
        kind="approval",
        resolved=True,
        role="human",
        account="@luo:example.test",
        resolved_via="im_reply",
        resolved_by="human:@luo:example.test",
        resolve_evidence={"messageId": "$m2", "note": "approved in room"},
    )
    assert answered["ok"] is True
    record = answered["task"]["attention"][0]
    assert record["resolved"] is True
    assert record["resolvedRole"] == "human"
    assert record["resolvedActor"].startswith("human:")

    # The human integration's close is done; the acceptance runs under
    # the leader's own payload role (the trusted role is gone — the
    # authority already lives in the server-written marker on the
    # record, not in the runtime of the accept call).
    monkeypatch.setattr(server, "_runtime_role", lambda: "")
    result = _accept(tmp_path)
    assert result["ok"] is True, result
    assert result["nodeStatus"] == "completed"
    assert _task_meta(tmp_path)["status"] == "completed"


def test_r3_human_self_approval_does_not_count(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 7: a human approval whose decider account equals the task's
    assignee account -> self-approval never counts; accept still
    returns approval_required."""
    _enforce_on(monkeypatch)
    _write_submitted_task(
        tmp_path,
        decision_class="r1",
        assigned_to="@luo:example.test",
        submitted_by="worker:luo",
        review={
            "verdict": "pass",
            "reviewer": "critic:bob",
            "reviewer_team": "",
            "reviewed_at": "2026-10-29T00:00:00Z",
            "note": "looks right",
        },
        attention=[
            {
                "kind": "approval",
                "question": "Approve the result?",
                "attempt": 1,
                "resolved": True,
                "eventId": "$att-approval-1",
                "stall_count": 0,
                "resolvedVia": "im_reply",
                "resolvedBy": "human:@luo:example.test",
                "resolveEvidence": {"messageId": "$m1", "note": "approved"},
                "resolvedRole": "human",
                "resolvedActor": "human:@luo:example.test",
            }
        ],
    )

    result = _accept(tmp_path)
    assert result["ok"] is False
    assert result["error_code"] == "approval_required"
    assert _task_meta(tmp_path)["status"] == "submitted"


def test_r3_human_approval_from_distinct_account_accepted(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 8: critic pass + a human approval from an account that
    differs from the assignee -> accepted (completed)."""
    _enforce_on(monkeypatch)
    _write_submitted_task(
        tmp_path,
        decision_class="r1",
        assigned_to="@alice:example.test",
        submitted_by="worker:alice",
        review={
            "verdict": "pass",
            "reviewer": "critic:bob",
            "reviewer_team": "",
            "reviewed_at": "2026-10-29T00:00:00Z",
            "note": "looks right",
        },
        attention=[
            {
                "kind": "approval",
                "question": "Approve the result?",
                "attempt": 1,
                "resolved": True,
                "eventId": "$att-approval-1",
                "stall_count": 0,
                "resolvedVia": "im_command",
                "resolvedBy": "human:@luo:example.test",
                "resolveEvidence": {"messageId": "$m2", "note": "approved"},
                "resolvedRole": "human",
                "resolvedActor": "human:@luo:example.test",
            }
        ],
    )

    result = _accept(tmp_path)
    assert result["ok"] is True
    assert result["nodeStatus"] == "completed"
    assert _task_meta(tmp_path)["status"] == "completed"


def test_r3_off_switch_worker_close_behaves_as_before(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """Case 9: with the switch off the worker flow is byte-identical —
    the worker may close its own approval record via the tool, and no
    new protocol field is written (decision_status / stall_count /
    resolvedActor / resolvedRole)."""
    _write_in_progress_task(tmp_path)

    created = _request_attention(tmp_path, kind="approval", question="Approve?")
    assert created["ok"] is True
    closed = _request_attention(
        tmp_path, kind="approval", resolved=True, resolved_via="tool"
    )
    assert closed["ok"] is True
    record = closed["task"]["attention"][0]
    assert record["resolved"] is True
    assert record["resolvedVia"] == "tool"
    assert "resolvedActor" not in record
    assert "resolvedRole" not in record
    assert "stall_count" not in record
    assert "decision_status" not in closed["task"]


def test_r3_record_counting_matrix() -> None:
    """The r1 gate's record counting, pinned at unit level: the
    trusted-authority marker (the server-written resolvedRole="human" —
    never inferred from the decider strings), the human channel set, the
    server-written decider (resolvedActor ONLY — resolvedBy is display
    provenance and never decides; a record without the marker is not an
    approval, #1467 round 8), and the self-approval exclusion
    (canonicalized: the MXID localpart and the runtimeName of the same
    member are the same identity)."""
    has = server.review._has_approval_record

    def record(**overrides: Any) -> dict[str, Any]:
        base: dict[str, Any] = {
            "kind": "approval",
            "resolved": True,
            "resolvedRole": "human",
            "resolvedVia": "im_reply",
            "resolvedBy": "human:@luo:example.test",
            "resolvedActor": "human:@luo:example.test",
        }
        base.update(overrides)
        return base

    # A human-channel close with the trusted marker and a server-written
    # decider account counts — on any human channel.
    assert has({"attention": [record()]}) is True
    assert has({"attention": [record(resolvedVia="im_command")]}) is True
    assert has({"attention": [record(resolvedVia="dashboard")]}) is True
    # Never on an agent channel, when unresolved, or for another kind.
    assert has({"attention": [record(resolvedVia="tool")]}) is False
    assert has({"attention": [record(resolvedVia="api")]}) is False
    assert has({"attention": [record(resolved=False)]}) is False
    assert has({"attention": [record(kind="decision")]}) is False
    # The marker is the authority: a wrong, empty, or missing
    # resolvedRole never counts, whatever the decider strings claim.
    assert has({"attention": [record(resolvedRole="worker")]}) is False
    assert has({"attention": [record(resolvedRole="")]}) is False
    stripped = {k: v for k, v in record().items() if k != "resolvedRole"}
    assert has({"attention": [stripped]}) is False
    # The decider is the server-written resolvedActor ONLY (#1467
    # round 8): a record without it is not an approval — fail closed —
    # whatever its display fields claim (resolvedBy never decides).
    by_only = {k: v for k, v in record().items() if k != "resolvedActor"}
    assert has({"attention": [by_only]}) is False
    # A decider without an account (a bare "human" claim, or empty)
    # never counts, even with the marker.
    assert has({"attention": [record(resolvedActor="human")]}) is False
    assert has({"attention": [record(resolvedActor="")]}) is False
    # Self-approval (decider account == submitter account) never
    # counts — canonicalized one way on both sides, so the MXID
    # localpart and the runtimeName of the same member are the same
    # identity (and a distinct account, in either spelling, still
    # counts).
    assert has({"attention": [record()]}, submitter_account="@luo:example.test") is False
    assert has({"attention": [record()]}, submitter_account="luo") is False
    assert has({"attention": [record(resolvedActor="human:luo")]},
               submitter_account="@luo:example.test") is False
    assert has({"attention": [record()]}, submitter_account="@alice:example.test") is True
    assert has({"attention": [record()]}, submitter_account="alice") is True


# --- Round 5 (#1467): frozen is absorbing ----------------------------------


def _history_pairs(task: dict[str, Any]) -> list[tuple[str, str]]:
    """The (from, to) sequence of a task's decision_status_history."""
    return [(e["from"], e["to"]) for e in task.get("decision_status_history") or []]


def test_r5_maintainer_chain_frozen_absorbing(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """The maintainer's exact chain: an approval record stalls to
    frozen; a separate decision record then stalls again (it must not
    demote the task to blocked) and is resolved (it must not open the
    task). Frozen stays until the trusted human closes the approval
    record — the single authorized exit."""
    _enforce_on(monkeypatch)
    monkeypatch.setattr(server, "_runtime_role", lambda: "worker")
    _write_in_progress_task(tmp_path)

    # The approval chain drives the task to frozen.
    created = _request_attention(tmp_path, kind="approval", question="Approve?")
    assert created["ok"] is True
    assert created["task"].get("decision_status", constants.DEFAULT_DECISION_STATUS) == "open"
    assert _history_pairs(_task_meta(tmp_path)) == []
    _request_attention(tmp_path, kind="approval")  # stall 1 -> open
    assert _history_pairs(_task_meta(tmp_path)) == []
    _request_attention(tmp_path, kind="approval")  # stall 2 -> blocked
    assert _history_pairs(_task_meta(tmp_path)) == [("open", "blocked")]
    frozen = _request_attention(tmp_path, kind="approval")  # stall 3 -> frozen
    assert frozen["ok"] is True
    assert frozen["task"]["decision_status"] == "frozen"
    assert _history_pairs(_task_meta(tmp_path)) == [
        ("open", "blocked"), ("blocked", "frozen")
    ]

    # A second record stalls on the frozen task: the escalation
    # short-circuits — no demotion to blocked, no history churn.
    _request_attention(tmp_path, kind="decision", question="Side decision.")
    assert _task_meta(tmp_path)["decision_status"] == "frozen"
    _request_attention(tmp_path, kind="decision")  # stall 1
    assert _task_meta(tmp_path)["decision_status"] == "frozen"
    _request_attention(tmp_path, kind="decision")  # stall 2: would target blocked
    assert _task_meta(tmp_path)["decision_status"] == "frozen"
    assert _history_pairs(_task_meta(tmp_path)) == [
        ("open", "blocked"), ("blocked", "frozen")
    ]

    # Resolving the ordinary record cannot leave frozen (the policy
    # guard refuses it before any write); the approval stays open.
    refused = _request_attention(tmp_path, kind="decision", resolved=True)
    assert refused["ok"] is False
    assert refused["error_code"] == "frozen_unfreeze_requires_approval"
    task = _task_meta(tmp_path)
    assert task["decision_status"] == "frozen"
    assert _history_pairs(task) == [("open", "blocked"), ("blocked", "frozen")]
    assert task["attention"][0]["resolved"] is False

    # The trusted human closes the approval record: the one authorized
    # exit — a single frozen -> open entry.
    monkeypatch.setattr(server, "_runtime_role", lambda: "human")
    answered = _request_attention(
        tmp_path,
        kind="approval",
        resolved=True,
        role="human",
        account="@luo:example.test",
        resolved_via="im_reply",
    )
    assert answered["ok"] is True
    task = _task_meta(tmp_path)
    assert task["decision_status"] == "open"
    assert _history_pairs(task) == [
        ("open", "blocked"), ("blocked", "frozen"), ("frozen", "open")
    ]
    assert task["decision_status_history"][-1]["reason"] == "unfrozen by approval record"
    assert task["decision_status_history"][-1]["source"] == "attention:approval"
    assert task["attention"][0]["resolved"] is True


@pytest.mark.parametrize("kind", ("approval", "decision", "escalation", "other"))
def test_r5_absorbing_matrix(
    kind: str, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """Every schema kind, every other writer, from frozen: raise the
    same kind again, retry past both thresholds, resolve the ordinary
    record, tool close — the task stays frozen after every step and
    the history never grows past the two entry/exit pairs."""
    _enforce_on(monkeypatch)
    monkeypatch.setattr(server, "_runtime_role", lambda: "worker")
    _write_in_progress_task(tmp_path)

    def assert_frozen_clean() -> None:
        assert _task_meta(tmp_path)["decision_status"] == "frozen"
        assert _history_pairs(_task_meta(tmp_path)) == [
            ("open", "blocked"), ("blocked", "frozen")
        ]

    # Drive the task to frozen through this kind's own chain:
    # create (stall 0) + re-requests 1, 2, 3.
    created = _request_attention(tmp_path, kind=kind, question="Ask?")
    assert created["ok"] is True
    _request_attention(tmp_path, kind=kind)
    _request_attention(tmp_path, kind=kind)
    frozen = _request_attention(tmp_path, kind=kind)
    assert frozen["ok"] is True
    assert_frozen_clean()
    assert _task_meta(tmp_path)["attention"][0]["stall_count"] == 3

    # Raise the same kind again (reuse path, stall 4).
    raised = _request_attention(tmp_path, kind=kind)
    assert raised["ok"] is True
    assert raised["attention"]["reused"] is True
    assert _task_meta(tmp_path)["attention"][0]["stall_count"] == 4
    assert_frozen_clean()

    # Retry past both thresholds (stall 5, 6).
    for expected in (5, 6):
        retried = _request_attention(tmp_path, kind=kind)
        assert retried["ok"] is True
        assert _task_meta(tmp_path)["attention"][0]["stall_count"] == expected
        assert_frozen_clean()

    # Resolve the ordinary record: refused, frozen stays.
    if kind == "approval":
        _request_attention(tmp_path, kind="decision", question="Side decision.")
        resolved = _request_attention(tmp_path, kind="decision", resolved=True)
    else:
        resolved = _request_attention(tmp_path, kind=kind, resolved=True)
    assert resolved["ok"] is False
    assert resolved["error_code"] == "frozen_unfreeze_requires_approval"
    assert_frozen_clean()

    # A tool close: the approval channel needs a trusted human.
    if kind != "approval":
        _request_attention(tmp_path, kind="approval", question="Tool close?")
    closed = _request_attention(
        tmp_path, kind="approval", resolved=True, resolved_via="tool"
    )
    assert closed["ok"] is False
    assert closed["error_code"] == "approval_requires_human"
    assert_frozen_clean()


def test_r5_refusal_is_audited_at_the_choke_point() -> None:
    """The choke point itself (unit level, no protocol switch): a
    frozen task refuses frozen -> blocked and frozen -> open without
    approval authority, auditing each attempt as a from='frozen',
    to='frozen', refused=<attempted status> entry — and the authorized
    approval close is the only writer of frozen -> open."""
    task: dict[str, Any] = {
        "task_id": "w2-task-unit",
        "status": "in_progress",
        "decision_status": "frozen",
        "decision_status_history": [
            {"ts": "2026-10-31T00:00:00Z", "from": "open", "to": "blocked"},
            {"ts": "2026-10-31T00:00:01Z", "from": "blocked", "to": "frozen"},
        ],
    }

    refused = server.transitions._set_decision_status(
        task,
        "blocked",
        reason="attention stalled 2 times without an answer",
        source="attention:decision",
        actor="worker:@alice:example.test",
    )
    assert refused == "frozen"
    assert task["decision_status"] == "frozen"
    entry = task["decision_status_history"][-1]
    assert entry["from"] == "frozen"
    assert entry["to"] == "frozen"
    assert entry["refused"] == "blocked"
    assert entry["reason"] == "attention stalled 2 times without an answer"
    assert entry["source"] == "attention:decision"
    assert entry["actor"] == "worker:@alice:example.test"

    refused = server.transitions._set_decision_status(
        task,
        "open",
        reason="attention answer received",
        source="attention:decision",
        actor="worker:@alice:example.test",
    )
    assert refused == "frozen"
    assert task["decision_status"] == "frozen"
    entry = task["decision_status_history"][-1]
    assert entry["from"] == "frozen"
    assert entry["to"] == "frozen"
    assert entry["refused"] == "open"
    assert "actor" in entry

    # No phantom transition has entered the history.
    assert _history_pairs(task) == [
        ("open", "blocked"), ("blocked", "frozen"),
        ("frozen", "frozen"), ("frozen", "frozen"),
    ]

    # The authorized approval close is the only exit.
    effective = server.transitions._set_decision_status(
        task,
        "open",
        reason="unfrozen by approval record",
        source="attention:approval",
        actor="human:@luo:example.test",
        unfreeze_authority="approval",
    )
    assert effective == "open"
    assert task["decision_status"] == "open"
    entry = task["decision_status_history"][-1]
    assert entry["from"] == "frozen"
    assert entry["to"] == "open"
    assert "refused" not in entry


# --- Round 6 (#1467): trusted review identity -------------------------------


def _set_runtime_member(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    member: dict[str, Any],
    filename: str,
) -> None:
    """Point TEAMHARNESS_RUNTIME_CONFIG at a config file carrying only
    the given member block (the shape the DeepSeek-harness adapter
    writes — fixtures/runtime.yaml). Each call stands in for a
    different process's runtime identity."""
    config_path = tmp_path / filename
    config_path.write_text(json.dumps({"member": member}), encoding="utf-8")
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(config_path))


def test_r6_distinct_members_without_account_id(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 1 (the maintainer's exact failure): a Worker process whose
    runtime config says member.name=worker submits, then a Leader
    process (member.name=leader) reviews — neither caller supplies
    account_id (it is not declared in the taskflow schema). The review
    succeeds and both sides carry the runtime member identity, not the
    shared 'default' fallback."""
    _enforce_on(monkeypatch)
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    _set_runtime_member(monkeypatch, tmp_path, {"name": "worker"}, "worker-config.json")
    submitted = _submit(tmp_path, role="worker")
    assert submitted["ok"] is True
    assert submitted["task"]["submitted_by"] == "worker:worker"

    # The second process: a different runtime config, the leader.
    _set_runtime_member(monkeypatch, tmp_path, {"name": "leader"}, "leader-config.json")
    result = _review(tmp_path, role="leader", verdict="pass")
    assert result["ok"] is True, result
    review = _task_meta(tmp_path)["review"]
    assert review["reviewer"] == "leader:leader"
    assert review["verdict"] == "pass"
    assert _task_meta(tmp_path)["submitted_by"] == "worker:worker"


def test_r6_trusted_identity_beats_payload_evasion(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 2: the submitter's runtime member is 'worker'; the SAME
    member (same runtime config) attempts the review carrying
    account_id=leader in the payload. The trusted identity wins: the
    reviewer resolves to 'leader:worker' and self_review_forbidden
    holds — the payload field is provenance, never authorization."""
    _enforce_on(monkeypatch)
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    _set_runtime_member(monkeypatch, tmp_path, {"name": "worker"}, "worker-config.json")
    submitted = _submit(tmp_path, role="worker")
    assert submitted["ok"] is True
    assert submitted["task"]["submitted_by"] == "worker:worker"

    result = _review(tmp_path, role="leader", account="leader", verdict="pass")
    assert result["ok"] is False
    assert result["error_code"] == "self_review_forbidden"
    assert "review" not in _task_meta(tmp_path)


def test_r6_legacy_default_submitter_not_self_review(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 3: a record written by an older build
    (submitted_by='worker:default' — the shared fallback that collapsed
    every member) is reviewed by a trusted runtime member 'leader' ->
    allowed: an unknown submitter account is not a self-review match,
    and the review record carries the trusted identity."""
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="worker:default")

    _set_runtime_member(monkeypatch, tmp_path, {"name": "leader"}, "leader-config.json")
    result = _review(tmp_path, role="leader", verdict="pass")
    assert result["ok"] is True, result
    review = _task_meta(tmp_path)["review"]
    assert review["reviewer"] == "leader:leader"


def test_r6_no_trusted_identity_payload_account_still_differentiates(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 4: no runtime config, no name env — the local / test path
    (clean_env guarantees neither leaks in). The payload's account_id
    still differentiates accounts (backward compatible): the same
    account self-reviewing is blocked, a distinct account is allowed."""
    _enforce_on(monkeypatch)
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    submitted = _submit(tmp_path, role="worker", account="alice")
    assert submitted["ok"] is True
    assert submitted["task"]["submitted_by"] == "worker:alice"

    same = _review(tmp_path, role="critic", account="alice", verdict="pass")
    assert same["ok"] is False
    assert same["error_code"] == "self_review_forbidden"

    ok = _review(tmp_path, role="critic", account="bob", verdict="pass")
    assert ok["ok"] is True
    assert _task_meta(tmp_path)["review"]["reviewer"] == "critic:bob"


def test_r6_runtime_identity_resolution_order(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The resolution order of _runtime_identity (unit level): the
    AGENTTEAMS_WORKER_NAME env override first, then member.name,
    member.runtimeName, the matrixUserId localpart; normalized (strip,
    lowercase); '' when unknown."""
    assert server._runtime_identity() == ""

    # The env override wins, normalized.
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "  Dsh-Worker-A ")
    assert server._runtime_identity() == "dsh-worker-a"

    def config_path(member: dict[str, Any], name: str) -> Path:
        path = tmp_path / name
        path.write_text(json.dumps({"member": member}), encoding="utf-8")
        return path

    monkeypatch.delenv("AGENTTEAMS_WORKER_NAME")
    # member.name first.
    monkeypatch.setenv(
        "TEAMHARNESS_RUNTIME_CONFIG",
        str(config_path({"name": "Cfg-Name", "runtimeName": "rt"}, "cfg1.json")),
    )
    assert server._runtime_identity() == "cfg-name"
    # ...then runtimeName (stripped), then the matrixUserId localpart.
    monkeypatch.setenv(
        "TEAMHARNESS_RUNTIME_CONFIG",
        str(config_path({"runtimeName": "  Rt-Name "}, "cfg2.json")),
    )
    assert server._runtime_identity() == "rt-name"
    monkeypatch.setenv(
        "TEAMHARNESS_RUNTIME_CONFIG",
        str(config_path({"matrixUserId": "@User.X:matrix.local"}, "cfg3.json")),
    )
    assert server._runtime_identity() == "user.x"
    # An empty member block stays unknown.
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(config_path({}, "cfg4.json")))
    assert server._runtime_identity() == ""


# --- Round 7 (#1467): identity provenance -----------------------------------


def test_r7_trusted_default_member_self_review_forbidden(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 1 (the maintainer's case): the operator provisioned a
    trusted Matrix member literally named 'default'. The member submits
    as worker — the record is worker:default with
    submitted_by_source='runtime'. The operator then reconfigures the
    SAME member's role to leader: its review resolves to leader:default
    (reviewer_source='runtime') and is refused self_review_forbidden —
    a trusted name is never discarded as the legacy sentinel."""
    _enforce_on(monkeypatch)
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    # The member's trusted runtime identity is literally "default".
    _set_runtime_member(monkeypatch, tmp_path, {"name": "default"}, "member-config.json")
    submitted = _submit(tmp_path, role="worker")
    assert submitted["ok"] is True
    assert submitted["task"]["submitted_by"] == "worker:default"
    assert submitted["task"]["submitted_by_source"] == "runtime"
    # The provenance marker is persisted, not just returned.
    assert _task_meta(tmp_path)["submitted_by"] == "worker:default"
    assert _task_meta(tmp_path)["submitted_by_source"] == "runtime"

    # Same member, operator reconfigured its role to leader.
    result = _review(tmp_path, role="leader", verdict="pass")
    assert result["ok"] is False
    assert result["error_code"] == "self_review_forbidden"
    assert result["message"] == "reviewer must differ from submitted_by"
    assert "review" not in _task_meta(tmp_path)


def test_r7_trusted_concrete_member_control_unchanged(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 2 (the control): the identical flow with member.name=worker
    (a name that was never ambiguous) still ends
    self_review_forbidden — the round-6 behaviour is unchanged."""
    _enforce_on(monkeypatch)
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    _set_runtime_member(monkeypatch, tmp_path, {"name": "worker"}, "worker-config.json")
    submitted = _submit(tmp_path, role="worker")
    assert submitted["ok"] is True
    assert submitted["task"]["submitted_by"] == "worker:worker"
    assert submitted["task"]["submitted_by_source"] == "runtime"

    result = _review(tmp_path, role="leader", verdict="pass")
    assert result["ok"] is False
    assert result["error_code"] == "self_review_forbidden"
    assert "review" not in _task_meta(tmp_path)


def test_r7_distinct_trusted_member_reviews_default_named_submitter(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 3: a trusted member named 'default' submits; a DISTINCT
    trusted member (member.name=leader) reviews -> allowed. Both sides
    are runtime-sourced and the accounts differ, and the review record
    carries its own provenance (reviewer_source='runtime')."""
    _enforce_on(monkeypatch)
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    _set_runtime_member(monkeypatch, tmp_path, {"name": "default"}, "worker-config.json")
    submitted = _submit(tmp_path, role="worker")
    assert submitted["ok"] is True
    assert submitted["task"]["submitted_by"] == "worker:default"
    assert submitted["task"]["submitted_by_source"] == "runtime"

    # The second process: a different member, 'leader'.
    _set_runtime_member(monkeypatch, tmp_path, {"name": "leader"}, "leader-config.json")
    result = _review(tmp_path, role="leader", verdict="pass")
    assert result["ok"] is True, result
    review = _task_meta(tmp_path)["review"]
    assert review["reviewer"] == "leader:leader"
    assert review["reviewer_source"] == "runtime"
    assert review["verdict"] == "pass"


def test_r7_legacy_default_submitter_without_source_still_allowed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 4 (legacy compatibility): a pre-round-7 record
    (submitted_by='worker:default', NO submitted_by_source marker) is
    reviewed by a trusted 'leader' -> allowed: a record without runtime
    provenance is treated as unknown, so the shared 'default' sentinel
    can never be a self-review match."""
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="worker:default")

    _set_runtime_member(monkeypatch, tmp_path, {"name": "leader"}, "leader-config.json")
    result = _review(tmp_path, role="leader", verdict="pass")
    assert result["ok"] is True, result
    review = _task_meta(tmp_path)["review"]
    assert review["reviewer"] == "leader:leader"
    assert review["reviewer_source"] == "runtime"


def test_r7_legacy_concrete_submitter_without_source_still_forbidden(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 5 (legacy compatibility): a pre-round-7 record
    (submitted_by='worker:alice', NO source marker) reviewed by a
    trusted 'alice' -> forbidden: even without provenance, concrete
    accounts still compare."""
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="worker:alice")

    _set_runtime_member(monkeypatch, tmp_path, {"name": "alice"}, "leader-config.json")
    result = _review(tmp_path, role="leader", verdict="pass")
    assert result["ok"] is False
    assert result["error_code"] == "self_review_forbidden"
    assert "review" not in _task_meta(tmp_path)


def test_r7_payload_reviewer_cannot_claim_trusted_submitter(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 6: a trusted member 'alice' submits (the record carries
    submitted_by_source='runtime'); a reviewer with NO trusted runtime
    identity claims account_id='alice' in the payload ->
    self_review_forbidden: on a mixed-provenance pair the round-6
    sentinel/concrete rules apply — the payload account is provenance
    only, and a concrete account still matches the submitter's. (No
    regression of the round-6 evasion tests.)"""
    _enforce_on(monkeypatch)
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    _set_runtime_member(monkeypatch, tmp_path, {"name": "alice"}, "worker-config.json")
    submitted = _submit(tmp_path, role="worker")
    assert submitted["ok"] is True
    assert submitted["task"]["submitted_by"] == "worker:alice"
    assert submitted["task"]["submitted_by_source"] == "runtime"

    # The review process has NO trusted runtime identity (local / test
    # path): its account comes from the payload only.
    monkeypatch.delenv("TEAMHARNESS_RUNTIME_CONFIG")
    result = _review(tmp_path, role="critic", account="alice", verdict="pass")
    assert result["ok"] is False
    assert result["error_code"] == "self_review_forbidden"
    assert "review" not in _task_meta(tmp_path)


# --- Round 8 (#1467): the trusted decider ----------------------------------

_R8_REVIEW = {
    "verdict": "pass",
    "reviewer": "critic:bob",
    "reviewer_team": "",
    "reviewed_at": "2026-10-31T00:00:00Z",
    "note": "looks right",
}


def _r8_record(resolved_actor: str, resolved_by: str) -> dict[str, Any]:
    """A resolved approval record in the shape the authorized close
    writes: the trusted-authority marker (resolvedRole / resolvedActor
    — server-written) plus the display provenance (resolvedBy —
    caller-written)."""
    record: dict[str, Any] = {
        "kind": "approval",
        "question": "Approve the result?",
        "attempt": 1,
        "resolved": True,
        "eventId": "$att-approval-1",
        "stall_count": 0,
        "resolvedVia": "im_reply",
        "resolvedBy": resolved_by,
        "resolveEvidence": {"messageId": "$m1", "note": "approved in room"},
        "resolvedRole": "human",
    }
    if resolved_actor is not None:
        record["resolvedActor"] = resolved_actor
    return record


def test_r8_display_field_never_decides(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """The maintainer's pair: a human runtime role is configured for
    the SAME identity as the submitter — the server wrote
    resolvedActor='human:worker' from the trusted runtime member.
    Changing ONLY the caller-written display field (resolvedBy) must
    not change the decision: the trusted decider is the submitter, so
    self-approval holds in both spellings, the r1 acceptance stays
    approval_required, and the task never reaches completed."""
    _enforce_on(monkeypatch)
    _write_submitted_task(
        tmp_path,
        decision_class="r1",
        assigned_to="@worker:review.test",
        submitted_by="worker:worker",
        review=dict(_R8_REVIEW),
        attention=[
            _r8_record(
                resolved_actor="human:worker",
                resolved_by="human:@worker:review.test",
            )
        ],
    )

    result = _accept(tmp_path)
    assert result["ok"] is False
    assert result["error_code"] == "approval_required"
    assert _task_meta(tmp_path)["status"] == "submitted"

    # Flip ONLY the display field (the same trusted decider): still
    # self-approval, still refused.
    task = _task_meta(tmp_path)
    task["attention"][0]["resolvedBy"] = "human:@human:review.test"
    _write_task_meta(tmp_path, task)

    result = _accept(tmp_path)
    assert result["ok"] is False
    assert result["error_code"] == "approval_required"
    assert _task_meta(tmp_path)["status"] == "submitted"


def test_r8_distinct_human_decider_accepted(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """A GENUINELY different human: the server-written decider
    (resolvedActor='human:other') differs from the submitter's
    identity in both spellings -> the r1 acceptance succeeds and the
    task is persisted as completed."""
    _enforce_on(monkeypatch)
    _write_submitted_task(
        tmp_path,
        decision_class="r1",
        assigned_to="@worker:review.test",
        submitted_by="worker:worker",
        review=dict(_R8_REVIEW),
        attention=[
            _r8_record(
                resolved_actor="human:other",
                resolved_by="human:@other:review.test",
            )
        ],
    )

    result = _accept(tmp_path)
    assert result["ok"] is True
    assert result["nodeStatus"] == "completed"
    assert _task_meta(tmp_path)["status"] == "completed"


def test_r8_mxid_localpart_equals_runtimename(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """One canonicalization on both sides of the comparison: the MXID
    localpart and the runtimeName of the same member are the same
    identity, so self-approval is refused in BOTH spellings — decider
    'human:@worker:review.test' against the task account 'worker', and
    the reverse, decider 'human:worker' against '@worker:review.test'.
    (The display field carries an unrelated human's spelling in both:
    only the server-written decider is compared.)"""
    _enforce_on(monkeypatch)
    for assigned_to, resolved_actor in (
        ("worker", "human:@worker:review.test"),
        ("@worker:review.test", "human:worker"),
    ):
        _write_submitted_task(
            tmp_path,
            decision_class="r1",
            assigned_to=assigned_to,
            submitted_by="worker:worker",
            review=dict(_R8_REVIEW),
            attention=[
                _r8_record(
                    resolved_actor=resolved_actor,
                    resolved_by="human:@other:review.test",
                )
            ],
        )

        result = _accept(tmp_path)
        assert result["ok"] is False, (assigned_to, resolved_actor)
        assert result["error_code"] == "approval_required"
        assert _task_meta(tmp_path)["status"] == "submitted"


def test_r8_missing_server_marker_fails_closed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Fail closed: a resolved approval record with the human marker
    role, a human channel, and a human-looking resolvedBy — but NO
    server-written resolvedActor — is not an approval (a record that
    lacks the server marker can never carry authority): the r1
    acceptance stays approval_required."""
    _enforce_on(monkeypatch)
    _write_submitted_task(
        tmp_path,
        decision_class="r1",
        assigned_to="@worker:review.test",
        submitted_by="worker:worker",
        review=dict(_R8_REVIEW),
        attention=[
            _r8_record(
                resolved_actor=None,
                resolved_by="human:@luo:review.test",
            )
        ],
    )

    result = _accept(tmp_path)
    assert result["ok"] is False
    assert result["error_code"] == "approval_required"
    assert _task_meta(tmp_path)["status"] == "submitted"


# --- review_task gate edges -------------------------------------------------


def test_review_requires_critic_or_leader(
    tmp_path: Path, storage: None
) -> None:
    _write_submitted_task(tmp_path)

    result = _review(tmp_path, role="worker", verdict="pass")

    assert result["ok"] is False
    assert result["error"] == "review_task requires critic or leader role"
    assert "review" not in _task_meta(tmp_path)


def test_review_requires_submitted_state(
    tmp_path: Path, storage: None
) -> None:
    _write_in_progress_task(tmp_path)

    result = _review(tmp_path, role="critic", verdict="pass")

    assert result["ok"] is False
    assert result["error"] == "illegal_transition"
    assert "task is 'in_progress'" in result["detail"]


def test_review_invalid_verdict_rejected(
    tmp_path: Path, storage: None
) -> None:
    _write_submitted_task(tmp_path)

    result = _review(tmp_path, role="critic", verdict="meh")

    assert result["ok"] is False
    assert result["error"] == "verdict must be one of pass, fail"


# --- off-switch byte identity for submit and attention ---------------------


def test_submit_writes_submitted_by_only_when_enforcing(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    _enforce_on(monkeypatch)
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    result = _submit(tmp_path, role="worker", account="alice")
    assert result["ok"] is True
    assert _task_meta(tmp_path)["submitted_by"] == "worker:alice"


def test_submit_without_enforce_writes_no_identity(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "assigned",
        "assigned_to": WORKER_A,
    })

    result = _submit(tmp_path, role="worker", account="alice")
    assert result["ok"] is True
    task = _task_meta(tmp_path)
    assert "submitted_by" not in task
    assert "submitted_by_source" not in task
    assert task["status"] == "submitted"


def test_attention_without_enforce_writes_no_protocol_fields(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    _write_in_progress_task(tmp_path)

    created = _request_attention(tmp_path)
    assert created["ok"] is True
    assert "stall_count" not in created["task"]["attention"][0]
    assert "decision_status" not in created["task"]

    reused = _request_attention(tmp_path)
    assert reused["attention"]["reused"] is True
    assert "stall_count" not in reused["task"]["attention"][0]
    assert "decision_status" not in reused["task"]


# --- Round 9 (#1467): the authoritative task before cross-actor writes ---
#
# The round-9 repro: the leader's local task cache predates the worker's
# stall-escalated decision_status=frozen; a cross-actor write (review /
# cancel / accept) mirrors the WHOLE local directory back to shared
# storage and deletes the frozen state, the approval record and the
# decision_status_history. The guarded entries now load the
# authoritative task first (fail closed on a failed pull), so the write
# lands on the frozen state instead of clobbering it.


def _r9_frozen_record() -> dict[str, Any]:
    """The approval record of the round-9 repro: the worker's approval
    request stalled to the frozen threshold (stall_count 3) and the
    authorized close resolved it — the record a frozen task persists in
    shared storage (same shape as the row-5 / round-8 records)."""
    return {
        "kind": "approval",
        "question": "Approve the result?",
        "attempt": 1,
        "resolved": True,
        "eventId": "$att-approval-1",
        "stall_count": 3,
        "resolvedVia": "im_reply",
        "resolvedBy": "human:@approver:example.test",
        "resolveEvidence": {"messageId": "$m1", "note": "approved in room"},
        "resolvedRole": "human",
        "resolvedActor": "human:@approver:example.test",
    }


def _r9_freeze(task: dict[str, Any]) -> dict[str, Any]:
    """A copy of the task with the round-9 frozen state layered on:
    decision_status=frozen, the approval record, and the
    decision_status_history (open -> blocked -> frozen) exactly as the
    stall chain writes it (transitions._set_decision_status)."""
    frozen = dict(task)
    frozen["decision_status"] = "frozen"
    frozen["attention"] = [_r9_frozen_record()]
    frozen["decision_status_history"] = [
        {
            "ts": "2026-11-01T00:00:00Z",
            "from": "open",
            "to": "blocked",
            "reason": "attention stalled 2 times without an answer",
            "source": "attention:approval",
        },
        {
            "ts": "2026-11-01T00:01:00Z",
            "from": "blocked",
            "to": "frozen",
            "reason": "attention stalled 3 times without an answer",
            "source": "attention:approval",
        },
    ]
    return frozen


def test_r9_review_preserves_remote_frozen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, shared_remote: dict[str, Any]
) -> None:
    """The maintainer's round-9 repro: the leader's local cache holds the
    task as submitted/open while shared storage holds the same task
    frozen by the worker's approval stall chain. The review (a
    cross-actor whole-directory write) must load the authoritative task
    first, so the committed directory records the review ON TOP of the
    frozen state instead of clobbering it."""
    _enforce_on(monkeypatch)
    cached = _write_submitted_task(tmp_path, submitted_by="worker:alice")
    frozen = _r9_freeze(cached)
    shared_remote["remote"] = frozen

    result = _review(tmp_path, role="leader", account="carol", verdict="pass")

    assert result["ok"] is True
    committed = shared_remote["committed"]
    assert committed is not None
    assert committed["status"] == "submitted"
    # The frozen state survived the review write.
    assert committed["decision_status"] == "frozen"
    assert committed["attention"] == [_r9_frozen_record()]
    assert committed["decision_status_history"] == frozen["decision_status_history"]
    # ... and the review was recorded on top of it.
    assert committed["review"]["verdict"] == "pass"
    assert committed["review"]["reviewer"] == "leader:carol"


def test_r9_review_pull_failure_refuses_stale_copy(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, shared_remote: dict[str, Any]
) -> None:
    """Fail closed: when the authoritative pull fails, the review must
    refuse to write the stale local cache back (which would clobber the
    remote frozen state) — the call returns an error, the local meta is
    untouched and nothing is committed to shared storage."""
    _enforce_on(monkeypatch)
    cached = _write_submitted_task(tmp_path, submitted_by="worker:alice")
    shared_remote["remote"] = _r9_freeze(cached)
    shared_remote["pull_ok"] = False

    result = _review(tmp_path, role="leader", account="carol", verdict="pass")

    assert result["ok"] is False
    assert "stale local copy" in result["error"]
    # The stale local cache was not written back: still submitted/open,
    # no review record, no frozen field adopted locally.
    local = _task_meta(tmp_path)
    assert local["status"] == "submitted"
    assert "review" not in local
    assert "decision_status" not in local
    assert shared_remote["committed"] is None


def test_r9_cancel_preserves_remote_frozen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, shared_remote: dict[str, Any]
) -> None:
    """cancel_task is the same family of cross-actor write: the
    leader's in-flight task cache predates the worker's stall-frozen
    state. The cancel must land on the authoritative task, so the
    committed directory is cancelled but keeps decision_status=frozen
    with its approval record and history."""
    _enforce_on(monkeypatch)
    _write_in_progress_task(tmp_path)
    cached = _task_meta(tmp_path)
    frozen = _r9_freeze(cached)
    shared_remote["remote"] = frozen

    result = _call("taskflow", {
        "role": "leader",
        "action": "cancel_task",
        "workspaceDir": str(tmp_path),
        "payload": {
            "projectId": PROJECT_ID,
            "taskId": TASK_ID,
            "reason": "superseded by the approval stall",
        },
    })

    assert result["ok"] is True
    committed = shared_remote["committed"]
    assert committed is not None
    assert committed["status"] == "cancelled"
    # The frozen state survived the cancel write-back.
    assert committed["decision_status"] == "frozen"
    assert committed["attention"] == [_r9_frozen_record()]
    assert committed["decision_status_history"] == frozen["decision_status_history"]


def test_r9_accept_preserves_remote_frozen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, shared_remote: dict[str, Any]
) -> None:
    """accept_task_result decides a submitted result and writes the task
    meta back (whole directory): the leader's stale submitted cache must
    not clobber the authoritative frozen state. The commit carries the
    terminal node status AND the frozen decision_status with its
    approval record and the pass review the acceptance gate validated
    (the r1 approval gate is out of scope: decision_class reads r4)."""
    _enforce_on(monkeypatch)
    cached = _write_submitted_task(tmp_path, submitted_by="worker:alice")
    frozen = _r9_freeze(cached)
    frozen["review"] = {
        "verdict": "pass",
        "reviewer": "critic:bob",
        "reviewer_team": "",
        "reviewed_at": "2026-10-29T00:00:00Z",
        "note": "looks right",
    }
    shared_remote["remote"] = frozen

    result = _accept(tmp_path)

    assert result["ok"] is True
    assert result["nodeStatus"] == "completed"
    committed = shared_remote["committed"]
    assert committed is not None
    assert committed["status"] == "completed"
    # The frozen state survived the acceptance write-back.
    assert committed["decision_status"] == "frozen"
    assert committed["attention"] == [_r9_frozen_record()]
    assert committed["decision_status_history"] == frozen["decision_status_history"]
    assert committed["review"]["verdict"] == "pass"
