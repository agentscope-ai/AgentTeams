# -*- coding: utf-8 -*-
"""TH-20 approval three items (审批三项): bounded wait, audit pair, callId dedup.

The approval gate is the TASK-DECISION layer (the DSH comparison audit
fixed the layering: ours is the task-decision layer, not the tool-call
layer), and its three gaps close here:

① bounded wait — an approval ask carries a deadline (explicit
   ``deadline_at``, ``wait_seconds``, or the 30-minute default). When
   the deadline lapses without a human decision the ask transitions to
   ``unavailable``: the wait is bounded (never permanently frozen) and
   the lapse is never an approval (fail closed — no auto-release, no
   late decision after the deadline; re-ask with a fresh callId).

② asked/decided audit pair — every ask appends an ``asked`` event and
   every terminal outcome (approved / denied / unavailable) appends the
   ``decided`` event carrying the same callId, so the pair is
   correlatable by callId. The journal is a SEPARATE append-only file
   (``approval_audit.jsonl`` in the task directory), stored apart from
   the task meta attention list and the conversation transcript.

③ callId dedup — the callId is the approval attempt identity: assigned
   at ask time, carried by both audit events, and the dedup key of
   resolve. A second resolve with the same callId is an idempotent
   replay (same decision) or a rejection (conflicting decision), never
   a second decision.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

MCP_DIR = Path(__file__).resolve().parents[2] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402

approval = server.approval

T0 = "2026-11-04T10:00:00Z"
T_DEADLINE = "2026-11-04T10:30:00Z"  # T0 + the 30-minute default wait
T_BEFORE = "2026-11-04T10:29:59Z"  # one second before the deadline
T_LATE = "2026-11-04T10:31:00Z"  # after the deadline


def _task(task_id: str = "t-20") -> dict:
    return {"task_id": task_id, "status": "in_progress"}


def _ask(tmp_path: Path, task: dict | None = None, task_id: str = "t-20", **kwargs):
    task = task if task is not None else _task(task_id)
    result = approval.ask_approval(task, tmp_path, task_id, now=T0, **kwargs)
    return task, result


def _journal(tmp_path: Path, task_id: str = "t-20") -> list:
    return approval.read_approval_audit(tmp_path, task_id)


def _decided_events(tmp_path: Path, task_id: str = "t-20") -> list:
    return [e for e in _journal(tmp_path, task_id) if e.get("event") == "decided"]


class TestBoundedWait:
    """① bounded wait timeout -> unavailable (no permanent freeze, no
    auto-release)."""

    def test_default_deadline_is_bounded(self, tmp_path):
        task, res = _ask(tmp_path)
        assert res["deadline_at"] == T_DEADLINE
        state = approval.approval_state(task, now=T0)
        assert state["status"] == "pending"
        assert state["expired"] is False

    def test_wait_seconds_sets_the_deadline(self, tmp_path):
        task, res = _ask(tmp_path, wait_seconds=300)
        assert res["deadline_at"] == "2026-11-04T10:05:00Z"

    def test_explicit_deadline_wins_over_default(self, tmp_path):
        task, res = _ask(tmp_path, deadline_at="2026-11-04T10:05:00Z")
        assert res["deadline_at"] == "2026-11-04T10:05:00Z"
        approval.check_approval_bounded_wait(
            task, tmp_path, "t-20", now="2026-11-04T10:06:00Z"
        )
        assert task["attention"][-1]["approvalStatus"] == "unavailable"

    def test_check_before_deadline_keeps_pending(self, tmp_path):
        task, res = _ask(tmp_path)
        transitions = approval.check_approval_bounded_wait(
            task, tmp_path, "t-20", now=T_BEFORE
        )
        assert transitions == []
        assert approval.approval_state(task, now=T_BEFORE)["status"] == "pending"
        # No decision of any kind was recorded:
        assert _decided_events(tmp_path) == []

    def test_lapse_marks_unavailable(self, tmp_path):
        task, res = _ask(tmp_path)
        transitions = approval.check_approval_bounded_wait(
            task, tmp_path, "t-20", now=T_LATE
        )
        assert len(transitions) == 1
        assert transitions[0]["call_id"] == res["call_id"]
        assert transitions[0]["unavailable_at"] == T_LATE
        record = task["attention"][-1]
        assert record["approvalStatus"] == "unavailable"
        assert record["unavailableAt"] == T_LATE

    def test_lapse_is_never_an_approval(self, tmp_path):
        task, res = _ask(tmp_path)
        approval.check_approval_bounded_wait(task, tmp_path, "t-20", now=T_LATE)
        record = task["attention"][-1]
        # Fail closed: the loop is NOT resolved and nothing approved —
        # the wait simply ended with "approver unavailable".
        assert record.get("resolved") is False
        assert record["approvalStatus"] != "approved"
        assert "resolvedBy" not in record
        assert "resolvedVia" not in record
        decided = _decided_events(tmp_path)
        assert len(decided) == 1
        assert decided[0]["outcome"] == "unavailable"
        assert decided[0]["reason"] == "bounded_wait_expired"
        assert decided[0]["callId"] == res["call_id"]

    def test_check_is_idempotent_after_lapse(self, tmp_path):
        task, res = _ask(tmp_path)
        approval.check_approval_bounded_wait(task, tmp_path, "t-20", now=T_LATE)
        again = approval.check_approval_bounded_wait(task, tmp_path, "t-20", now=T_LATE)
        assert again == []
        # One lapse, one decided event — never two:
        assert len(_decided_events(tmp_path)) == 1

    def test_resolve_after_deadline_is_rejected_and_lapses(self, tmp_path):
        task, res = _ask(tmp_path)
        with pytest.raises(approval.ApprovalGuardError) as excinfo:
            approval.resolve_approval(
                task, tmp_path, "t-20", res["call_id"], "approve", now=T_LATE
            )
        assert excinfo.value.error_code == "approval_unavailable"
        # The resolve path evaluates the deadline itself: the record
        # lapsed (and was audited) before the rejection was raised.
        assert task["attention"][-1]["approvalStatus"] == "unavailable"
        entries = _journal(tmp_path)
        assert [e["event"] for e in entries] == ["asked", "decided"]
        assert entries[1]["outcome"] == "unavailable"

    def test_approval_state_reports_expired_pending(self, tmp_path):
        task, res = _ask(tmp_path)
        # Past the deadline but the check has not run yet: the state is
        # still pending AND flagged expired — a late gate read sees the
        # true position, not a zombie "waiting forever".
        state = approval.approval_state(task, now=T_LATE)
        assert state["status"] == "pending"
        assert state["expired"] is True
        approval.check_approval_bounded_wait(task, tmp_path, "t-20", now=T_LATE)
        assert approval.approval_state(task, now=T_LATE)["status"] == "unavailable"


class TestAuditPair:
    """② asked/decided audit events: paired, correlatable by callId,
    stored apart from the task meta / transcript."""

    def test_ask_appends_the_asked_event(self, tmp_path):
        task, res = _ask(tmp_path, question="may I deploy?")
        entries = _journal(tmp_path)
        assert len(entries) == 1
        asked = entries[0]
        assert asked["event"] == "asked"
        assert asked["seq"] == 1
        assert asked["callId"] == res["call_id"]
        assert asked["taskId"] == "t-20"
        assert asked["attempt"] == 1
        assert asked["deadlineAt"] == T_DEADLINE
        assert asked["at"] == T0

    def test_approve_closes_the_pair(self, tmp_path):
        task, res = _ask(tmp_path)
        approval.resolve_approval(
            task, tmp_path, "t-20", res["call_id"], "approve",
            decided_by="@admin:example.test", via="im_reply", now=T_BEFORE,
        )
        entries = _journal(tmp_path)
        assert [e["event"] for e in entries] == ["asked", "decided"]
        decided = entries[1]
        assert decided["seq"] == 2
        assert decided["callId"] == res["call_id"]
        assert decided["outcome"] == "approved"
        assert decided["decidedBy"] == "@admin:example.test"
        assert decided["via"] == "im_reply"

    def test_deny_closes_the_pair(self, tmp_path):
        task, res = _ask(tmp_path)
        approval.resolve_approval(
            task, tmp_path, "t-20", res["call_id"], "deny", now=T_BEFORE
        )
        entries = _journal(tmp_path)
        assert entries[1]["event"] == "decided"
        assert entries[1]["callId"] == res["call_id"]
        assert entries[1]["outcome"] == "denied"

    def test_lapse_closes_the_pair(self, tmp_path):
        task, res = _ask(tmp_path)
        approval.check_approval_bounded_wait(task, tmp_path, "t-20", now=T_LATE)
        entries = _journal(tmp_path)
        assert [e["event"] for e in entries] == ["asked", "decided"]
        assert entries[1]["callId"] == res["call_id"]
        assert entries[1]["outcome"] == "unavailable"

    def test_audit_pairs_report_paired_journal(self, tmp_path):
        task, res = _ask(tmp_path)
        approval.resolve_approval(
            task, tmp_path, "t-20", res["call_id"], "approve", now=T_BEFORE
        )
        report = approval.audit_pairs(_journal(tmp_path))
        assert set(report["pairs"]) == {res["call_id"]}
        assert report["pairs"][res["call_id"]]["asked"]["callId"] == res["call_id"]
        assert report["pairs"][res["call_id"]]["decided"]["outcome"] == "approved"
        assert report["unpaired_asked"] == []
        assert report["unpaired_decided"] == []
        assert report["duplicate_decided"] == {}

    def test_audit_pairs_flags_orphans(self, tmp_path):
        entries = [
            {"seq": 1, "event": "asked", "callId": "call_a", "at": T0},
            {"seq": 2, "event": "decided", "callId": "call_a",
             "outcome": "approved", "at": T_BEFORE},
            # Asked, never decided (an open or abandoned wait):
            {"seq": 3, "event": "asked", "callId": "call_b", "at": T0},
            # Decided without any ask (corrupt or foreign event):
            {"seq": 4, "event": "decided", "callId": "call_c",
             "outcome": "denied", "at": T_BEFORE},
        ]
        report = approval.audit_pairs(entries)
        assert set(report["pairs"]) == {"call_a"}
        assert report["unpaired_asked"] == ["call_b"]
        assert report["unpaired_decided"] == ["call_c"]
        assert report["duplicate_decided"] == {}

    def test_audit_pairs_flags_duplicate_decided(self, tmp_path):
        # Two decided events for one callId is a dedup violation the
        # pairing report must surface, not average away:
        entries = [
            {"seq": 1, "event": "asked", "callId": "call_a", "at": T0},
            {"seq": 2, "event": "decided", "callId": "call_a",
             "outcome": "approved", "at": T_BEFORE},
            {"seq": 3, "event": "decided", "callId": "call_a",
             "outcome": "approved", "at": T_LATE},
        ]
        report = approval.audit_pairs(entries)
        assert "call_a" in report["pairs"]
        assert report["duplicate_decided"] == {"call_a": 2}

    def test_journal_is_a_separate_file_from_task_meta(self, tmp_path):
        task, res = _ask(tmp_path)
        approval.resolve_approval(
            task, tmp_path, "t-20", res["call_id"], "approve", now=T_BEFORE
        )
        # The audit pair exists ONLY in the separate append-only
        # journal — the task meta (the loop record's home, next to the
        # conversation) carries no audit events, seq counters, or
        # asked/decided markers.
        for record in task.get("attention", []):
            assert "event" not in record
            assert "seq" not in record
            assert "asked" not in record
            assert "decided" not in record
        journal = approval.approval_audit_path(tmp_path, "t-20")
        assert journal == (
            tmp_path / "shared" / "tasks" / "t-20" / "approval_audit.jsonl"
        )
        assert journal.is_file()
        lines = journal.read_text(encoding="utf-8").strip().splitlines()
        assert len(lines) == 2
        # Each line is a standalone JSON object (append-only journal):
        assert [json.loads(line)["event"] for line in lines] == ["asked", "decided"]

    def test_read_approval_audit_skips_malformed_lines(self, tmp_path):
        task, res = _ask(tmp_path)
        journal = approval.approval_audit_path(tmp_path, "t-20")
        with journal.open("a", encoding="utf-8") as handle:
            handle.write("{not json}\n")
        entries = approval.read_approval_audit(tmp_path, "t-20")
        assert len(entries) == 1
        assert entries[0]["event"] == "asked"


class TestCallIdDedup:
    """③ callId prevents a double resolve: the same callId resolved
    twice is an idempotent replay or a rejection, never a second
    decision."""

    def test_second_resolve_same_call_id_is_idempotent(self, tmp_path):
        task, res = _ask(tmp_path)
        first = approval.resolve_approval(
            task, tmp_path, "t-20", res["call_id"], "approve",
            decided_by="@admin:example.test", now=T_BEFORE,
        )
        assert first["duplicate"] is False
        first_decided_at = task["attention"][-1]["decidedAt"]
        # The retry (same callId, same decision — e.g. after a lost
        # response) replays the original decision:
        second = approval.resolve_approval(
            task, tmp_path, "t-20", res["call_id"], "approve",
            decided_by="@admin:example.test", now=T_LATE,
        )
        assert second["duplicate"] is True
        assert second["outcome"] == "approved"
        assert second["decided"]["seq"] == 2  # the ORIGINAL event
        # No second decision anywhere:
        assert [e["event"] for e in _journal(tmp_path)] == ["asked", "decided"]
        assert task["attention"][-1]["decidedAt"] == first_decided_at

    def test_conflicting_retry_same_call_id_is_rejected(self, tmp_path):
        task, res = _ask(tmp_path)
        approval.resolve_approval(
            task, tmp_path, "t-20", res["call_id"], "approve", now=T_BEFORE
        )
        with pytest.raises(approval.ApprovalGuardError) as excinfo:
            approval.resolve_approval(
                task, tmp_path, "t-20", res["call_id"], "deny", now=T_BEFORE
            )
        assert excinfo.value.error_code == "approval_already_decided"
        # First decision wins; the record and the journal are untouched:
        assert task["attention"][-1]["approvalStatus"] == "approved"
        assert [e["event"] for e in _journal(tmp_path)] == ["asked", "decided"]
        assert _journal(tmp_path)[1]["outcome"] == "approved"

    def test_unknown_call_id_is_rejected(self, tmp_path):
        task, res = _ask(tmp_path)
        with pytest.raises(approval.ApprovalGuardError) as excinfo:
            approval.resolve_approval(
                task, tmp_path, "t-20", "call-other", "approve", now=T_BEFORE
            )
        assert excinfo.value.error_code == "approval_not_found"
        # Nothing was decided:
        assert [e["event"] for e in _journal(tmp_path)] == ["asked"]
        assert approval.approval_state(task, now=T_BEFORE)["status"] == "pending"

    def test_empty_call_id_is_rejected(self, tmp_path):
        task, res = _ask(tmp_path)
        with pytest.raises(approval.ApprovalGuardError) as excinfo:
            approval.resolve_approval(
                task, tmp_path, "t-20", "", "approve", now=T_BEFORE
            )
        assert excinfo.value.error_code == "invalid_call_id"

    def test_invalid_decision_is_rejected(self, tmp_path):
        task, res = _ask(tmp_path)
        with pytest.raises(approval.ApprovalGuardError) as excinfo:
            approval.resolve_approval(
                task, tmp_path, "t-20", res["call_id"], "maybe", now=T_BEFORE
            )
        assert excinfo.value.error_code == "invalid_decision"
        assert [e["event"] for e in _journal(tmp_path)] == ["asked"]

    def test_late_resolve_after_lapse_is_rejected(self, tmp_path):
        task, res = _ask(tmp_path)
        approval.check_approval_bounded_wait(task, tmp_path, "t-20", now=T_LATE)
        with pytest.raises(approval.ApprovalGuardError) as excinfo:
            approval.resolve_approval(
                task, tmp_path, "t-20", res["call_id"], "approve", now=T_LATE
            )
        assert excinfo.value.error_code == "approval_unavailable"

    def test_reask_after_unavailable_starts_a_fresh_pair(self, tmp_path):
        task, first = _ask(tmp_path)
        approval.check_approval_bounded_wait(task, tmp_path, "t-20", now=T_LATE)
        # The bounded wait ended; asking again is a NEW attempt with a
        # fresh callId and a fresh deadline (the old pair stays intact):
        second = approval.ask_approval(task, tmp_path, "t-20", now=T_LATE)
        assert second["call_id"] != first["call_id"]
        assert second["record"]["attempt"] == 2
        assert second["deadline_at"] == "2026-11-04T11:01:00Z"
        assert approval.approval_state(task, now=T_LATE)["status"] == "pending"
        entries = _journal(tmp_path)
        assert [e["event"] for e in entries] == ["asked", "decided", "asked"]
        assert entries[2]["callId"] == second["call_id"]
        approval.resolve_approval(
            task, tmp_path, "t-20", second["call_id"], "approve",
            now="2026-11-04T11:00:00Z",
        )
        report = approval.audit_pairs(_journal(tmp_path))
        assert set(report["pairs"]) == {first["call_id"], second["call_id"]}
        assert report["unpaired_asked"] == []
        assert report["unpaired_decided"] == []
        assert report["duplicate_decided"] == {}

    def test_idempotent_ask_same_call_id(self, tmp_path):
        task, first = _ask(tmp_path)
        again = approval.ask_approval(
            task, tmp_path, "t-20", call_id=first["call_id"], now=T0
        )
        assert again["duplicate"] is True
        assert again["call_id"] == first["call_id"]
        approvals = [
            r for r in task["attention"] if r.get("kind") == "approval"
        ]
        assert len(approvals) == 1
        asked = [e for e in _journal(tmp_path) if e["event"] == "asked"]
        assert len(asked) == 1

    def test_legacy_record_without_call_id_is_out_of_scope(self, tmp_path):
        # Pre-TH-20 records (raised by the legacy request_attention
        # path, no callId) keep their existing behaviour: the bounded
        # wait and the audit pair apply to callId-carrying records
        # only, and they are never force-migrated.
        task = _task()
        task["attention"] = [
            {"kind": "approval", "question": "legacy", "attempt": 1,
             "requestedAt": T0, "resolved": False}
        ]
        transitions = approval.check_approval_bounded_wait(
            task, tmp_path, "t-20", now=T_LATE
        )
        assert transitions == []
        assert task["attention"][0].get("approvalStatus") is None
        assert _journal(tmp_path) == []
