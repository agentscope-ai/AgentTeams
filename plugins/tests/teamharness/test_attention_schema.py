# -*- coding: utf-8 -*-
"""S0 + S4a of the request_attention UX line (#1394, ported from #1418).

S0 — record fields (all optional, backward compatible): ``timeoutAt``
(canonical deadline; ``expiresAt`` stays a compatibility alias for the
#1449/#1434 readers), ``options[]`` with the #1449 strict validation
plus ``description``, and the resolve provenance (``resolvedVia`` /
``resolvedBy`` / ``resolveEvidence`` / ``resolvedAt``).
S4a — notification outcome persistence: one ``notification_log`` entry
per logical attempt with ``room_accepted`` (Matrix room acceptance) and
a ``channel_accepted`` that is never inferred from room acceptance.

W1-a2 (issue #1451 P6): the implementation lives in
mcp/lifecycle/taskflow.py; the tests still enter through the assembled
server (refs proxies keep server-attribute monkeypatches visible) and
patch taskflow's direct imports on the taskflow module itself.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[2] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


def _sent(**extra: Any) -> dict[str, Any]:
    result: dict[str, Any] = {"sent": True, "eventId": "$evt1"}
    result.update(extra)
    return result


def _arguments(workspace: Path, **payload: Any) -> dict[str, Any]:
    return {
        "role": "worker",
        "action": "request_attention",
        "workspaceDir": str(workspace),
        "payload": payload,
    }


def _write_task(workspace: Path, task_id: str, meta: dict[str, Any]) -> None:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")


def _read_task(workspace: Path, task_id: str) -> dict[str, Any]:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    return json.loads(path.read_text(encoding="utf-8"))


def _prepare(
    workspace: Path,
    monkeypatch: pytest.MonkeyPatch,
    *,
    notify=None,
) -> tuple[Path, str]:
    task_id = "attn-task-1"
    _write_task(
        workspace,
        task_id,
        {
            "task_id": task_id,
            "status": "in_progress",
            "project_id": "",
            "room_id": "!room:matrix.local",
        },
    )
    # Proxy names resolve against the assembled server's globals.
    monkeypatch.setattr(server, "_pull_project", lambda *a, **k: True)
    monkeypatch.setattr(server, "_sync_task", lambda *a, **k: True)
    monkeypatch.setattr(
        server, "_validate_assignee_membership", lambda *a, **k: {"ok": True}
    )
    # Direct imports inside mcp/lifecycle/taskflow.py: patch the module
    # namespace that actually performs the call.
    monkeypatch.setattr(
        server.taskflow, "_team_leader_matrix_id", lambda: "@lead:matrix.local"
    )
    monkeypatch.setattr(
        server.taskflow, "_team_human_matrix_ids", lambda: ["@luo:matrix.local"]
    )
    if notify is not None:
        monkeypatch.setattr(server.taskflow, "_send_attention_notification", notify)
    return workspace, task_id


def test_record_carries_timeout_and_options(tmp_path, monkeypatch):
    workspace, task_id = _prepare(tmp_path, monkeypatch, notify=lambda *a, **k: _sent())
    result = server._taskflow(
        _arguments(
            workspace,
            taskId=task_id,
            kind="decision",
            question="Pick a database",
            timeoutAt="2026-10-24T00:00:00Z",
            # #1449 validation: every option entry carries a strict
            # string id + label (the original PR defaulted a missing id
            # to its index; that relaxation must not come back).
            options=[
                {"label": "Postgres", "id": "pg"},
                {"label": "SQLite", "id": "sqlite", "description": "local only"},
            ],
        )
    )
    assert result["ok"] is True, result
    record = result["task"]["attention"][-1]
    assert record["timeoutAt"] == "2026-10-24T00:00:00Z"
    assert record["options"] == [
        {"id": "pg", "label": "Postgres"},
        {"id": "sqlite", "label": "SQLite", "description": "local only"},
    ]
    # The caller used the canonical field: no alias key on the record.
    assert "expiresAt" not in record


def test_expires_at_alias_writes_canonical_timeout(tmp_path, monkeypatch):
    workspace, task_id = _prepare(tmp_path, monkeypatch, notify=lambda *a, **k: _sent())
    result = server._taskflow(
        _arguments(
            workspace,
            taskId=task_id,
            kind="approval",
            question="Approve?",
            expiresAt="2026-10-25T00:00:00Z",
        )
    )
    assert result["ok"] is True
    record = result["task"]["attention"][-1]
    # The canonical key is written even for alias callers ...
    assert record["timeoutAt"] == "2026-10-25T00:00:00Z"
    # ... and the alias is echoed with the same value for #1449/#1434 readers.
    assert record["expiresAt"] == record["timeoutAt"]


def test_expires_at_and_timeout_at_must_agree(tmp_path, monkeypatch):
    workspace, task_id = _prepare(tmp_path, monkeypatch, notify=lambda *a, **k: _sent())
    ok = server._taskflow(
        _arguments(
            workspace,
            taskId=task_id,
            kind="approval",
            question="Approve?",
            timeoutAt="2026-10-25T00:00:00Z",
            expiresAt="2026-10-25T00:00:00Z",
        )
    )
    assert ok["ok"] is True
    record = ok["task"]["attention"][-1]
    assert record["timeoutAt"] == record["expiresAt"]
    # Conflicting deadlines are rejected before any write.
    conflict = server._taskflow(
        _arguments(
            workspace,
            taskId=task_id,
            kind="escalation",
            question="Escalate?",
            timeoutAt="2026-10-25T00:00:00Z",
            expiresAt="2026-10-26T00:00:00Z",
        )
    )
    assert conflict["ok"] is False
    assert "timeoutAt" in conflict["error"]
    # Rejections happen before any write: nothing landed on disk.
    on_disk = _read_task(workspace, task_id)
    assert not any(
        it.get("kind") == "escalation" for it in on_disk.get("attention", [])
    )


def test_invalid_deadline_rejected(tmp_path, monkeypatch):
    workspace, task_id = _prepare(tmp_path, monkeypatch)

    def run(kind: str, **deadline: Any) -> dict[str, Any]:
        return server._taskflow(
            _arguments(workspace, taskId=task_id, kind=kind, question="Q", **deadline)
        )

    # _taskflow turns ValueError into a structured error response.
    bad_timeout = run("decision", timeoutAt="tomorrow")
    assert bad_timeout["ok"] is False
    assert "ISO-8601" in bad_timeout["error"]

    bad_alias = run("escalation", expiresAt="tomorrow")
    assert bad_alias["ok"] is False
    assert "ISO-8601" in bad_alias["error"]


def test_option_validation_not_regressed(tmp_path, monkeypatch):
    workspace, task_id = _prepare(tmp_path, monkeypatch)

    def run(kind: str, **extra: Any) -> dict[str, Any]:
        return server._taskflow(
            _arguments(workspace, taskId=task_id, kind=kind, question="Q", **extra)
        )

    duplicate = run(
        "decision",
        options=[{"id": "x", "label": "L1"}, {"id": "x", "label": "L2"}],
    )
    assert duplicate["ok"] is False
    assert "options ids must be unique" in duplicate["error"]

    non_string_id = run("escalation", options=[{"id": 1, "label": "L"}])
    assert non_string_id["ok"] is False
    assert "options" in non_string_id["error"]

    non_string_description = run(
        "other", options=[{"id": "a", "label": "L", "description": 7}]
    )
    assert non_string_description["ok"] is False
    assert "description" in non_string_description["error"]


def test_notification_log_records_success(tmp_path, monkeypatch):
    def notify(arguments, **kwargs):
        return _sent()

    workspace, task_id = _prepare(tmp_path, monkeypatch, notify=notify)
    result = server._taskflow(
        _arguments(workspace, taskId=task_id, kind="approval", question="Approve?")
    )
    assert result["ok"] is True
    entry = result["task"]["attention"][-1]["notification_log"][-1]
    assert entry["attempt"] == 1
    assert entry["room_accepted"] is True
    # The per-recipient receipt is not reported by the room adapter: it
    # must stay null, never inferred from room acceptance.
    assert entry["channel_accepted"] is None
    assert entry["retries"] == []
    assert "error" not in entry


def test_notification_log_records_failure(tmp_path, monkeypatch):
    def notify(arguments, **kwargs):
        return {"sent": False, "error": "Matrix API error: HTTP 403"}

    workspace, task_id = _prepare(tmp_path, monkeypatch, notify=notify)
    result = server._taskflow(
        _arguments(workspace, taskId=task_id, kind="approval", question="Approve?")
    )
    assert result["ok"] is True
    entry = result["task"]["attention"][-1]["notification_log"][-1]
    assert entry["room_accepted"] is False
    assert "HTTP 403" in entry["error"]
    # The record is persisted even though the send failed.
    on_disk = _read_task(workspace, task_id)
    record = [it for it in on_disk["attention"] if it.get("kind") == "approval"][0]
    assert record["notification_log"][-1]["room_accepted"] is False
    assert "eventId" not in record


def test_resolve_records_provenance(tmp_path, monkeypatch):
    workspace, task_id = _prepare(tmp_path, monkeypatch, notify=lambda *a, **k: _sent())
    server._taskflow(
        _arguments(workspace, taskId=task_id, kind="decision", question="Pick one")
    )
    resolved = server._taskflow(
        _arguments(
            workspace,
            taskId=task_id,
            kind="decision",
            question="Pick one",
            resolved=True,
            resolvedVia="im_reply",
            resolvedBy="@luo:matrix.local",
            resolveEvidence={"messageId": "$m1", "note": "answered in room"},
        )
    )
    assert resolved["ok"] is True
    record = resolved["task"]["attention"][-1]
    assert record["resolved"] is True
    assert record["resolvedVia"] == "im_reply"
    assert record["resolvedBy"] == "@luo:matrix.local"
    assert record["resolveEvidence"]["messageId"] == "$m1"
    assert record["resolvedAt"]

    # The default provenance for a plain close is the tool flip.
    server._taskflow(
        _arguments(workspace, taskId=task_id, kind="escalation", question="Escalate?")
    )
    default_close = server._taskflow(
        _arguments(
            workspace,
            taskId=task_id,
            kind="escalation",
            question="Escalate?",
            resolved=True,
        )
    )
    assert default_close["ok"] is True
    esc = [
        it for it in default_close["task"]["attention"] if it.get("kind") == "escalation"
    ][0]
    assert esc["resolvedVia"] == "tool"
    assert "resolvedBy" not in esc
    assert "resolveEvidence" not in esc

    # An invalid resolvedVia is rejected before any write.
    bad = server._taskflow(
        _arguments(
            workspace,
            taskId=task_id,
            kind="decision",
            question="Pick one",
            resolved=True,
            resolvedVia="telepathy",
        )
    )
    assert bad["ok"] is False
    assert "resolvedVia" in bad["error"]


def test_legacy_call_keeps_legacy_shape(tmp_path, monkeypatch):
    workspace, task_id = _prepare(tmp_path, monkeypatch, notify=lambda *a, **k: _sent())
    result = server._taskflow(
        _arguments(workspace, taskId=task_id, kind="approval", question="Ship it?")
    )
    assert result["ok"] is True
    record = result["task"]["attention"][-1]
    legacy_keys = {"kind", "question", "attempt", "requestedAt", "resolved", "eventId"}
    # S4a always records the notification outcome; the optional S0
    # fields stay absent.
    assert set(record.keys()) <= legacy_keys | {"notification_log"}
    assert not ({"options", "suggested", "timeoutAt", "expiresAt"} & set(record.keys()))


def _sync_double(
    workspace: Path, outcomes: list[bool]
) -> tuple[Any, list[dict[str, Any]]]:
    """A snapshotting _sync_task stand-in.

    ``outcomes`` is the per-push return value by call index (the last
    one repeats once exhausted). Every push snapshots the on-disk task
    meta as it stands at that moment, so a test can assert what the
    shared storage actually received.
    """
    snapshots: list[dict[str, Any]] = []
    calls = {"n": 0}

    def sync(arguments: Any, task_id: str, **kwargs: Any) -> bool:
        index = calls["n"]
        calls["n"] += 1
        ok = outcomes[index] if index < len(outcomes) else outcomes[-1]
        snapshots.append(_read_task(workspace, task_id))
        return ok

    return sync, snapshots


def test_notification_outcome_is_pushed_after_send(tmp_path, monkeypatch):
    workspace, task_id = _prepare(tmp_path, monkeypatch, notify=lambda *a, **k: _sent())
    sync, snapshots = _sync_double(workspace, [True])
    monkeypatch.setattr(server, "_sync_task", sync)
    result = server._taskflow(
        _arguments(workspace, taskId=task_id, kind="approval", question="Approve?")
    )
    assert result["ok"] is True, result
    assert result["synced"] is True, result
    # Two pushes: the pre-send push (record without the send outcome)
    # and the post-send push that must carry it.
    assert len(snapshots) == 2, snapshots
    first_push = snapshots[0]["attention"][-1]
    assert "eventId" not in first_push
    assert "notification_log" not in first_push
    pushed = snapshots[1]["attention"][-1]
    assert pushed["eventId"] == "$evt1"
    assert pushed["notification_log"][-1]["room_accepted"] is True


def test_post_send_sync_failure_is_retryable_without_resend(tmp_path, monkeypatch):
    send_calls: list[Any] = []

    def notify(arguments, **kwargs):
        send_calls.append(kwargs.get("attempt"))
        return _sent()

    workspace, task_id = _prepare(tmp_path, monkeypatch, notify=notify)
    # Push 1 (pre-send) succeeds, push 2 (post-send) fails; pushes after
    # storage recovery succeed.
    sync, snapshots = _sync_double(workspace, [True, False, True])
    monkeypatch.setattr(server, "_sync_task", sync)
    first = server._taskflow(
        _arguments(workspace, taskId=task_id, kind="approval", question="Approve?")
    )
    assert first["ok"] is False, first
    assert first["retryable"] is True, first
    assert first["synced"] is False, first
    # The eventId is recorded locally and is never rolled back on the
    # failed push ...
    on_disk = _read_task(workspace, task_id)
    record = on_disk["attention"][-1]
    assert record["eventId"] == "$evt1"
    assert record["notification_log"][-1]["room_accepted"] is True
    # ... and the retry only re-syncs: the notifier is not called again.
    second = server._taskflow(
        _arguments(workspace, taskId=task_id, kind="approval", question="Approve?")
    )
    assert second["ok"] is True, second
    assert second["synced"] is True, second
    assert (second.get("attention") or {}).get("reused") is True, second
    assert send_calls == [1], send_calls
    # The recovered push carries the outcome the failed one left behind.
    final_push = snapshots[-1]["attention"][-1]
    assert final_push["eventId"] == "$evt1"
    assert final_push["notification_log"][-1]["room_accepted"] is True


def test_retry_folds_into_single_log_entry(tmp_path, monkeypatch):
    outcomes = {"send": False}

    def notify(arguments, **kwargs):
        if outcomes["send"]:
            return _sent()
        return {"sent": False, "error": "Matrix API error: HTTP 503"}

    workspace, task_id = _prepare(tmp_path, monkeypatch, notify=notify)
    first = server._taskflow(
        _arguments(workspace, taskId=task_id, kind="approval", question="Approve?")
    )
    assert first["ok"] is True, first
    failed = first["task"]["attention"][-1]["notification_log"][-1]
    assert failed["room_accepted"] is False
    assert "HTTP 503" in failed["error"]

    outcomes["send"] = True
    second = server._taskflow(
        _arguments(workspace, taskId=task_id, kind="approval", question="Approve?")
    )
    assert second["ok"] is True, second
    record = second["task"]["attention"][-1]
    log = record["notification_log"]
    # One entry per logical attempt: the retry did not append a second
    # attempt=1 entry.
    assert len(log) == 1, log
    entry = log[0]
    assert entry["attempt"] == 1
    assert entry["room_accepted"] is True
    assert "error" not in entry
    # The failed physical attempt was folded into retries[] with its
    # error.
    assert len(entry["retries"]) == 1, entry
    folded = entry["retries"][0]
    assert folded["room_accepted"] is False
    assert "HTTP 503" in folded["error"]
    # The record carries the event id of the successful send ...
    assert record["eventId"] == "$evt1"
    # ... and the folded shape is what was persisted.
    on_disk = _read_task(workspace, task_id)
    assert len(on_disk["attention"][-1]["notification_log"]) == 1
