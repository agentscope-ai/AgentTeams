# -*- coding: utf-8 -*-
"""Approval observability (#1411) — receipts on the hook path, the
cross-task ``GET /approvals`` view, and the mid-run denial wake.

Groups:
1. hook deny appends one ``source=hook`` receipt line; hook allow writes nothing;
2. hook env missing (no TASK_ID / RECEIPTS) → no crash, decision unchanged;
3. ``/approvals``: two tasks' mixed control+hook receipts, behavior /
   task_id / since / limit filters, empty dir → empty list;
4. ``_note_denial``: same rule deduped (300 s window), distinct rules
   wake, wake text carries rule/reason, ``wake_status`` untouched;
5. watchdog ``_scan_once`` observes new hook denials once (offset watermark).
"""

import json
import os
import time
from pathlib import Path
from typing import Any, Generator

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from qwen_code_bridge import dispatch as dispatch_mod
from qwen_code_bridge import hook_policy, state
from qwen_code_bridge.dispatch import DispatchService
from qwen_code_bridge.router import router as bridge_router

API = "/api/qwen-code-bridge"
TASK_A = "a" * 32
TASK_B = "b" * 32


def _shell_payload(command: str, cwd: Path) -> dict:
    return {
        "session_id": "test",
        "hook_event_name": "PreToolUse",
        "cwd": str(cwd),
        "tool_name": "run_shell_command",
        "tool_input": {"command": command},
    }


def _decision(out: dict) -> str:
    return out["hookSpecificOutput"]["permissionDecision"]


def _reason(out: dict) -> str:
    return out["hookSpecificOutput"]["permissionDecisionReason"]


def _entry(
    task_id: str,
    ts: str,
    behavior: str,
    rule: str,
    reason: str,
    source: str,
    tool: str = "run_shell_command",
    request_id: str = "",
    requires_human: bool = False,
) -> dict:
    return {
        "ts": ts,
        "task_id": task_id,
        "kind": "approval",
        "request_id": request_id,
        "tool_name": tool,
        "input": {"command": "x"},
        "risk": "allow" if behavior == "allow" else "deny",
        "rule": rule,
        "reason": reason,
        "behavior": behavior,
        "requires_human": requires_human,
        "source": source,
    }


def _write_receipts(path: Path, entries: list[dict], mtime: float) -> None:
    path.write_text(
        "".join(json.dumps(e, ensure_ascii=False) + "\n" for e in entries),
        encoding="utf-8",
    )
    os.utime(path, (mtime, mtime))


# ---------------------------------------------------------------------------
# 1. hook path: deny writes one receipt, allow writes nothing
# ---------------------------------------------------------------------------


def test_hook_deny_appends_receipt(tmp_path, monkeypatch):
    receipts = tmp_path / f"{TASK_A}.receipts.jsonl"
    monkeypatch.setenv(state.APPROVAL_ROOTS_ENV, json.dumps([str(tmp_path)]))
    monkeypatch.setenv("QWEN_CODE_BRIDGE_TASK_ID", TASK_A)
    monkeypatch.setenv("QWEN_CODE_BRIDGE_RECEIPTS", str(receipts))

    out = hook_policy.evaluate(_shell_payload("cat /var/private/data", tmp_path))
    assert _decision(out) == "deny"
    assert "outside-root" in _reason(out)

    lines = receipts.read_text(encoding="utf-8").splitlines()
    assert len(lines) == 1
    entry = json.loads(lines[0])
    assert entry["source"] == "hook"
    assert entry["task_id"] == TASK_A
    assert entry["behavior"] == "deny"
    assert entry["rule"] == "outside-root"
    assert entry["reason"]
    assert entry["request_id"] == ""


def test_hook_allow_writes_no_receipt(tmp_path, monkeypatch):
    receipts = tmp_path / f"{TASK_A}.receipts.jsonl"
    monkeypatch.setenv(state.APPROVAL_ROOTS_ENV, json.dumps([str(tmp_path)]))
    monkeypatch.setenv("QWEN_CODE_BRIDGE_TASK_ID", TASK_A)
    monkeypatch.setenv("QWEN_CODE_BRIDGE_RECEIPTS", str(receipts))

    out = hook_policy.evaluate(_shell_payload("git status", tmp_path))
    assert _decision(out) == "allow"
    assert not receipts.exists()


