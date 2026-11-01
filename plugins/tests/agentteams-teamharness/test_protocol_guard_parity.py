"""W2 protocol guards (issue #1451 W2) — second-tree parity.

``plugins/agentteams-teamharness`` carries a whole-tree compatibility
copy of ``plugins/teamharness`` (see its DEDUPE.md; the drift is
enforced by ``golden/test_compatibility_copy_drift.py``). The
three-blocker fixes — the review gate applies to a positive acceptance
only (missing/malformed record -> review_missing), a fail verdict never
blocks a rejection/return-for-revision, and identity comparison uses the
full account after the role prefix — therefore must hold in the COPY as
well. The round-3/4 approval-authority fix (#1467; round 4: trusted
authority only) landed in ``mcp/lifecycle/taskflow.py`` and
``mcp/lifecycle/review.py`` as well, so it is part of the comparison
set. This module:

(a) asserts the three guard files (``mcp/lifecycle/review.py``,
    ``mcp/lifecycle/taskflow.py``, ``mcp/project/flow.py``) stay
    byte-identical between the trees;
(b) re-runs the blocker regression cases against the copy's own
    ``mcp/server.py`` — loaded in this suite's own process (this file is
    the only one here that imports ``mcp``), so the ``mcp`` package
    resolves to the ``agentteams-teamharness`` tree — plus the round-3/4
    approval-authority cases. Round 4 (#1467) is trusted authority only:
    the close requires the trusted runtime role 'human' (fail closed
    without it; a forged provenance payload — role=human + im_reply +
    resolvedBy + resolveEvidence, or an injected resolvedRole /
    resolvedActor — is never copied into the record), so the key
    regressions are re-run here: the forged provenance on a frozen
    approval is denied with nothing written (case 1), and the r1 forged
    approval -> approval_required -> trusted human approval ->
    accepted flow (case 5).
Round 5 (#1467) — frozen is absorbing: the round-5 regressions (the
maintainer's chain, the per-kind absorbing matrix, and the choke-point
refusal audit against the copy's transitions module) are re-run here.
"""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[2] / "agentteams-teamharness" / "mcp"
LEGACY_MCP_DIR = Path(__file__).resolve().parents[2] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


def _load_server() -> Any:
    """This file's own copy of the bundle tree's server (suite
    convention, see plugins/tests/teamharness/test_protocol_guard.py):
    refs name resolution is process-global and every harness that execs
    server.py rebinds it at exec time — so this file's monkeypatches
    must land on the copy the proxies resolve against, i.e. the copy
    this file loads itself."""
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_protocol_guard_parity_test",
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
    """Drive this file's own server copy (loaded once; no other test
    file of this suite runs between the tests of this file, so the
    binding holds for all of them)."""
    global server, _own_copy_loaded
    if not _own_copy_loaded:
        server = _load_server()
        _own_copy_loaded = True


WORKER_A = "@alice:example.test"
ROOM = "!taskroom:example.test"

PROJECT_ID = "w2p-project"
TASK_ID = "w2p-task-01"

