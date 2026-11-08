# -*- coding: utf-8 -*-
"""AgentTeams coding-runs index writer (Q3) unit tests."""

import json

from qwen_code_bridge import runs


def _task(**kw):
    base = {
        "task_id": "t1",
        "task": "demo",
        "cli": "qwen",
        "prompt": "do it",
        "state": "done",
        "exit_code": 0,
        "timeout": 600,
        "started_at": "2026-10-22T00:00:00Z",
        "finished_at": "2026-10-22T00:01:00Z",
    }
    base.update(kw)
    return base


def test_writes_transcript_and_index(tmp_path):
    rec = runs.write_run_record(_task(), "hello log", tmp_path)
    assert rec is not None
    assert (tmp_path / rec["run"]).read_text() == "hello log"
    line = json.loads((tmp_path / "runs.jsonl").read_text().strip())
    assert line["v"] == 1
    assert line["status"] == "ok"
    assert line["run"] == rec["run"]
    assert line["log_bytes"] == len(b"hello log")


def test_status_mapping(tmp_path):
    for state, status in (
        ("done", "ok"),
        ("timeout", "timeout"),
        ("failed", "failed"),
        ("stale", "failed"),
    ):
        rec = runs.write_run_record(_task(state=state), "x", tmp_path)
        assert rec["record"]["status"] == status


def test_no_dir_is_noop():
    assert runs.write_run_record(_task(), "x", None) is None


def test_collision_claims_distinct_names(tmp_path):
    first = runs.write_run_record(_task(), "a", tmp_path)
    second = runs.write_run_record(_task(), "b", tmp_path)
    assert first["run"] != second["run"]


def test_parse_cli_version():
    assert runs.parse_cli_version("0.26.0") == "0.26.0"
    assert runs.parse_cli_version("qwen 0.26.0\n") == "0.26.0"
    assert runs.parse_cli_version("") is None