def test_hook_config_deny_also_appends_receipt(tmp_path, monkeypatch):
    """Fail-closed (missing roots) is a deny too — it leaves a receipt."""
    receipts = tmp_path / f"{TASK_A}.receipts.jsonl"
    monkeypatch.delenv(state.APPROVAL_ROOTS_ENV, raising=False)
    monkeypatch.setenv("QWEN_CODE_BRIDGE_TASK_ID", TASK_A)
    monkeypatch.setenv("QWEN_CODE_BRIDGE_RECEIPTS", str(receipts))

    out = hook_policy.evaluate(_shell_payload("git status", tmp_path))
    assert _decision(out) == "deny"
    assert "hook-config" in _reason(out)
    entry = json.loads(receipts.read_text(encoding="utf-8").strip())
    assert entry["source"] == "hook"
    assert entry["rule"] == "hook-config"


# ---------------------------------------------------------------------------
# 2. hook env missing → no crash, decision unchanged
# ---------------------------------------------------------------------------


def test_hook_missing_env_no_crash_decision_unchanged(tmp_path, monkeypatch):
    monkeypatch.setenv(state.APPROVAL_ROOTS_ENV, json.dumps([str(tmp_path)]))
    monkeypatch.delenv("QWEN_CODE_BRIDGE_TASK_ID", raising=False)
    monkeypatch.delenv("QWEN_CODE_BRIDGE_RECEIPTS", raising=False)

    out = hook_policy.evaluate(_shell_payload("cat /var/private/data", tmp_path))
    assert _decision(out) == "deny"
    assert "outside-root" in _reason(out)
    # Nothing written anywhere under the run root.
    assert list(tmp_path.glob("*.receipts.jsonl")) == []


def test_hook_partial_env_writes_nothing(tmp_path, monkeypatch):
    monkeypatch.setenv(state.APPROVAL_ROOTS_ENV, json.dumps([str(tmp_path)]))
    receipts = tmp_path / f"{TASK_A}.receipts.jsonl"
    # Only one of the two vars: still no receipt.
    monkeypatch.setenv("QWEN_CODE_BRIDGE_TASK_ID", TASK_A)
    monkeypatch.delenv("QWEN_CODE_BRIDGE_RECEIPTS", raising=False)
    hook_policy.evaluate(_shell_payload("cat /var/private/data", tmp_path))
    assert not receipts.exists()
    monkeypatch.delenv("QWEN_CODE_BRIDGE_TASK_ID", raising=False)
    monkeypatch.setenv("QWEN_CODE_BRIDGE_RECEIPTS", str(receipts))
    hook_policy.evaluate(_shell_payload("cat /var/private/data", tmp_path))
    assert not receipts.exists()


# ---------------------------------------------------------------------------
# 3. GET /approvals — mixed control+hook receipts across two tasks
# ---------------------------------------------------------------------------


@pytest.fixture
def approvals_client(tmp_path) -> Generator[TestClient, None, None]:
    saved = {key: os.environ.get(key) for key in ("HOME", state.STATE_DIR_ENV)}
    state_dir = tmp_path / "state"
    os.environ["HOME"] = str(tmp_path)
    os.environ[state.STATE_DIR_ENV] = str(state_dir)
    dispatch_dir = state_dir / "dispatch"
    dispatch_dir.mkdir(parents=True)

    base = 1_762_200_000.0  # 2025-11-03T00:00:00Z-ish epoch, mtime only
    _write_receipts(
        dispatch_dir / f"{TASK_A}.receipts.jsonl",
        [
            _entry(TASK_A, "2026-11-04T00:00:00Z", "allow",
                   "exec-allow", "ok", "control", request_id="r1"),
            _entry(TASK_A, "2026-11-04T00:01:00Z", "deny",
                   "exec-compound", "shell metacharacter outside quotes",
                   "control", request_id="r2"),
            _entry(TASK_A, "2026-11-04T00:02:00Z", "deny",
                   "outside-root", "path argument is outside the run root",
                   "hook"),
            _entry(TASK_A, "2026-11-04T00:03:00Z", "deny",
                   "unknown-tool", "tool is not in the policy table",
                   "control", request_id="r4", requires_human=True),
        ],
        base + 600,
    )
    _write_receipts(
        dispatch_dir / f"{TASK_B}.receipts.jsonl",
        [
            _entry(TASK_B, "2026-11-03T00:00:00Z", "deny",
                   "exec-denied", "program not allowed", "hook"),
            _entry(TASK_B, "2026-11-03T01:00:00Z", "allow",
                   "read", "read-only tool", "control",
                   tool="read_file", request_id="b1"),
        ],
        base,
    )

    app = FastAPI()
    app.include_router(bridge_router, prefix=API)
    yield TestClient(app)
    for key, value in saved.items():
        if value is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = value