SUBMISSION_ID = "sub-w2p-0001"


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
        "title": "W2 parity project",
        "status": "active",
        "tasks": [
            {
                "task_id": task_id,
                "title": "W2 parity task",
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
        "continuation": {"status": "pending", "delivery_id": "dl-w2p-0001"},
    }
    task.update(extra)
    _write_task_meta(workspace, task)
    return task


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
    accepted: bool = True,
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
            "accepted": accepted,
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


def _write_in_progress_task(workspace: Path) -> None:
    _write_project(workspace, task_status="in_progress")
    _write_task_meta(workspace, {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "status": "in_progress",
        "room_id": ROOM,
        "assigned_to": WORKER_A,
    })


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


# --- (a) the guard files stay byte-identical between the trees -----------


@pytest.mark.parametrize(
    "relpath",
    [
        "lifecycle/review.py",
        "lifecycle/taskflow.py",
        # Round 6 (#1467): the trusted-identity change landed in the
        # transition-actor and server modules as well.
        "lifecycle/transitions.py",
        "server.py",
        "project/flow.py",
    ],
)
def test_guard_files_byte_identical_between_trees(relpath: str) -> None:
    legacy = (LEGACY_MCP_DIR / relpath).read_bytes()
    bundle = (MCP_DIR / relpath).read_bytes()
    assert bundle == legacy, (
        f"mcp/{relpath} drifted between plugins/teamharness and "
        "plugins/agentteams-teamharness (re-sync whole per DEDUPE.md)"
    )


# --- (b) the blocker regression cases against the bundle tree ------------


def test_accept_without_review_record_rejected(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Blocker 1: a submitted r4 task with NO review record is refused
    with review_missing and left 'submitted'."""
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
    """Blocker 1: a review that is not a verdict-bearing record (a bare
    string, an empty dict, a dict without a verdict) is malformed:
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


def test_rejection_with_fail_verdict_reaches_revision(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Blocker 2: with a fail review record the leader's accepted=false
    still works: node 'revision', no review_not_passed, task NOT left
    'submitted'."""
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="worker:alice")
    reviewed = _review(tmp_path, role="critic", account="bob", verdict="fail")
    assert reviewed["ok"] is True

    result = _accept(tmp_path, accepted=False)

    assert result["ok"] is True
    assert "error_code" not in result
    assert result["nodeStatus"] == "revision"
    assert result["accepted"] is False
    assert _task_meta(tmp_path)["status"] == "revision"


def test_fail_verdict_still_blocks_acceptance(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """The other side of Blocker 2: a fail verdict still refuses a
    POSITIVE acceptance (review_not_passed)."""
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="worker:alice")
    reviewed = _review(tmp_path, role="critic", account="bob", verdict="fail")
    assert reviewed["ok"] is True

    result = _accept(tmp_path, accepted=True)

    assert result["ok"] is False
    assert result["error_code"] == "review_not_passed"
    assert _task_meta(tmp_path)["status"] == "submitted"


def test_account_of_strips_only_the_role_prefix() -> None:
    account_of = server.review._account_of
    assert account_of("worker:@alice:example.test") == "@alice:example.test"
    assert account_of("critic:@bob:example.test") == "@bob:example.test"
    assert account_of("remote-member:@carol:example.test") == "@carol:example.test"
    assert account_of("unknown:@dave:example.test") == "@dave:example.test"
    assert account_of("alice") == "alice"
    # An actor whose prefix is not a known role is returned unchanged —
    # never split on the last colon.
    assert account_of("observer:@alice:example.test") == "observer:@alice:example.test"


def test_review_colon_identities(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Blocker 3: same full account across roles is still self-review;
    a distinct full account (@bob) is allowed — under the old rsplit
    both reduced to 'example.test' and were refused."""
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


# --- (c) round 3/4: approval authority (#1467) against the bundle tree ----


def test_r3_parity_worker_tool_close_denied_keeps_frozen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """Case 2 (bundle tree): a worker drives the approval loop to frozen
    and closes it (resolvedVia=tool, no trusted runtime role) -> denied
    approval_requires_human; the task stays frozen and the record stays
    open."""
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
    task = _task_meta(tmp_path)
    assert task["decision_status"] == "frozen"
    assert task["attention"][0]["resolved"] is False


def test_r3_parity_worker_self_closed_approval_does_not_satisfy_r1(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 6 (bundle tree): after a genuine critic pass, the worker's
    tool close of its own approval record is denied, and
    accept_task_result still returns approval_required (the self-closed
    record must not satisfy the r1 gate)."""
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


def test_r3_parity_human_approval_from_distinct_account_accepted(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 8 (bundle tree): critic pass + a human approval (carrying
    the trusted-authority marker) from an account that differs from the
    assignee -> accepted (completed)."""
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


def test_r4_parity_forged_provenance_denied_keeps_frozen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """Case 1 (bundle tree): the maintainer's exact forged payload — no
    trusted runtime role, and the caller claims role=human + im_reply +
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


def test_r4_parity_r1_forged_approval_then_trusted_human(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 5 (bundle tree, the maintainer's r1 attack, end to end):
    after a genuine critic pass, the forged approval (payload claims
    human, no trusted runtime role) cannot be closed and
    accept_task_result still returns approval_required; the same close
    under the trusted runtime role 'human' then resolves — the record
    carries the server-written marker — and the acceptance succeeds
    (completed)."""
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
    # the leader's own payload role (the authority already lives in the
    # server-written marker on the record).
    monkeypatch.setattr(server, "_runtime_role", lambda: "")
    result = _accept(tmp_path)
    assert result["ok"] is True
    assert result["nodeStatus"] == "completed"
    assert _task_meta(tmp_path)["status"] == "completed"


# --- Round 5 (#1467): frozen is absorbing ----------------------------------


def _history_pairs(task: dict[str, Any]) -> list[tuple[str, str]]:
    """The (from, to) sequence of a task's decision_status_history."""
    return [(e["from"], e["to"]) for e in task.get("decision_status_history") or []]


def test_r5_parity_maintainer_chain_frozen_absorbing(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """The maintainer's exact chain (bundle tree): an approval record
    stalls to frozen; a separate decision record then stalls again (it
    must not demote the task to blocked) and is resolved (it must not
    open the task). Frozen stays until the trusted human closes the
    approval record — the single authorized exit."""
    _enforce_on(monkeypatch)
    monkeypatch.setattr(server, "_runtime_role", lambda: "worker")
    _write_in_progress_task(tmp_path)

    # The approval chain drives the task to frozen.
    created = _request_attention(tmp_path, kind="approval", question="Approve?")
    assert created["ok"] is True
    assert created["task"].get("decision_status", "open") == "open"
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
def test_r5_parity_absorbing_matrix(
    kind: str, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    storage: None, attention_env: None,
) -> None:
    """Every schema kind, every other writer, from frozen (bundle
    tree): raise the same kind again, retry past both thresholds,
    resolve the ordinary record, tool close — the task stays frozen
    after every step and the history never grows past the two
    entry/exit pairs."""
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


def test_r5_parity_refusal_is_audited_at_the_choke_point() -> None:
    """The choke point itself (bundle tree, unit level, no protocol
    switch): a frozen task refuses frozen -> blocked and frozen -> open
    without approval authority, auditing each attempt as a
    from='frozen', to='frozen', refused=<attempted status> entry — and
    the authorized approval close is the only writer of frozen -> open."""
    task: dict[str, Any] = {
        "task_id": TASK_ID,
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


# --- Round 6 (#1467): trusted review identity (bundle tree) -----------------


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


def test_r6_parity_distinct_members_without_account_id(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 1 (bundle tree): a Worker process (runtime
    member.name=worker) submits, then a Leader process
    (member.name=leader) reviews, neither supplying account_id. The
    review succeeds and both sides carry the runtime member identity,
    not the shared 'default' fallback."""
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


def test_r6_parity_trusted_identity_beats_payload_evasion(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 2 (bundle tree): the submitter's runtime member is
    'worker'; the SAME member (same runtime config) attempts the review
    carrying account_id=leader in the payload. The trusted identity
    wins: the reviewer resolves to 'leader:worker' and
    self_review_forbidden holds."""
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


def test_r6_parity_legacy_default_submitter_not_self_review(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 3 (bundle tree): a record written by an older build
    (submitted_by='worker:default') is reviewed by a trusted runtime
    member 'leader' -> allowed: an unknown submitter account is not a
    self-review match, and the review record carries the trusted
    identity."""
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="worker:default")

    _set_runtime_member(monkeypatch, tmp_path, {"name": "leader"}, "leader-config.json")
    result = _review(tmp_path, role="leader", verdict="pass")
    assert result["ok"] is True, result
    review = _task_meta(tmp_path)["review"]
    assert review["reviewer"] == "leader:leader"


# --- Round 7 (#1467): identity provenance (bundle tree) ---------------------


def test_r7_parity_trusted_default_member_self_review_forbidden(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 1 (bundle tree, the maintainer's case): a trusted Matrix
    member literally named 'default' submits as worker — the record is
    worker:default with submitted_by_source='runtime'; the operator
    reconfigures the SAME member's role to leader -> its review
    resolves to leader:default (reviewer_source='runtime') and is
    refused self_review_forbidden."""
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


def test_r7_parity_trusted_concrete_member_control_unchanged(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 2 (bundle tree, the control): the identical flow with
    member.name=worker still ends self_review_forbidden — the round-6
    behaviour is unchanged."""
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


def test_r7_parity_distinct_trusted_member_reviews_default_named_submitter(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 3 (bundle tree): a trusted member named 'default' submits;
    a DISTINCT trusted member (member.name=leader) reviews -> allowed,
    and the review record carries its own provenance
    (reviewer_source='runtime')."""
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


def test_r7_parity_legacy_default_submitter_without_source_still_allowed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 4 (bundle tree, legacy compatibility): a pre-round-7 record
    (submitted_by='worker:default', NO submitted_by_source marker) is
    reviewed by a trusted 'leader' -> allowed: a record without runtime
    provenance is treated as unknown."""
    _enforce_on(monkeypatch)
    _write_submitted_task(tmp_path, submitted_by="worker:default")

    _set_runtime_member(monkeypatch, tmp_path, {"name": "leader"}, "leader-config.json")
    result = _review(tmp_path, role="leader", verdict="pass")
    assert result["ok"] is True, result
    review = _task_meta(tmp_path)["review"]
    assert review["reviewer"] == "leader:leader"
    assert review["reviewer_source"] == "runtime"


def test_r7_parity_legacy_concrete_submitter_without_source_still_forbidden(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 5 (bundle tree, legacy compatibility): a pre-round-7 record
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


# --- Round 8 (#1467): the trusted decider (bundle tree) ---------------------


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


def test_r8_parity_display_field_never_decides(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 1 (bundle tree, the maintainer's pair): a human runtime
    role is configured for the SAME identity as the submitter — the
    server wrote resolvedActor='human:worker' from the trusted runtime
    member. Changing ONLY the caller-written display field (resolvedBy)
    must not change the decision: the trusted decider is the submitter,
    so self-approval holds in both spellings, the r1 acceptance stays
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


def test_r8_parity_distinct_human_decider_accepted(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 2 (bundle tree): a GENUINELY different human: the
    server-written decider (resolvedActor='human:other') differs from
    the submitter's identity in both spellings -> the r1 acceptance
    succeeds and the task is persisted as completed."""
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


def test_r8_parity_mxid_localpart_equals_runtimename(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 3 (bundle tree): one canonicalization on both sides of the
    comparison: the MXID localpart and the runtimeName of the same
    member are the same identity, so self-approval is refused in BOTH
    spellings — decider 'human:@worker:review.test' against the task
    account 'worker', and the reverse, decider 'human:worker' against
    '@worker:review.test'. (The display field carries an unrelated
    human's spelling in both: only the server-written decider is
    compared.)"""
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


def test_r8_parity_missing_server_marker_fails_closed(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, storage: None
) -> None:
    """Case 4 (bundle tree): fail closed: a resolved approval record
    with the human marker role, a human channel, and a human-looking
    resolvedBy — but NO server-written resolvedActor — is not an
    approval (a record that lacks the server marker can never carry
    authority): the r1 acceptance stays approval_required."""
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


# --- Round 11 (#1467) parity: request_attention / submit are whole-task
# writes — they must load the authoritative task and fail closed. ----------


@pytest.fixture
def shared_remote(
    monkeypatch: pytest.MonkeyPatch, storage: None
) -> dict[str, Any]:
    """Two-plane shared-storage model (bundle copy): a hidden 'remote' meta
    plus the local meta.json the action reads; a failed pull is scriptable."""
    planes: dict[str, Any] = {"remote": None, "committed": None, "pull_ok": True}

    def fake_pull(_arguments: Any, task_id: Any, **_kw: Any) -> bool:
        if not planes["pull_ok"]:
            return False
        if planes["remote"] is not None:
            server._write_json(
                server._task_state_path(_arguments, str(task_id)), planes["remote"]
            )
        return True

    def fake_sync(_arguments: Any, task_id: Any, **kw: Any) -> bool:
        planes["committed"] = server._read_json(
            server._task_state_path(_arguments, str(task_id))
        )
        return True

    monkeypatch.setattr(server, "_pull_task", fake_pull)
    monkeypatch.setattr(server, "_sync_task", fake_sync)
    return planes


def _r11_frozen_record() -> dict[str, Any]:
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


def _r11_freeze(task: dict[str, Any]) -> dict[str, Any]:
    frozen = dict(task)
    frozen["decision_status"] = "frozen"
    frozen["attention"] = [_r11_frozen_record()]
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


def test_r11_parity_request_attention_preserves_remote_frozen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, shared_remote: dict[str, Any],
    attention_env: None,
) -> None:
    """The maintainer's repro in the bundle tree: an ordinary question must
    not erase a persisted freeze, approval record or history."""
    _enforce_on(monkeypatch)
    cached = _write_submitted_task(tmp_path, submitted_by="worker:alice")
    frozen = _r11_freeze(cached)
    shared_remote["remote"] = frozen

    result = _request_attention(tmp_path, kind="other", question="Operator note?")

    assert result["ok"] is True
    committed = shared_remote["committed"]
    assert committed is not None
    assert committed["decision_status"] == "frozen"
    assert committed["decision_status_history"] == frozen["decision_status_history"]
    assert committed["attention"][0] == _r11_frozen_record()
    assert committed["attention"][-1]["question"] == "Operator note?"


def test_r11_parity_request_attention_pull_failure_refuses_stale_copy(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, shared_remote: dict[str, Any],
    attention_env: None,
) -> None:
    _enforce_on(monkeypatch)
    cached = _write_submitted_task(tmp_path, submitted_by="worker:alice")
    shared_remote["remote"] = _r11_freeze(cached)
    shared_remote["pull_ok"] = False

    result = _request_attention(tmp_path, kind="other", question="Operator note?")

    assert result["ok"] is False
    assert "stale local copy" in result["error"]
    assert shared_remote["committed"] is None


def test_r11_parity_submit_preserves_remote_frozen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, shared_remote: dict[str, Any],
    attention_env: None,
) -> None:
    _enforce_on(monkeypatch)
    cached = _write_submitted_task(tmp_path, submitted_by="worker:alice")
    frozen = _r11_freeze(cached)
    shared_remote["remote"] = frozen

    result = _submit(tmp_path)

    assert result["ok"] is True
    committed = shared_remote["committed"]
    assert committed["status"] == "submitted"
    assert committed["decision_status"] == "frozen"
    assert committed["decision_status_history"] == frozen["decision_status_history"]


def test_r11_parity_submit_pull_failure_refuses_stale_copy(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, shared_remote: dict[str, Any],
    attention_env: None,
) -> None:
    _enforce_on(monkeypatch)
    cached = _write_submitted_task(tmp_path, submitted_by="worker:alice")
    shared_remote["remote"] = _r11_freeze(cached)
    shared_remote["pull_ok"] = False

    result = _submit(tmp_path)

    assert result["ok"] is False
    assert "stale local copy" in result["error"]
    assert shared_remote["committed"] is None


# --- Round 12 (#1467): first delegation with an absent remote task ----------
# The source-tree module is imported lazily (it binds no `mcp` package at
# import time), so this file's bundle-tree `mcp` stays the single identity
# in this process.


def _load_first_delegation_module() -> Any:
    path = (
        Path(__file__).resolve().parents[1]
        / "teamharness"
        / "mcp"
        / "test_first_delegation_absent_prefix.py"
    )
    spec = importlib.util.spec_from_file_location("legacy_first_delegation_r12", path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_r12_parity_first_delegation_with_an_absent_prefix(
    bound_server: None, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A brand-new task has no remote prefix: the copy must still create it
    (the failed pull is confirmed absent, not treated as a storage error)."""
    _load_first_delegation_module().test_first_delegation_succeeds_with_an_absent_remote_prefix(
        server, tmp_path, monkeypatch
    )


def test_r12_parity_existing_task_failed_pull_still_fails_closed(
    bound_server: None, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _load_first_delegation_module().test_existing_task_with_a_failed_pull_still_fails_closed(
        server, tmp_path, monkeypatch
    )


def test_r12_parity_storage_outage_fails_closed(
    bound_server: None, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _load_first_delegation_module().test_storage_outage_fails_closed(
        server, tmp_path, monkeypatch
    )
