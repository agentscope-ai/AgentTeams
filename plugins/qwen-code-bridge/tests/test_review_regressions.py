"""Regression coverage for approval boundaries and coexisting wake events."""

import asyncio
import json
import shlex
import time

import pytest

from qwen_code_bridge import approval, wake_queue
from qwen_code_bridge.dispatch import DispatchService, _utcnow


@pytest.mark.parametrize("command", [
    'export OUTPUT=../outside/marker; printf owned > "$OUTPUT"',
    'printf owned > "$OUTPUT"',
    'cat "$INPUT"',
    'cat $INPUT',
    'cd sub && cat data',
    'node -e "console.log(1)"',
    'ruby -e "puts 1"',
    'bash script.sh',
])
def test_unsafe_shell_forms_are_not_auto_approved(tmp_path, command):
    result = approval.decide("shell", {"command": command, "cwd": str(tmp_path)}, [str(tmp_path)])
    assert result["behavior"] == "deny"


def test_inline_python_cannot_write_outside_run_root(tmp_path):
    root = tmp_path / "run"
    root.mkdir()
    outside = tmp_path / "outside-marker"
    command = shlex.join(["python3", "-c", f"open({str(outside)!r}, 'w').write('owned')"])
    result = approval.decide("shell", {"command": command, "cwd": str(root)}, [str(root)])
    assert result["behavior"] == "deny"
    assert not outside.exists()


def test_fresh_watchdog_finalize_keeps_terminal_write_ownership(tmp_path, monkeypatch):
    svc = DispatchService(dispatch_dir=tmp_path / "dispatch")
    monkeypatch.setattr(svc, "_pid_alive", lambda pid: False)
    task = {
        "task_id": "a" * 32, "state": "running", "pid": 123,
        "timeout": 1, "started_epoch": time.time() - 10,
        "terminating": True, "updated_at": _utcnow(), "exit_code": None,
    }
    svc._write_task(task)
    result = svc._mark_stale(task)
    assert result["state"] == "running"
    assert "finished_at" not in result


@pytest.fixture
def isolated_queue(monkeypatch, tmp_path):
    wake_queue.stop()
    monkeypatch.setattr(wake_queue, "_path", lambda: tmp_path / "wake-queue.json")
    monkeypatch.setattr(wake_queue, "_lock", None)
    monkeypatch.setattr(wake_queue, "_on_delivered", None)
    monkeypatch.setattr(wake_queue, "_is_delivered", None)
    monkeypatch.setattr(wake_queue, "chat_is_busy", lambda *args: False)
    yield
    wake_queue.stop()


def test_terminal_and_denials_survive_one_drain(isolated_queue):
    records = [
        {"task_id": "a" * 32, "text": "done"},  # pre-upgrade terminal
        {"task_id": "a" * 32, "kind": "denial", "event_id": "rule-a:1", "text": "denied a"},
        {"task_id": "a" * 32, "kind": "denial", "event_id": "rule-b:1", "text": "denied b"},
    ]
    for record in records:
        wake_queue.enqueue(record)
    assert len(wake_queue.load()) == 3
    calls = []
    stats = asyncio.run(wake_queue.drain_once(lambda *args, **kwargs: calls.append(args[3])))
    assert stats["sent"] == 3
    assert calls == ["done", "denied a", "denied b"]
    assert not wake_queue.load()


def test_terminal_discard_preserves_denials(isolated_queue):
    wake_queue.enqueue({"task_id": "a" * 32, "text": "done"})
    wake_queue.enqueue({"task_id": "a" * 32, "kind": "denial", "text": "denied"})
    wake_queue.discard("a" * 32)
    assert [r["text"] for r in wake_queue.load()] == ["denied"]


def test_denial_event_identity_deduplicates_only_same_event(isolated_queue):
    for event in ["rule-a:1", "rule-a:1", "rule-a:2"]:
        wake_queue.enqueue({"task_id": "a" * 32, "kind": "denial", "event_id": event, "text": "denied"})
    assert len(wake_queue.load()) == 2


def test_legacy_denials_with_distinct_payloads_survive_restart(isolated_queue):
    path = wake_queue._path()
    path.write_text(json.dumps([
        {"task_id": "a" * 32, "kind": "denial", "text": "denied a"},
        {"task_id": "a" * 32, "kind": "denial", "text": "denied b"},
    ]))
    stats = asyncio.run(wake_queue.drain_once(lambda *args, **kwargs: None))
    assert stats["sent"] == 2
    assert not wake_queue.load()


def test_retry_preserves_each_event(isolated_queue):
    for event in ["rule-a:1", "rule-b:1"]:
        wake_queue.enqueue({"task_id": "a" * 32, "kind": "denial", "event_id": event, "text": event})
    def fail(*args, **kwargs):
        raise RuntimeError("owned transport failure")
    stats = asyncio.run(wake_queue.drain_once(fail))
    assert stats["failed"] == 2
    assert {r["event_id"] for r in wake_queue.load()} == {"rule-a:1", "rule-b:1"}
    assert all(r["attempts"] == 1 for r in wake_queue.load())