def test_approvals_all_and_file_order(approvals_client):
    resp = approvals_client.get(f"{API}/approvals")
    assert resp.status_code == 200
    body = resp.json()
    assert body["total"] == 6
    assert body["truncated"] is False
    assert len(body["approvals"]) == 6
    # Newest mtime file first: task A's rows lead.
    assert body["approvals"][0]["task_id"] == TASK_A
    row = body["approvals"][0]
    for key in (
        "ts", "task_id", "tool_name", "behavior",
        "rule", "reason", "request_id", "source",
    ):
        assert key in row


def test_approvals_filter_task_id(approvals_client):
    body = approvals_client.get(
        f"{API}/approvals", params={"task_id": TASK_A}
    ).json()
    assert body["total"] == 4
    assert {row["task_id"] for row in body["approvals"]} == {TASK_A}


def test_approvals_filter_behavior(approvals_client):
    deny = approvals_client.get(
        f"{API}/approvals", params={"behavior": "deny"}
    ).json()
    assert deny["total"] == 4
    assert all(row["behavior"] == "deny" for row in deny["approvals"])

    escalate = approvals_client.get(
        f"{API}/approvals", params={"behavior": "escalate"}
    ).json()
    assert escalate["total"] == 1
    row = escalate["approvals"][0]
    assert row["task_id"] == TASK_A and row["rule"] == "unknown-tool"
    assert row["source"] == "control"

    allow = approvals_client.get(
        f"{API}/approvals", params={"behavior": "allow"}
    ).json()
    assert allow["total"] == 2
    assert all(row["behavior"] == "allow" for row in allow["approvals"])


def test_approvals_filter_since_and_limit(approvals_client):
    since = approvals_client.get(
        f"{API}/approvals", params={"since": "2026-11-04T00:00:30Z"}
    ).json()
    assert since["total"] == 3
    assert {row["task_id"] for row in since["approvals"]} == {TASK_A}

    limited = approvals_client.get(
        f"{API}/approvals", params={"limit": 1}
    ).json()
    assert limited["total"] == 6
    assert len(limited["approvals"]) == 1
    assert limited["truncated"] is True


def test_approvals_empty_dir_empty_list(tmp_path):
    saved = {key: os.environ.get(key) for key in ("HOME", state.STATE_DIR_ENV)}
    os.environ["HOME"] = str(tmp_path)
    os.environ[state.STATE_DIR_ENV] = str(tmp_path / "state")
    app = FastAPI()
    app.include_router(bridge_router, prefix=API)
    client = TestClient(app)
    try:
        body = client.get(f"{API}/approvals").json()
        assert body == {"approvals": [], "total": 0, "truncated": False}
    finally:
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


# ---------------------------------------------------------------------------
# 4. _note_denial — rule dedupe, wake text, wake_status untouched
# ---------------------------------------------------------------------------


def _running_task(tmp_path: Path, task_id: str = TASK_A) -> None:
    task = {
        "task_id": task_id,
        "task": "t",
        "state": "running",
        "wake": {"session_id": "wake-1", "agent_id": "A"},
        "origin": {"session_id": "origin-1", "channel": "console"},
        "wake_status": None,
    }
    (tmp_path / f"{task_id}.json").write_text(json.dumps(task), encoding="utf-8")


