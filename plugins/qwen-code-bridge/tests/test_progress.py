# -*- coding: utf-8 -*-
"""Summary-level realtime progress derivation — unit tests."""

import json

from qwen_code_bridge import progress


def feed(tracker, events):
    for event in events:
        tracker.feed(event)


def test_phase_editing_and_target():
    t = progress.ProgressTracker(roots=["/tmp/x"])
    feed(t, [{"type": "system", "subtype": "init"}])
    feed(t, [{"type": "assistant", "message": {"content": [{"type": "tool_use", "name": "edit", "input": {"file_path": "/tmp/x/a.py"}}]}}])
    snap = t.snapshot("running")
    assert snap["phase"] == "editing"
    assert snap["last_tool"] == "edit"
    assert snap["last_target"] == "/tmp/x/a.py"
    assert snap["tool_calls"] == 1


def test_phase_testing():
    t = progress.ProgressTracker()
    feed(t, [{"type": "assistant", "message": {"content": [{"type": "tool_use", "name": "shell", "input": {"command": "pytest -q"}}]}}])
    assert t.snapshot("running")["phase"] == "testing"


def test_tests_parsed_from_tool_result():
    t = progress.ProgressTracker()
    feed(t, [{"type": "user", "message": {"content": [{"type": "tool_result", "content": "7 passed, 2 failed in 1.2s"}]}}])
    snap = t.snapshot("running")
    assert snap["tests"]["passed"] == 7
    assert snap["tests"]["failed"] == 2


def test_result_done_and_tokens():
    t = progress.ProgressTracker()
    feed(t, [{"type": "result", "subtype": "success", "usage": {"input_tokens": 10, "output_tokens": 3}}])
    snap = t.snapshot("running")
    assert snap["phase"] == "done"
    assert snap["tokens"] == {"input": 10, "output": 3}


def test_result_failed():
    t = progress.ProgressTracker()
    feed(t, [{"type": "result", "subtype": "error", "is_error": True, "result": "boom"}])
    snap = t.snapshot("done")
    assert snap["phase"] == "failed"
    assert "boom" in snap["last_error"]


def test_stalled_line():
    import time

    t = progress.ProgressTracker(stall_after=5)
    t.last_event = time.time() - 30
    line = t.line("running")
    assert "STALLED" in line


def test_write_progress_atomic(tmp_path):
    path = tmp_path / "p.json"
    progress.write_progress(path, {"phase": "tool"})
    assert json.loads(path.read_text())["phase"] == "tool"


def test_first_event_latency_recorded():
    t = progress.ProgressTracker()
    assert t.snapshot("running")["first_event_s"] is None
    feed(t, [{"type": "system", "subtype": "init"}])
    first = t.snapshot("running")["first_event_s"]
    assert isinstance(first, (int, float)) and first >= 0
