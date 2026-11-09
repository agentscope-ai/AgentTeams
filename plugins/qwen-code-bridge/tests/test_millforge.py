# -*- coding: utf-8 -*-
"""Millforge contract integration: M5 tiers + T5 coding-runs mapping."""

from __future__ import annotations

from qwen_code_bridge.millforge import coding_run_record, notify_tier


def test_notify_tiers() -> None:
    assert notify_tier("queued") == "machine"
    assert notify_tier("running") == "agent"
    assert notify_tier("done") == "agent"
    for bad in ("failed", "timeout", "killed"):
        assert notify_tier(bad) == "human"
    assert notify_tier("failed-budget") == "human"
    assert notify_tier("") == "machine"


def test_coding_run_record_mapping() -> None:
    task = {
        "task_id": "a" * 32,
        "session_id": "11111111-2222-4333-8444-555555555555",
        "cli": "qwen",
        "state": "done",
        "exit_code": 0,
        "started_at": "2026-10-22T18:00:00Z",
        "finished_at": "2026-10-22T18:01:00Z",
        "wall_s": 60.0,
        "log_tail": '{"type":"result","result":"OK"}\n',
        "approval_mode": "auto",
        "approval_level": "AUTO",
    }
    rec = coding_run_record(task)
    assert rec["task_id"] == "a" * 32
    assert rec["run"] == task["session_id"]
    assert rec["status"] == "succeeded"
    assert rec["exit_code"] == 0
    assert rec["ended_at"] == task["finished_at"]
    assert rec["summary"] == '{"type":"result","result":"OK"}'
    assert rec["notify_tier"] == "agent"
    assert rec["approval_mode"] == "auto"

    failed = dict(task, state="timeout", exit_code=55)
    assert coding_run_record(failed)["status"] == "timeout"
    assert coding_run_record(failed)["notify_tier"] == "human"