def _task_file(tmp_path: Path, task_id: str = TASK_A) -> dict:
    return json.loads(
        (tmp_path / f"{task_id}.json").read_text(encoding="utf-8")
    )


def _queued_recorder(monkeypatch) -> list[dict]:
    queued: list[dict[str, Any]] = []
    monkeypatch.setattr(
        dispatch_mod.wake_queue, "enqueue", lambda rec: queued.append(dict(rec))
    )
    return queued


def test_note_denial_same_rule_deduped(tmp_path, monkeypatch):
    queued = _queued_recorder(monkeypatch)
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    _running_task(tmp_path)
    decision = {
        "rule": "exec-compound",
        "reason": "shell metacharacter outside quotes",
        "behavior": "deny",
    }
    svc._note_denial(TASK_A, "run_shell_command", decision)
    svc._note_denial(TASK_A, "run_shell_command", decision)
    assert len(queued) == 1
    text = queued[0]["text"]
    assert "exec-compound" in text
    assert "shell metacharacter outside quotes" in text
    assert TASK_A in text and "run_shell_command" in text
    # Origin session (not the wake target session) is the sid.
    assert queued[0]["session_id"] == "origin-1"
    task_now = _task_file(tmp_path)
    assert task_now["wake_status"] is None
    assert set(task_now["denial_wakes"]) == {"exec-compound"}


def test_note_denial_distinct_rules_wake(tmp_path, monkeypatch):
    queued = _queued_recorder(monkeypatch)
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    _running_task(tmp_path)
    svc._note_denial(
        TASK_A, "run_shell_command",
        {"rule": "exec-compound", "reason": "compound", "behavior": "deny"},
    )
    svc._note_denial(
        TASK_A, "cat",
        {"rule": "outside-root", "reason": "outside", "behavior": "deny"},
    )
    assert len(queued) == 2
    assert set(_task_file(tmp_path)["denial_wakes"]) == {
        "exec-compound",
        "outside-root",
    }


def test_note_denial_cooldown_window(tmp_path, monkeypatch):
    queued = _queued_recorder(monkeypatch)
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    _running_task(tmp_path)
    # Same rule 100 s ago: inside the 300 s window → suppressed.
    task = _task_file(tmp_path)
    task["denial_wakes"] = {"exec-compound": time.time() - 100}
    (tmp_path / f"{TASK_A}.json").write_text(json.dumps(task), encoding="utf-8")
    svc._note_denial(
        TASK_A, "run_shell_command",
        {"rule": "exec-compound", "reason": "again", "behavior": "deny"},
    )
    assert len(queued) == 0
    # Same rule 301 s ago: window over → wakes again.
    task = _task_file(tmp_path)
    task["denial_wakes"] = {"exec-compound": time.time() - 301}
    (tmp_path / f"{TASK_A}.json").write_text(json.dumps(task), encoding="utf-8")
    svc._note_denial(
        TASK_A, "run_shell_command",
        {"rule": "exec-compound", "reason": "again", "behavior": "deny"},
    )
    assert len(queued) == 1


def test_note_denial_rule_cap(tmp_path, monkeypatch):
    queued = _queued_recorder(monkeypatch)
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    _running_task(tmp_path)
    # Ten distinct rules already noted: an 11th is suppressed.
    task = _task_file(tmp_path)
    task["denial_wakes"] = {
        f"rule-{i}": time.time() - 100 for i in range(10)
    }
    (tmp_path / f"{TASK_A}.json").write_text(json.dumps(task), encoding="utf-8")
    svc._note_denial(
        TASK_A, "cat",
        {"rule": "rule-11", "reason": "new", "behavior": "deny"},
    )
    assert len(queued) == 0
    # A rule already in the map is not capped by the distinct-rule count.
    task = _task_file(tmp_path)
    task["denial_wakes"]["rule-1"] = time.time() - 301
    (tmp_path / f"{TASK_A}.json").write_text(json.dumps(task), encoding="utf-8")
    svc._note_denial(
        TASK_A, "cat",
        {"rule": "rule-1", "reason": "new", "behavior": "deny"},
    )
    assert len(queued) == 1


def test_note_denial_missing_task_no_raise(tmp_path, monkeypatch):
    _queued_recorder(monkeypatch)
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    svc._note_denial(
        "f" * 32, "cat", {"rule": "x", "reason": "y", "behavior": "deny"}
    )  # no task file: silent no-op


# ---------------------------------------------------------------------------
# 5. watchdog _scan_once observes new hook denials exactly once
# ---------------------------------------------------------------------------


def test_scan_once_wakes_on_hook_denial(tmp_path, monkeypatch):
    queued = _queued_recorder(monkeypatch)
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    task = {
        "task_id": TASK_A,
        "task": "t",
        "state": "running",
        "pid": None,
        "started_epoch": time.time(),
        "timeout": 600,
        "wake": {"session_id": "wake-9", "agent_id": "A"},
        "origin": {"session_id": "origin-9", "channel": "console"},
        "wake_status": None,
    }
    (tmp_path / f"{TASK_A}.json").write_text(json.dumps(task), encoding="utf-8")
    receipts = tmp_path / f"{TASK_A}.receipts.jsonl"
    _write_receipts(
        receipts,
        [
            _entry(TASK_A, "2026-11-04T00:00:00Z", "allow",
                   "exec-allow", "ok", "control", request_id="r1"),
            _entry(TASK_A, "2026-11-04T00:01:00Z", "deny",
                   "exec-denied", "program not allowed", "hook"),
            _entry(TASK_A, "2026-11-04T00:02:00Z", "allow",
                   "read", "read-only tool", "hook", tool="read_file"),
            _entry(TASK_A, "2026-11-04T00:03:00Z", "deny",
                   "exec-denied", "program not allowed", "hook"),
        ],
        time.time(),
    )

    svc._scan_once()
    # control allow + hook allow ignored; two hook denies share a rule → 1.
    assert len(queued) == 1
    assert "exec-denied" in queued[0]["text"]
    task_now = _task_file(tmp_path)
    assert task_now["hook_denial_offset"] == receipts.stat().st_size
    assert task_now["wake_status"] is None

    # Second round: the watermark stops the re-scan, no second wake.
    svc._scan_once()
    assert len(queued) == 1

    # A NEW hook deny with a different rule wakes once more.
    with receipts.open("a", encoding="utf-8") as fh:
        fh.write(
            json.dumps(
                _entry(TASK_A, "2026-11-04T00:04:00Z", "deny",
                       "exec-compound", "compound", "hook"),
                ensure_ascii=False,
            )
            + "\n"
        )
    svc._scan_once()
    assert len(queued) == 2
    assert "exec-compound" in queued[1]["text"]
    assert _task_file(tmp_path)["hook_denial_offset"] == (
        receipts.stat().st_size
    )


def test_scan_once_ignores_non_running_and_control_denials(tmp_path, monkeypatch):
    queued = _queued_recorder(monkeypatch)
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    # Terminal task: hook denies in its receipts are not re-woken.
    _running_task(tmp_path, TASK_A)
    (tmp_path / f"{TASK_A}.json").write_text(
        json.dumps({
            **_task_file(tmp_path),
            "state": "done",
            "pid": None,
        }),
        encoding="utf-8",
    )
    _write_receipts(
        tmp_path / f"{TASK_A}.receipts.jsonl",
        [_entry(TASK_A, "2026-11-04T00:01:00Z", "deny",
                "exec-denied", "program not allowed", "hook")],
        time.time(),
    )
    # Running task whose only denial came through the control path:
    # _note_hook_denials must not double-wake it (only _note_denial,
    # called from the control handler, is responsible).
    _running_task(tmp_path, TASK_B)
    _write_receipts(
        tmp_path / f"{TASK_B}.receipts.jsonl",
        [_entry(TASK_B, "2026-11-04T00:01:00Z", "deny",
                "exec-compound", "compound", "control", request_id="c1")],
        time.time(),
    )
    svc._scan_once()
    assert len(queued) == 0
