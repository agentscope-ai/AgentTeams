# -*- coding: utf-8 -*-
"""Dispatch surface tests (design §4.2, gateless per the 10/22 ruling).

Drives a real ``DispatchService`` (real subprocess spawn, real file
state) against fake CLI binaries on a restricted tmp PATH, plus the
HTTP routes through ``TestClient``. The service is not patched; the
wake sender is injected through the constructor — the designed seam.

Env overrides (try/finally, same pattern as the P1 router tests):
- ``QWENPAW_CODING_CLI_STATE_DIR`` -> tmp state dir;
- ``QWENPAW_DISPATCH_WATCH_INTERVAL`` -> 0.1s poll for fast terminal
  states (the production default is 20s, like dispatch_watch.py);
- ``PATH`` -> tmp bin dir plus ``/usr/bin:/bin`` only, so the fake
  CLIs resolve and the real managed CLIs (``qwen`` & co., installed
  under /usr/local) are deterministically absent.
"""

# pylint: disable=redefined-outer-name
from __future__ import annotations

import asyncio
import json
import os
import time
from pathlib import Path
from typing import Any, Generator

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from qwen_code_bridge.dispatch import (
    WATCH_INTERVAL_ENV,
    CliNotFoundError,
    DispatchBusyError,
    DispatchService,
)
from qwen_code_bridge.router import build_router
from qwen_code_bridge.state import STATE_DIR_ENV

API = "/api/qwen-code-bridge"

WAKE = {
    "session_id": "matrix:!wake42:matrix.example",
    "agent_id": "AgentTeamGuide",
    "base_url": None,
}


def _make_bin(bin_dir: Path, name: str, body: str) -> None:
    bin_dir.mkdir(parents=True, exist_ok=True)
    path = bin_dir / name
    path.write_text(f"#!/bin/sh\n{body}", encoding="utf-8")
    path.chmod(0o755)


class Env:
    """Hermetic env: tmp state dir, 0.1s watchdog poll, fake CLI PATH."""

    def __init__(self, tmp_path: Path) -> None:
        self.tmp = tmp_path
        self.state_dir = tmp_path / "state"
        self.bin_dir = tmp_path / "bin"
        self._saved: dict[str, str] = {}

    def __enter__(self) -> "Env":
        self._saved = dict(os.environ)
        os.environ[STATE_DIR_ENV] = str(self.state_dir)
        os.environ[WATCH_INTERVAL_ENV] = "0.1"
        os.environ["PATH"] = f"{self.bin_dir}:/usr/bin:/bin"
        os.environ["QWEN_CODE_BRIDGE_CHAT_SYNC"] = "0"
        os.environ["QWEN_CODE_BRIDGE_CLI_HOME"] = str(self.tmp / "cli-home")
        return self

    def __exit__(self, *exc_info: Any) -> None:
        os.environ.clear()
        os.environ.update(self._saved)


def _wake_recorder() -> tuple[list[dict[str, str]], Any]:
    calls: list[dict[str, str]] = []

    def sender(
        session_id: str,
        agent_id: str,
        base_url: str,
        text: str,
        channel: str = "console",
        user_id: str = "default",
        room_bell: bool = False,
        approval_level: str = "",
    ) -> None:
        calls.append(
            {
                "session_id": session_id,
                "agent_id": agent_id,
                "base_url": base_url,
                "text": text,
                "channel": channel,
                "user_id": user_id,
                "room_bell": room_bell,
                "approval_level": approval_level,
            }
        )

    return calls, sender


async def _wait_terminal(
    service: DispatchService, task_id: str, tries: int = 200
) -> dict:
    for _ in range(tries):
        task = service.get_task(task_id)
        if task is not None and task["state"] != "running":
            return task
        await asyncio.sleep(0.05)
    raise AssertionError(f"task {task_id} still running after {tries} polls")


async def _wait_wake(service: DispatchService, task_id: str, tries: int = 100) -> dict:
    """Poll until wake_status has settled (wake fires after the terminal write)."""
    for _ in range(tries):
        task = service.get_task(task_id)
        status = (task or {}).get("wake_status")
        if status is not None and status != "pending":
            return task
        await asyncio.sleep(0.05)
    raise AssertionError(f"task {task_id} wake_status never settled")


@pytest.mark.asyncio
async def test_spawn_done_wakes(tmp_path) -> None:
    """spawn -> running -> done; log captured; wake got the terminal event.

    A re-dispatch with the same task name is allowed once the first
    run is terminal (claim flag released).
    """
    with Env(tmp_path) as env:
        _make_bin(env.bin_dir, "fakeq", 'echo \'{"type":"assistant","text":"ok"}\'')
        calls, sender = _wake_recorder()
        service = DispatchService(wake_sender=sender)
        task_id = await service.start(
            task_name="t-done",
            prompt="do the thing",
            cwd=None,
            cli="fakeq",
            timeout=30,
            wake=dict(WAKE),
        )
        task = service.get_task(task_id)
        assert task is not None
        assert task["state"] in ("running", "done")
        assert task["pid"]
        assert task["session_id"]
        assert task["wake"] == WAKE
        assert "files_to_touch" not in task  # pure metadata, never persisted

        terminal = await _wait_terminal(service, task_id)
        assert terminal["state"] == "done"
        assert terminal["exit_code"] == 0
        assert terminal["wall_s"] is not None
        assert terminal["log_tail"].strip().startswith("{")
        assert '"text":"ok"' in terminal["log_tail"]
        settled = await _wait_wake(service, task_id)
        assert settled["wake_status"] == "sent"

        assert len(calls) == 1
        assert calls[0]["session_id"] == WAKE["session_id"]
        assert calls[0]["agent_id"] == "AgentTeamGuide"
        assert "终态=done" in calls[0]["text"]
        assert f"dispatch/{task_id}/log" in calls[0]["text"]

        log = service.get_log(task_id)
        assert log is not None
        assert '"text":"ok"' in log

        # claim flag released after terminal: same name may run again
        task_id2 = await service.start(
            task_name="t-done",
            prompt="again",
            cwd=None,
            cli="fakeq",
            timeout=30,
            wake=None,
        )
        terminal2 = await _wait_terminal(service, task_id2)
        assert terminal2["state"] == "done"
        assert terminal2["wake_status"] == "skipped"
        assert len(calls) == 1  # no wake for the second run (wake=None)


@pytest.mark.asyncio
async def test_spawn_args_match_production_adapter(tmp_path, monkeypatch) -> None:
    """Spawn argv must equal the production adapter ("qwen"."args").

    The fake CLI records argv one-per-line; expected is the literal
    from tools/dispatch_adapters.json with {sid}/{prompt} substituted
    (10/22 ruling: yolo + CI + stream-json, not the simplified
    "-o json" from the original task book).
    """
    import qwen_code_bridge.dispatch as dispatch_mod

    monkeypatch.setattr(dispatch_mod, "_effective_approval_level", lambda _a: "")
    with Env(tmp_path) as env:
        args_file = env.tmp / "captured-args.txt"
        body = (
            'printf "%s\\n" "$@" > '
            f"{args_file}; "
            "echo '{\"type\":\"assistant\",\"text\":\"ok\"}'"
        )
        _make_bin(env.bin_dir, "fakeq", body)
        service = DispatchService()
        task_id = await service.start(
            task_name="t-args",
            prompt="do the thing",
            cwd=None,
            cli="fakeq",
            timeout=30,
            wake=None,
        )
        terminal = await _wait_terminal(service, task_id)
        assert terminal["state"] == "done"
        captured = args_file.read_text(encoding="utf-8").splitlines()
        assert captured == [
            "--approval-mode",
            "yolo",
            "--channel",
            "CI",
            "-o",
            "stream-json",
            "--chat-recording",
            "--session-id",
            terminal["session_id"],
            "-p",
            "do the thing",
        ]


@pytest.mark.asyncio
async def test_spawn_failed(tmp_path) -> None:
    """Non-zero exit -> terminal state failed + wake carries exit=1."""
    with Env(tmp_path) as env:
        _make_bin(
            env.bin_dir,
            "fakefail",
            'echo \'{"type":"error","msg":"boom"}\'; exit 1',
        )
        calls, sender = _wake_recorder()
        service = DispatchService(wake_sender=sender)
        task_id = await service.start(
            task_name="t-failed",
            prompt="x",
            cwd=None,
            cli="fakefail",
            timeout=30,
            wake=dict(WAKE),
        )
        terminal = await _wait_terminal(service, task_id)
        assert terminal["state"] == "failed"
        assert terminal["exit_code"] == 1
        settled = await _wait_wake(service, task_id)
        assert settled["wake_status"] == "sent"
        assert "终态=failed" in calls[0]["text"]
        assert "exit=1" in calls[0]["text"]
        log = service.get_log(task_id)
        assert log is not None
        assert '"msg":"boom"' in log


@pytest.mark.asyncio
async def test_timeout_kills(tmp_path, monkeypatch) -> None:
    """timeout=1s + a sleeping CLI (no events) -> killed once the stall
    window is exhausted, terminal state timeout.

    The wall clock alone no longer kills: the child is kept while the
    event stream is fresh, so this test pins a short stall window to
    reach the kill path.
    """
    monkeypatch.setenv("QWEN_CODE_BRIDGE_STALL_TIMEOUT", "2")
    with Env(tmp_path) as env:
        _make_bin(env.bin_dir, "fakesleep", "exec sleep 30")
        service = DispatchService(wake_sender=lambda *a: None)
        task_id = await service.start(
            task_name="t-timeout",
            prompt="x",
            cwd=None,
            cli="fakesleep",
            timeout=1,
            wake=None,
        )
        terminal = await _wait_terminal(service, task_id, tries=400)
        assert terminal["state"] == "timeout"
        assert terminal["exit_code"] == -9
        assert terminal["wall_s"] is not None
        assert terminal["wall_s"] < 20
        assert terminal["death_reason"] == "timeout_killed"
        assert terminal["wake_status"] == "skipped"


@pytest.mark.asyncio
async def test_claim_flag_and_not_found(tmp_path) -> None:
    """Claim flag: same name running -> busy; other names unaffected.

    CLI not on PATH -> CliNotFoundError with an install hint for
    managed CLIs.
    """
    with Env(tmp_path) as env:
        _make_bin(env.bin_dir, "fakesleep", "exec sleep 30")
        service = DispatchService(wake_sender=lambda *a: None)
        task_id = await service.start(
            task_name="held",
            prompt="x",
            cwd=None,
            cli="fakesleep",
            timeout=300,
            wake=None,
        )
        with pytest.raises(DispatchBusyError):
            await service.start(
                task_name="held",
                prompt="y",
                cwd=None,
                cli="fakesleep",
                timeout=300,
                wake=None,
            )
        # a different task name is not blocked (no parallel gate)
        other = await service.start(
            task_name="other",
            prompt="y",
            cwd=None,
            cli="fakesleep",
            timeout=300,
            wake=None,
        )

        with pytest.raises(CliNotFoundError) as excinfo:
            await service.start(
                task_name="n1",
                prompt="x",
                cwd=None,
                cli="no-such-cli-zzz",
                timeout=30,
                wake=None,
            )
        assert "not found on PATH" in str(excinfo.value)
        # restricted PATH has no /usr/local/bin -> 'qwen' absent -> hint
        with pytest.raises(CliNotFoundError) as excinfo2:
            await service.start(
                task_name="n2",
                prompt="x",
                cwd=None,
                cli="qwen",
                timeout=30,
                wake=None,
            )
        assert (
            "POST /api/qwen-code-bridge/qwen-code/install"
            in str(excinfo2.value)
        )
        # bad cwd -> ValueError
        with pytest.raises(ValueError):
            await service.start(
                task_name="n3",
                prompt="x",
                cwd="/nonexistent-dir-xyz",
                cli="fakesleep",
                timeout=30,
                wake=None,
            )

        # cleanup: kill both children so no orphan sleeps linger
        for tid in (task_id, other):
            t = service.get_task(tid)
            assert t is not None and t["state"] == "running"
            os.kill(t["pid"], 9)
        t1 = await _wait_terminal(service, task_id)
        t2 = await _wait_terminal(service, other)
        assert t1["state"] == "failed"
        assert t1["exit_code"] == -9
        assert t2["state"] == "failed"


@pytest.fixture
def dispatch_http(tmp_path) -> Generator[tuple[TestClient, Env], None, None]:
    with Env(tmp_path) as env:
        _make_bin(env.bin_dir, "fakeq", 'echo \'{"type":"assistant","text":"ok"}\'')
        application = FastAPI()
        application.include_router(
            build_router(), prefix="/api/qwen-code-bridge"
        )
        with TestClient(application) as client:
            yield client, env


def test_http_dispatch_lifecycle(dispatch_http) -> None:
    """POST 202 -> state file running/done -> log -> list -> 404s."""
    client, _ = dispatch_http
    res = client.post(
        f"{API}/dispatch",
        json={"task": "http-done", "prompt": "do it", "cli": "fakeq", "timeout": 30},
    )
    assert res.status_code == 202
    task_id = res.json()["task_id"]
    assert task_id

    terminal = None
    for _ in range(200):
        got = client.get(f"{API}/dispatch/{task_id}")
        assert got.status_code == 200
        if got.json()["state"] != "running":
            terminal = got.json()
            break
        time.sleep(0.05)
    assert terminal is not None
    assert terminal["state"] == "done"
    assert terminal["exit_code"] == 0
    assert terminal["wake_status"] == "skipped"

    log_res = client.get(f"{API}/dispatch/{task_id}/log")
    assert log_res.status_code == 200
    assert log_res.json()["task_id"] == task_id
    assert '"text":"ok"' in log_res.json()["log"]

    listing = client.get(f"{API}/dispatch").json()["tasks"]
    assert any(
        t["task_id"] == task_id and t["task"] == "http-done" for t in listing
    )

    assert client.get(f"{API}/dispatch/{'0' * 32}").status_code == 404
    assert client.get(f"{API}/dispatch/not-a-hex-id").status_code == 404
    assert client.get(f"{API}/dispatch/{'0' * 32}/log").status_code == 404


def test_http_claim_flag_409(dispatch_http) -> None:
    """A running task file with the same name blocks re-dispatch (409)."""
    client, env = dispatch_http
    task_id = "b" * 32
    dispatch_dir = env.state_dir / "dispatch"
    dispatch_dir.mkdir(parents=True, exist_ok=True)
    (dispatch_dir / f"{task_id}.json").write_text(
        json.dumps(
            {
                "task_id": task_id,
                "task": "held",
                "cli": "fakeq",
                "state": "running",
                "pid": 999999,
                "started_epoch": 1.0,
            }
        ),
        encoding="utf-8",
    )
    res = client.post(
        f"{API}/dispatch",
        json={"task": "held", "prompt": "x", "cli": "fakeq"},
    )
    assert res.status_code == 409
    assert "already running" in res.json()["detail"]
    assert task_id in res.json()["detail"]


def test_http_cli_not_installed_409(dispatch_http) -> None:
    """CLI not on PATH -> 409 with an install hint; default cli is qwen."""
    client, _ = dispatch_http
    res = client.post(
        f"{API}/dispatch",
        json={"task": "n1", "prompt": "x", "cli": "no-such-cli-zzz"},
    )
    assert res.status_code == 409
    assert "not found on PATH" in res.json()["detail"]

    res = client.post(
        f"{API}/dispatch",
        json={"task": "n2", "prompt": "x", "cli": "qwen"},
    )
    assert res.status_code == 409
    assert (
        "POST /api/qwen-code-bridge/qwen-code/install"
        in res.json()["detail"]
    )

    # no cli field -> default "qwen" applies (also not on this PATH)
    res = client.post(f"{API}/dispatch", json={"task": "d1", "prompt": "x"})
    assert res.status_code == 409
    assert "'qwen' not found on PATH" in res.json()["detail"]


def test_http_bad_cwd_400(dispatch_http) -> None:
    client, _ = dispatch_http
    res = client.post(
        f"{API}/dispatch",
        json={
            "task": "n3",
            "prompt": "x",
            "cli": "fakeq",
            "cwd": "/nonexistent-dir-xyz",
        },
    )
    assert res.status_code == 400
    assert "cwd is not a directory" in res.json()["detail"]


# ---- approval mode (#1379 parity) + wake approval context (10/22) ----


@pytest.mark.asyncio
async def test_approval_mode_override_reaches_argv(tmp_path) -> None:
    """approval_mode=PATCH_THROUGH: explicit tier lands in the spawn argv."""
    with Env(tmp_path) as env:
        args_file = env.tmp / "captured-args.txt"
        body = (
            'printf "%s\\n" "$@" > '
            f"{args_file}; "
            'echo \'{"type":"assistant","text":"ok"}\''
        )
        _make_bin(env.bin_dir, "fakeq", body)
        service = DispatchService()
        task_id = await service.start(
            task_name="t-approval",
            prompt="do it",
            cwd=None,
            cli="fakeq",
            timeout=30,
            wake=None,
            approval_mode="auto",
        )
        terminal = await _wait_terminal(service, task_id)
        assert terminal["state"] == "done"
        assert terminal["approval_mode"] == "auto"
        captured = args_file.read_text(encoding="utf-8").splitlines()
        idx = captured.index("--approval-mode")
        assert captured[idx + 1] == "auto"


@pytest.mark.asyncio
async def test_approval_mode_default_yolo_and_invalid(tmp_path, monkeypatch) -> None:
    """No request/env -> yolo; unknown tier -> ValueError before spawn."""
    import qwen_code_bridge.dispatch as dispatch_mod

    monkeypatch.setattr(dispatch_mod, "_effective_approval_level", lambda _a: "")
    with Env(tmp_path) as env:
        _make_bin(env.bin_dir, "fakeq", 'echo \'{"type":"result","result":"ok"}\'')
        service = DispatchService()
        task_id = await service.start(
            task_name="t-approval-default",
            prompt="do it",
            cwd=None,
            cli="fakeq",
            timeout=30,
            wake=None,
        )
        terminal = await _wait_terminal(service, task_id)
        assert terminal["approval_mode"] == "yolo"

        with pytest.raises(ValueError):
            await service.start(
                task_name="t-approval-bad",
                prompt="do it",
                cwd=None,
                cli="fakeq",
                timeout=30,
                wake=None,
                approval_mode="bogus",
            )


def test_resolve_approval_mode_precedence(monkeypatch) -> None:
    from qwen_code_bridge.dispatch import (
        APPROVAL_MODE_ENV,
        resolve_approval_mode,
    )

    monkeypatch.delenv(APPROVAL_MODE_ENV, raising=False)
    assert resolve_approval_mode(None) == "yolo"
    monkeypatch.setenv(APPROVAL_MODE_ENV, "auto")
    assert resolve_approval_mode(None) == "auto"
    assert resolve_approval_mode("plan") == "plan"


def test_wake_body_pins_approval_level() -> None:
    from qwen_code_bridge.dispatch import _wake_body

    body = _wake_body("sid-1", "3 lines", "SMART")
    assert body["request_context"] == {"approval_level": "SMART"}
    assert body["session_id"] == "sid-1"
    assert body["channel"] == "console"
    assert body["user_id"] == "default"
    m = _wake_body("matrix:!r:mx", "t", "", "agentteams_matrix", "!r:mx")
    assert m["channel"] == "agentteams_matrix"
    assert m["user_id"] == "!r:mx"
    assert _wake_body("sid-2", "t", "") == {
        "channel": "console",
        "user_id": "default",
        "session_id": "sid-2",
        "input": [{"content": [{"type": "text", "text": "t"}], "metadata": {}}],
    }


@pytest.mark.asyncio
async def test_wake_reuses_dispatch_session_level(tmp_path, monkeypatch) -> None:
    """The wake turn is pinned to the level the dispatch ran under.

    A request-level ``approval_level`` wins over the agent profile: it
    maps the CLI tier (STRICT -> plan), is recorded on the task, and is
    what the wake sender receives — the wake must not re-read the
    profile and drift away from the dispatch session's level.
    """
    import qwen_code_bridge.dispatch as dispatch_mod

    monkeypatch.setattr(dispatch_mod, "_effective_approval_level", lambda _a: "AUTO")
    with Env(tmp_path) as env:
        _make_bin(env.bin_dir, "fakeq", "echo ok")
        calls, sender = _wake_recorder()
        service = DispatchService(wake_sender=sender)
        task_id = await service.start(
            task_name="t-level",
            prompt="do",
            cwd=None,
            cli="fakeq",
            timeout=30,
            wake=dict(WAKE),
            approval_level="STRICT",
        )
        task = service.get_task(task_id)
        assert task is not None
        assert task["approval_level"] == "STRICT"
        assert task["approval_mode"] == "plan"
        assert task["approval_mode_source"] == "level-request"

        terminal = await _wait_terminal(service, task_id)
        assert terminal["state"] == "done"
        settled = await _wait_wake(service, task_id)
        assert settled["wake_status"] == "sent"
        assert len(calls) == 1
        assert calls[0]["approval_level"] == "STRICT"


@pytest.mark.asyncio
async def test_wake_level_defaults_to_recorded_profile_level(tmp_path, monkeypatch) -> None:
    """Without a request level the recorded profile level still rides the wake."""
    import qwen_code_bridge.dispatch as dispatch_mod

    monkeypatch.setattr(dispatch_mod, "_effective_approval_level", lambda _a: "AUTO")
    with Env(tmp_path) as env:
        _make_bin(env.bin_dir, "fakeq", "echo ok")
        calls, sender = _wake_recorder()
        service = DispatchService(wake_sender=sender)
        task_id = await service.start(
            task_name="t-level-default",
            prompt="do",
            cwd=None,
            cli="fakeq",
            timeout=30,
            wake=dict(WAKE),
        )
        task = service.get_task(task_id)
        assert task is not None
        assert task["approval_level"] == "AUTO"
        assert task["approval_mode_source"] == "level-map"
        await _wait_terminal(service, task_id)
        settled = await _wait_wake(service, task_id)
        assert settled["wake_status"] == "sent"
        assert calls[0]["approval_level"] == "AUTO"


def test_default_wake_sender_prefers_observed_session_level(monkeypatch) -> None:
    """The level last seen on the session beats the dispatch-time capture."""
    import qwen_code_bridge.dispatch as dispatch_mod
    import qwen_code_bridge.session_levels as levels_mod

    monkeypatch.setattr(levels_mod, "last_level", lambda _s: "OFF")
    monkeypatch.setattr(dispatch_mod, "_effective_approval_level", lambda _a: "AUTO")
    monkeypatch.setattr(dispatch_mod, "_pending_approval_line", lambda _b, _s: "")
    captured: list[dict] = []

    class _Resp:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self, _n: int = -1) -> bytes:
            return b'{"ok":true}'

    def fake_urlopen(req, timeout=0):  # noqa: ARG001
        captured.append(json.loads(req.data.decode("utf-8")))
        return _Resp()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)
    dispatch_mod.default_wake_sender(
        "sid-lvl", "AgentTeamGuide", "", "done", approval_level="SMART"
    )
    assert captured[0]["request_context"] == {"approval_level": "OFF"}

    # No observation -> live profile; capture only when the profile is unknown.
    monkeypatch.setattr(levels_mod, "last_level", lambda _s: "")
    captured.clear()
    dispatch_mod.default_wake_sender(
        "sid-lvl", "AgentTeamGuide", "", "done", approval_level="SMART"
    )
    assert captured[0]["request_context"] == {"approval_level": "AUTO"}

    captured.clear()
    monkeypatch.setattr(dispatch_mod, "_effective_approval_level", lambda _a: "")
    dispatch_mod.default_wake_sender(
        "sid-lvl", "AgentTeamGuide", "", "done", approval_level="SMART"
    )
    assert captured[0]["request_context"] == {"approval_level": "SMART"}


def test_default_wake_sender_carries_level_and_pending(monkeypatch) -> None:
    """Wake POST body: request_context level + pending approvals appended."""
    import qwen_code_bridge.dispatch as dispatch_mod

    monkeypatch.setattr(
        dispatch_mod,
        "_effective_approval_level",
        lambda _agent_id: "AUTO",
    )
    monkeypatch.setattr(
        dispatch_mod,
        "_pending_approval_line",
        lambda _base, _sid: (
            "待审批 1 条 | 最近: run_shell_command → ls -la "
            "(age 3s, medium) reason=No rule hit"
        ),
    )

    captured: list[dict] = []

    class _Resp:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self, _n: int = -1) -> bytes:
            return b'{"ok":true}'

    def fake_urlopen(req, timeout=0):  # noqa: ARG001
        captured.append(json.loads(req.data.decode("utf-8")))
        return _Resp()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)
    dispatch_mod.default_wake_sender("sid-9", "AgentTeamGuide", "", "line1\nline2\nline3")

    assert captured, "wake never POSTed"
    body = captured[0]
    assert body["request_context"] == {"approval_level": "AUTO"}
    text = body["input"][0]["content"][0]["text"]
    assert text.startswith("line1\nline2\nline3")
    assert "待审批 1 条" in text
    assert "最近: run_shell_command" in text and "ls -la" in text
    assert "唤醒回合请先处理" in text


def test_pending_approval_line_reads_operation(monkeypatch) -> None:
    """The wake line carries what the newest pending approval actually is."""
    import qwen_code_bridge.dispatch as dispatch_mod

    payload = {
        "pending_approvals": [
            {
                "tool_name": "run_shell_command",
                "tool_display_name": "run_shell_command",
                "exact_target": "rm -rf /tmp/x",
                "reasoning": "No rule hit",
                "severity": "high",
                "created_at": time.time() - 5,
            },
            {
                "tool_name": "write_file",
                "created_at": time.time() - 50,
            },
        ],
        "count": 2,
    }

    class _Resp:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self, _n: int = -1) -> bytes:
            return json.dumps(payload).encode("utf-8")

    monkeypatch.setattr(
        "urllib.request.urlopen",
        lambda req, timeout=0: _Resp(),  # noqa: ARG005
    )
    line = dispatch_mod._pending_approval_line("", "sid-x")
    assert "待审批 2 条" in line
    assert "最近: run_shell_command" in line
    assert "rm -rf /tmp/x" in line
    assert "reason=No rule hit" in line
    assert "其余 1 条: write_file" in line


def test_room_bell_posts_to_messages_send(monkeypatch) -> None:
    """matrix wake + room_bell: chat inject then POST /api/messages/send."""
    import qwen_code_bridge.dispatch as dispatch_mod

    calls: list[tuple[str, dict]] = []

    class _Resp:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self, _n: int = -1) -> bytes:
            return b'{"ok":true}'

    def fake_urlopen(req, timeout=0):  # noqa: ARG001
        calls.append((req.full_url, json.loads(req.data.decode("utf-8"))))
        return _Resp()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)
    monkeypatch.setattr(
        dispatch_mod,
        "_effective_approval_level",
        lambda _agent: "",
    )
    monkeypatch.setattr(
        dispatch_mod,
        "_pending_approval_line",
        lambda _base, _sid: "",
    )
    dispatch_mod.default_wake_sender(
        "matrix:!room:mx",
        "worker-a",
        "",
        "l1\nl2\nl3",
        "agentteams_matrix",
        "!room:mx",
        True,
    )
    urls = [u for u, _ in calls]
    assert any(u.endswith("/api/console/chat") for u in urls)
    bell = [b for u, b in calls if u.endswith("/api/messages/send")]
    assert bell, "room bell not posted"
    assert bell[0] == {
        "channel": "agentteams_matrix",
        "target_user": "!room:mx",
        "target_session": "matrix:!room:mx",
        "text": "l1\nl2\nl3",
    }


@pytest.mark.asyncio
async def test_origin_replays_source_session_identity(tmp_path) -> None:
    """origin (session_id,user_id,channel) wins over wake — channel-agnostic."""
    with Env(tmp_path) as env:
        _make_bin(env.bin_dir, "fakeq", 'echo \'{"type":"result","result":"ok"}\'')
        calls, sender = _wake_recorder()
        service = DispatchService(wake_sender=sender)
        task_id = await service.start(
            task_name="t-origin",
            prompt="do it",
            cwd=None,
            cli="fakeq",
            timeout=30,
            wake={"session_id": "fallback-sid", "channel": "console"},
            origin={
                "session_id": "matrix:!room:mx",
                "user_id": "!room:mx",
                "channel": "agentteams_matrix",
            },
        )
        await _wait_terminal(service, task_id)
        settled = await _wait_wake(service, task_id)
        assert settled["wake_status"] == "sent"
        assert calls, "wake not sent"
        assert calls[0]["session_id"] == "matrix:!room:mx"
        assert calls[0]["user_id"] == "!room:mx"
        assert calls[0]["channel"] == "agentteams_matrix"
        assert settled["origin"] == {
            "session_id": "matrix:!room:mx",
            "user_id": "!room:mx",
            "channel": "agentteams_matrix",
        }


@pytest.mark.asyncio
async def test_dispatch_creates_and_renames_sidebar_chat(tmp_path, monkeypatch) -> None:
    """P1: start creates a 'running' chat; terminal renames it with the state."""
    import qwen_code_bridge.dispatch as dispatch_mod

    events: list[tuple[str, str]] = []
    monkeypatch.setattr(
        dispatch_mod.chats,
        "ensure_group",
        lambda _root, _agent: "g-1",
    )
    monkeypatch.setattr(
        dispatch_mod.chats,
        "create_run_chat",
        lambda _root, _agent, **kw: (
            events.append(("create", kw["name"])) or "chat-1"
        ),
    )
    monkeypatch.setattr(
        dispatch_mod.chats,
        "rename_chat",
        lambda _root, _agent, _cid, name: (
            events.append(("rename", name)) or True
        ),
    )
    with Env(tmp_path) as env:
        monkeypatch.setenv("QWEN_CODE_BRIDGE_CHAT_SYNC", "1")
        _make_bin(env.bin_dir, "fakeq", 'echo \'{"type":"result","result":"ok"}\'')
        service = DispatchService()
        task_id = await service.start(
            task_name="t-sidebar",
            prompt="do it",
            cwd=None,
            cli="fakeq",
            timeout=30,
            wake=None,
        )
        terminal = await _wait_terminal(service, task_id)
        assert terminal["state"] == "done"
        for _ in range(50):
            if any(e[0] == "rename" for e in events):
                break
            await asyncio.sleep(0.05)
    assert ("create", "t-sidebar · fakeq · running") in events
    assert ("rename", "t-sidebar · fakeq · done") in events
    assert service.get_task(task_id)["chat_id"] == "chat-1"


def test_approval_level_mapping(monkeypatch) -> None:
    """QwenPaw levels map 1:1 onto Qwen Code tiers."""
    from qwen_code_bridge.dispatch import (
        map_level_to_mode,
        resolve_approval_mode,
    )

    assert map_level_to_mode("STRICT") == "plan"
    assert map_level_to_mode("SMART") == "auto-edit"
    assert map_level_to_mode("AUTO") == "auto"
    assert map_level_to_mode("OFF") == "yolo"
    assert map_level_to_mode("") == ""
    assert map_level_to_mode(None) == ""
    monkeypatch.delenv("QWEN_CODE_BRIDGE_APPROVAL_MODE", raising=False)
    monkeypatch.delenv("AGENTTEAMS_CODING_CLI_APPROVAL_MODE", raising=False)
    assert resolve_approval_mode(None, "AUTO") == "auto"
    assert resolve_approval_mode("plan", "AUTO") == "plan"
    monkeypatch.setenv("QWEN_CODE_BRIDGE_APPROVAL_MODE", "yolo")
    assert resolve_approval_mode(None, "STRICT") == "yolo"
    monkeypatch.delenv("QWEN_CODE_BRIDGE_APPROVAL_MODE", raising=False)
    assert resolve_approval_mode(None, None) == "yolo"


@pytest.mark.asyncio
async def test_dispatch_uses_mapped_level_when_no_explicit_mode(
    tmp_path, monkeypatch
) -> None:
    import qwen_code_bridge.dispatch as dispatch_mod

    monkeypatch.setattr(
        dispatch_mod,
        "_effective_approval_level",
        lambda _agent: "AUTO",
    )
    with Env(tmp_path) as env:
        args_file = env.tmp / "argv.txt"
        body = (
            'printf "%s\\n" "$@" > '
            f"{args_file}; "
            'echo \'{"type":"result","result":"ok"}\''
        )
        _make_bin(env.bin_dir, "fakeq", body)
        service = DispatchService()
        task_id = await service.start(
            task_name="t-map",
            prompt="do it",
            cwd=None,
            cli="fakeq",
            timeout=30,
            wake={"agent_id": "agent-x"},
        )
        terminal = await _wait_terminal(service, task_id)
        assert terminal["approval_level"] == "AUTO"
        assert terminal["approval_mode"] == "auto"
        assert terminal["approval_mode_source"] == "level-map"
        argv = args_file.read_text(encoding="utf-8").splitlines()
        assert argv[argv.index("--approval-mode") + 1] == "auto"


def test_build_spawn_plan_stream_mode():
    from qwen_code_bridge.dispatch import build_spawn_plan, user_message

    args, via_stdin = build_spawn_plan("default", "sid-1", "do it")
    assert via_stdin is True
    assert "--input-format" in args and "stream-json" in args
    assert "-p" not in args and "do it" not in args
    assert '--approval-mode' in args and 'default' in args
    payload = user_message("sid-1", "do it")
    assert payload.endswith("\n")
    import json

    msg = json.loads(payload)
    assert msg["type"] == "user" and msg["session_id"] == "sid-1"
    assert msg["message"]["role"] == "user"


def test_build_spawn_plan_yolo_compat():
    from qwen_code_bridge.dispatch import build_spawn_plan

    args, via_stdin = build_spawn_plan("yolo", "sid-2", "do it")
    assert via_stdin is False
    assert "-p" in args and "do it" in args
    assert "--input-format" not in args


def test_build_spawn_plan_resume_uses_resume_flag():
    from qwen_code_bridge.dispatch import build_spawn_plan

    args, via_stdin = build_spawn_plan("yolo", "sid-9", "p", resume=True)
    assert "--resume" in args and "sid-9" in args
    assert "--session-id" not in args
    args2, _ = build_spawn_plan("default", "sid-9", "p", resume=True)
    assert "--resume" in args2 and "--input-format" in args2


def test_scan_once_marks_orphan_stale(tmp_path, monkeypatch):
    import json, time
    from qwen_code_bridge.dispatch import DispatchService

    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    tid = "d" * 32
    (tmp_path / f"{tid}.json").write_text(json.dumps({
        "task_id": tid, "task": "t", "state": "running", "pid": 424242,
        "started_epoch": time.time() - 300, "timeout": 600,
        "origin": {"session_id": "s"}, "wake": {},
    }))
    monkeypatch.setattr(svc, "_pid_alive", lambda pid: False)
    changed = svc._scan_once()
    assert [t["task_id"] for t in changed] == [tid]
    assert changed[0]["state"] == "stale"


def test_chat_sync_switch_env_and_config(tmp_path, monkeypatch):
    from qwen_code_bridge import dispatch as d

    monkeypatch.setenv("QWEN_CODE_BRIDGE_CHAT_SYNC", "0")
    assert d.chat_sync_enabled() is False
    monkeypatch.delenv("QWEN_CODE_BRIDGE_CHAT_SYNC", raising=False)
    monkeypatch.setattr(d, "state_dir", lambda: tmp_path)
    (tmp_path / "config.json").write_text('{"chat_sync": false}', encoding="utf-8")
    assert d.chat_sync_enabled() is False
    (tmp_path / "config.json").write_text('{"chat_sync": true}', encoding="utf-8")
    assert d.chat_sync_enabled() is True


def test_cli_home_env_and_config(tmp_path, monkeypatch):
    from qwen_code_bridge import state

    monkeypatch.setattr(state, "state_dir", lambda: tmp_path)
    (tmp_path / "config.json").write_text('{"cli_home": "/persist/home"}', encoding="utf-8")
    assert state.cli_home() == "/persist/home"
    monkeypatch.setenv("QWEN_CODE_BRIDGE_CLI_HOME", "/env/home")
    assert state.cli_home() == "/env/home"


def test_prepare_cli_home_creates_qwen_dir(tmp_path):
    from qwen_code_bridge import state

    home = tmp_path / "home"
    qdir = state.prepare_cli_home(str(home))
    assert qdir == str(home / ".qwen")
    assert (home / ".qwen").is_dir()


def test_qwen_cli_env_and_config(tmp_path, monkeypatch):
    from qwen_code_bridge import state

    monkeypatch.setattr(state, "state_dir", lambda: tmp_path)
    (tmp_path / "config.json").write_text(
        '{"qwen_cli": "/app/working/.qwen-cli/bin/qwen"}', encoding="utf-8"
    )
    assert state.qwen_cli() == "/app/working/.qwen-cli/bin/qwen"
    monkeypatch.setenv("QWEN_CODE_BRIDGE_CLI", "/env/qwen")
    assert state.qwen_cli() == "/env/qwen"


def test_keep_running_after_timeout():
    """Liveness table: progress first, three-state probe as tie-breaker."""
    from qwen_code_bridge.dispatch import (
        LIVENESS_FINALIZE,
        LIVENESS_KEEP,
        LIVENESS_KILL,
        decide_liveness,
        keep_running_after_timeout,
    )

    # child exited -> finalize (never kill)
    assert (
        decide_liveness(
            proc_alive=False, progress_age=0, busy=None, stall_after=1800
        )
        == LIVENESS_FINALIZE
    )
    # fresh progress keeps running regardless of the probe
    assert keep_running_after_timeout(True, 10, 1800) is True
    assert keep_running_after_timeout(False, 10, 1800) is True
    assert keep_running_after_timeout(None, 10, 1800) is True
    # silent + probe says idle -> kill immediately
    assert keep_running_after_timeout(False, 2000, 1800) is False
    # silent + probe unknown -> one grace window, then kill
    assert (
        decide_liveness(
            proc_alive=True,
            progress_age=2000,
            busy=None,
            stall_after=1800,
            silent_extensions=0,
        )
        == LIVENESS_KEEP
    )
    assert (
        decide_liveness(
            proc_alive=True,
            progress_age=2000,
            busy=None,
            stall_after=1800,
            silent_extensions=1,
        )
        == LIVENESS_KILL
    )
    # hard cap wins over progress
    assert (
        decide_liveness(
            proc_alive=True,
            progress_age=1,
            busy=True,
            stall_after=1800,
            hard_cap_left=False,
        )
        == LIVENESS_KILL
    )


# ---------------------------------------------------------------------------
# 10/23 review round: liveness, stale-race, interaction, notification routing
# ---------------------------------------------------------------------------


def _write_task_file(tmp_path, task: dict) -> None:
    (tmp_path / f"{task['task_id']}.json").write_text(
        json.dumps(task), encoding="utf-8"
    )


def test_reaper_recovers_terminating_task_with_dead_child(tmp_path, monkeypatch):
    """#914: terminating + dead pid must be recovered, never skipped.

    The watchdog marks `terminating` before it kills the child; if it dies
    (or the plugin restarts) before writing the terminal state, the marker
    used to exempt the task forever and it stayed `running` for hours.
    """
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    tid = "a" * 32
    _write_task_file(tmp_path, {
        "task_id": tid, "task": "t", "state": "running", "pid": 424242,
        "started_epoch": time.time() - 300, "timeout": 600,
        "terminating": True, "updated_at": "2026-01-01T00:00:00Z",
        "watchdog_pid": 4243, "origin": {"session_id": "s"}, "wake": {},
    })
    monkeypatch.setattr(svc, "_pid_alive", lambda pid: False)
    changed = svc._scan_once()
    assert [t["task_id"] for t in changed] == [tid]
    assert changed[0]["state"] == "stale"
    assert changed[0]["death_reason"] == "timeout_killed_watchdog_lost"


def test_reaper_does_not_trust_watchdog_pid_without_freshness(tmp_path, monkeypatch):
    """A live watchdog_pid alone must not exempt a task forever.

    After a container/plugin restart the recorded pid is routinely reused
    by the new app process; the old guard skipped those tasks forever.
    """
    from qwen_code_bridge.dispatch import _utcnow

    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    tid = "b" * 32
    _write_task_file(tmp_path, {
        "task_id": tid, "task": "t", "state": "running", "pid": 424242,
        "started_epoch": time.time() - 300, "timeout": 600,
        "updated_at": "2026-01-01T00:00:00Z",  # stale file
        "watchdog_pid": 4243, "origin": {"session_id": "s"}, "wake": {},
    })
    # child alive, "watchdog" alive (reused pid) but the file is stale ->
    # the reaper must not skip on the watchdog pid alone.
    monkeypatch.setattr(svc, "_pid_alive", lambda pid: int(pid) == 424242)
    changed = svc._scan_once()
    assert changed == []  # age 300s < timeout+60 -> nothing to kill yet
    # and it must NOT have been skipped silently: the wall/timeout path ran
    # (proven by it reaching the kill branch for an over-budget task)
    _write_task_file(tmp_path, {
        "task_id": tid, "task": "t", "state": "running", "pid": 424242,
        "started_epoch": time.time() - 700, "timeout": 600,
        "updated_at": "2026-01-01T00:00:00Z",
        "watchdog_pid": 4243, "origin": {"session_id": "s"}, "wake": {},
    })
    changed = svc._scan_once()
    assert [t["task_id"] for t in changed] == [tid]
    assert changed[0]["death_reason"] == "timeout_killed_by_reaper"


def test_pid_alive_treats_zombie_as_dead(tmp_path):
    """A zombie keeps the pid and answers os.kill(pid, 0); it is dead."""
    import subprocess as _sp

    proc = _sp.Popen(["/bin/sh", "-c", "exit 0"])
    try:
        time.sleep(0.3)  # exits; not reaped -> zombie
        assert DispatchService._pid_alive(proc.pid) is False
    finally:
        proc.wait()


def test_pid_alive_detects_live_process(tmp_path):
    import subprocess as _sp

    proc = _sp.Popen(["/bin/sh", "-c", "sleep 30"])
    try:
        time.sleep(0.1)
        assert DispatchService._pid_alive(proc.pid) is True
    finally:
        proc.kill()
        proc.wait()
    assert DispatchService._pid_alive(proc.pid) is False


def test_reaper_skips_recently_updated_task(tmp_path, monkeypatch):
    from qwen_code_bridge.dispatch import _utcnow
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    tid = "b" * 32
    _write_task_file(tmp_path, {
        "task_id": tid, "task": "t", "state": "running", "pid": 424242,
        "started_epoch": time.time() - 300, "timeout": 600,
        "updated_at": _utcnow(), "origin": {"session_id": "s"}, "wake": {},
    })
    monkeypatch.setattr(svc, "_pid_alive", lambda pid: False)
    assert svc._scan_once() == []


def test_stale_death_reason_recorded(tmp_path, monkeypatch):
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    tid = "c" * 32
    _write_task_file(tmp_path, {
        "task_id": tid, "task": "t", "state": "running", "pid": 424242,
        "started_epoch": time.time() - 300, "timeout": 600,
        "updated_at": "2026-01-01T00:00:00Z",
        "origin": {"session_id": "s"}, "wake": {},
    })
    monkeypatch.setattr(svc, "_pid_alive", lambda pid: False)
    changed = svc._scan_once()
    assert [t["task_id"] for t in changed] == [tid]
    assert changed[0]["state"] == "stale"
    assert changed[0]["death_reason"] == "crash_or_restart"
    assert "崩溃或重启" in changed[0]["terminal_summary"][0]


def test_wake_is_single_writer(tmp_path):
    import asyncio as _asyncio
    from qwen_code_bridge.dispatch import _utcnow

    calls: list[str] = []

    def sender(session_id, agent_id, base_url, text, *a, **k):
        calls.append(text)

    svc = DispatchService(dispatch_dir=tmp_path, wake_sender=sender)
    tid = "d" * 32
    _write_task_file(tmp_path, {
        "task_id": tid, "task": "t", "state": "timeout",
        "wake_status": "sent", "updated_at": _utcnow(),
        "origin": {"session_id": "s"}, "wake": {}, "wall_s": 1,
    })
    task = json.loads((tmp_path / f"{tid}.json").read_text())
    _asyncio.get_event_loop_policy().new_event_loop().run_until_complete(
        svc._notify_external(task)
    )
    assert calls == []  # already sent -> never a second wake


def test_git_dirty_summary(tmp_path):
    from qwen_code_bridge.dispatch import git_dirty_summary

    assert git_dirty_summary(None) == ""
    repo = tmp_path / "repo"
    repo.mkdir()
    os.system(f"git -C {repo} init -q")
    assert git_dirty_summary(str(repo)) == ""
    (repo / "a.txt").write_text("x", encoding="utf-8")
    summary = git_dirty_summary(str(repo))
    assert summary.startswith("1 个未提交改动")
    assert "a.txt" in summary


def test_notify_target_routing(monkeypatch):
    from qwen_code_bridge import notify

    monkeypatch.delenv(notify.NOTIFY_ENV, raising=False)
    assert notify.notify_target({"channel": "console"}) == "inbox"
    assert notify.notify_target({"channel": "matrix"}) == "room"
    monkeypatch.setenv(notify.NOTIFY_ENV, "inbox")
    assert notify.notify_target({"channel": "matrix"}) == "inbox"
    monkeypatch.setenv(notify.NOTIFY_ENV, "room")
    assert notify.notify_target({"channel": "console"}) == "room"


def test_goals_disabled_written_once(tmp_path, monkeypatch):
    from qwen_code_bridge import dispatch as d

    home = tmp_path / "home"
    (home / ".qwen").mkdir(parents=True)
    settings = home / ".qwen" / "settings.json"
    settings.write_text('{"model": {"name": "x"}}', encoding="utf-8")
    monkeypatch.setattr(d, "cli_home", lambda: str(home))
    monkeypatch.setattr(d, "prepare_cli_home", lambda h: str(home / ".qwen"))
    assert d.ensure_dispatch_goals_disabled() is True
    data = json.loads(settings.read_text())
    assert data["goals"]["modelProposed"] == "disabled"
    assert data["model"]["name"] == "x"  # other keys preserved
    before = settings.read_text()
    assert d.ensure_dispatch_goals_disabled() is True  # idempotent
    assert settings.read_text() == before


def _ask_cli(bin_dir: Path, stdin_log: Path) -> None:
    payload = {
        "type": "control_request",
        "request_id": "r1",
        "request": {
            "subtype": "can_use_tool",
            "tool_name": "ask_user_question",
            "input": {
                "questions": [
                    {
                        "header": "H",
                        "question": "Pick one",
                        "options": [
                            {"label": "A", "description": "a"},
                            {"label": "B", "description": "b"},
                        ],
                    }
                ]
            },
        },
    }
    script = (
        "#!/bin/sh\n"
        f"echo '{json.dumps(payload)}'\n"
        f'while IFS= read -r line; do printf "%s\\n" "$line" >> "{stdin_log}"; done\n'
    )
    bin_dir.mkdir(parents=True, exist_ok=True)
    path = bin_dir / "fakeask"
    path.write_text(script, encoding="utf-8")
    path.chmod(0o755)


async def _wait_pending(service, task_id, tries=200):
    for _ in range(tries):
        pending = service.pending_questions(task_id)
        if pending:
            return pending
        await asyncio.sleep(0.05)
    raise AssertionError("question never became pending")


async def _wait_stdin(stdin_log: Path, needle: str, tries=200) -> str:
    text = ""
    for _ in range(tries):
        text = stdin_log.read_text(errors="replace") if stdin_log.exists() else ""
        if needle in text:
            return text
        await asyncio.sleep(0.05)
    return text


@pytest.mark.asyncio
async def test_ask_user_question_notifies_and_answer_resolves(tmp_path, monkeypatch):
    from qwen_code_bridge import notify as notify_mod

    monkeypatch.setenv("QWEN_CODE_BRIDGE_HOLD_SECONDS", "10")
    monkeypatch.setenv("QWEN_CODE_BRIDGE_AGENT_GRACE", "5")
    notified: list[dict] = []

    async def fake_notify(**kwargs):
        notified.append(kwargs)
        return "inbox"

    monkeypatch.setattr(notify_mod, "notify_human", fake_notify)
    with Env(tmp_path) as env:
        stdin_log = tmp_path / "stdin.log"
        _ask_cli(env.bin_dir, stdin_log)
        service = DispatchService(wake_sender=lambda *a, **k: None)
        task_id = await service.start(
            task_name="t-ask", prompt="x", cwd=None, cli="fakeask",
            timeout=30, wake=dict(WAKE), approval_mode="default",
        )
        pending = await _wait_pending(service, task_id)
        assert pending[0]["tool_name"] == "ask_user_question"
        assert "Pick one" in pending[0]["question"]
        assert notified and notified[0]["title"]
        assert service.answer_question(task_id, "r1", {"0": "A"}, by="human")
        text = await _wait_stdin(stdin_log, "updatedInput")
        assert "updatedInput" in text and '"A"' in text
        assert service.pending_questions(task_id) == []


@pytest.mark.asyncio
async def test_ask_user_question_timeout_falls_back_to_default_deny(
    tmp_path, monkeypatch
):
    from qwen_code_bridge import notify as notify_mod

    monkeypatch.setenv("QWEN_CODE_BRIDGE_HOLD_SECONDS", "0.2")
    monkeypatch.setenv("QWEN_CODE_BRIDGE_AGENT_GRACE", "0.2")

    async def fake_notify(**kwargs):
        return "inbox"

    monkeypatch.setattr(notify_mod, "notify_human", fake_notify)
    with Env(tmp_path) as env:
        stdin_log = tmp_path / "stdin.log"
        _ask_cli(env.bin_dir, stdin_log)
        service = DispatchService(wake_sender=lambda *a, **k: None)
        task_id = await service.start(
            task_name="t-ask-timeout", prompt="x", cwd=None, cli="fakeask",
            timeout=30, wake=dict(WAKE), approval_mode="default",
        )
        await _wait_pending(service, task_id)
        text = await _wait_stdin(stdin_log, "most reasonable assumption")
        assert "most reasonable assumption" in text
        assert '"behavior": "deny"' in text or '"behavior":"deny"' in text


def test_pending_and_answer_endpoints(tmp_path):
    from qwen_code_bridge import interaction

    with Env(tmp_path) as env:
        app = FastAPI()
        app.include_router(build_router(), prefix=API)
        client = TestClient(app)
        tid = "e" * 32
        dispatch_dir = env.state_dir / "dispatch"
        dispatch_dir.mkdir(parents=True, exist_ok=True)
        _write_task_file(dispatch_dir, {
            "task_id": tid, "task": "t", "state": "running", "pid": 1,
            "started_epoch": time.time(), "timeout": 60,
            "updated_at": "2026-01-01T00:00:00Z",
            "origin": {"session_id": "s"}, "wake": {},
        })
        store = interaction.store_for(str(dispatch_dir))
        store.create(
            task_id=tid,
            request_id="r1",
            tool_name="ask_user_question",
            tool_input={"questions": [{"header": "H", "question": "Q?",
                                       "options": [{"label": "A"},
                                                   {"label": "B"}]}]},
        )
        resp = client.get(f"{API}/dispatch/{tid}/pending")
        assert resp.status_code == 200
        assert resp.json()["count"] == 1
        resp = client.post(
            f"{API}/dispatch/{tid}/answer",
            json={"request_id": "r1", "choice": "2"},
        )
        assert resp.status_code == 200
        assert resp.json()["answers"] == {"0": "B"}
        assert client.get(f"{API}/dispatch/{tid}/pending").json()["count"] == 0
        resp = client.post(
            f"{API}/dispatch/{tid}/answer",
            json={"request_id": "r1", "choice": "A"},
        )
        assert resp.status_code == 404


def test_cleanup_maps_terminating_to_watchdog_lost(tmp_path, monkeypatch):
    """#914: cleanup must report the watchdog-lost reason, not a crash."""
    svc = DispatchService(dispatch_dir=tmp_path, watch_interval=0.05)
    tid = "f" * 32
    _write_task_file(tmp_path, {
        "task_id": tid, "task": "t", "state": "running", "pid": 424242,
        "started_epoch": time.time() - 300, "timeout": 600,
        "terminating": True, "updated_at": "2026-01-01T00:00:00Z",
        "origin": {"session_id": "s"}, "wake": {},
    })
    monkeypatch.setattr(svc, "_pid_alive", lambda pid: False)
    result = svc.cleanup()
    assert result["count"] == 1
    task = json.loads((tmp_path / f"{tid}.json").read_text())
    assert task["state"] == "stale"
    assert task["death_reason"] == "timeout_killed_watchdog_lost"


def _ctrl_cli(bin_dir: Path, stdin_log: Path) -> None:
    """Fake CLI issuing four can_use_tool requests: an outside-root path
    (deny), a double-quoted command substitution (deny, exec-compound),
    and two that must be allowed (git status, git add)."""
    deny_req = {
        "type": "control_request",
        "request_id": "r-deny",
        "request": {
            "subtype": "can_use_tool",
            "tool_name": "shell",
            "input": {"command": "cat /var/private/data"},
        },
    }
    dq_deny_req = {
        "type": "control_request",
        "request_id": "r-dq-deny",
        "request": {
            "subtype": "can_use_tool",
            "tool_name": "run_shell_command",
            "input": {"command": 'git commit -m "$(id)"'},
        },
    }
    allow_req = {
        "type": "control_request",
        "request_id": "r-allow",
        "request": {
            "subtype": "can_use_tool",
            "tool_name": "shell",
            "input": {"command": "git status"},
        },
    }
    dq_allow_req = {
        "type": "control_request",
        "request_id": "r-dq-allow",
        "request": {
            "subtype": "can_use_tool",
            "tool_name": "run_shell_command",
            "input": {"command": "git add file"},
        },
    }
    script = (
        "#!/bin/sh\n"
        f"echo '{json.dumps(deny_req)}'\n"
        f"echo '{json.dumps(dq_deny_req)}'\n"
        f"echo '{json.dumps(allow_req)}'\n"
        f"echo '{json.dumps(dq_allow_req)}'\n"
        f'while IFS= read -r line; do printf "%s\\n" "$line" >> "{stdin_log}"; done\n'
    )
    bin_dir.mkdir(parents=True, exist_ok=True)
    path = bin_dir / "fakectrl"
    path.write_text(script, encoding="utf-8")
    path.chmod(0o755)


@pytest.mark.asyncio
async def test_control_response_deny_not_allow(tmp_path):
    """#1411: a deny decision must reach the CLI as behavior=deny with a
    message; allow may only be emitted for policy-clean commands.
    Includes the double-quote bypass: ``git commit -m "$(id)"`` must be
    denied with exec-compound, while plain ``git add`` keeps flowing."""
    with Env(tmp_path) as env:
        stdin_log = tmp_path / "stdin.log"
        _ctrl_cli(env.bin_dir, stdin_log)
        service = DispatchService(wake_sender=lambda *a, **k: None)
        task_id = await service.start(
            task_name="t-ctrl", prompt="x", cwd=None, cli="fakectrl",
            timeout=30, wake=dict(WAKE), approval_mode="default",
        )
        text = ""
        for _ in range(200):
            text = stdin_log.read_text(errors="replace") if stdin_log.exists() else ""
            if all(f'"r-{k}"' in text for k in ("deny", "dq-deny", "allow", "dq-allow")):
                break
            await asyncio.sleep(0.05)
        deny_line = next(l for l in text.splitlines() if '"r-deny"' in l)
        allow_line = next(l for l in text.splitlines() if '"r-allow"' in l)
        dq_deny_line = next(l for l in text.splitlines() if '"r-dq-deny"' in l)
        dq_allow_line = next(l for l in text.splitlines() if '"r-dq-allow"' in l)
        deny_resp = json.loads(deny_line)["response"]["response"]
        allow_resp = json.loads(allow_line)["response"]["response"]
        dq_deny_resp = json.loads(dq_deny_line)["response"]["response"]
        dq_allow_resp = json.loads(dq_allow_line)["response"]["response"]
        assert deny_resp["behavior"] == "deny"
        assert deny_resp["message"]
        assert allow_resp["behavior"] == "allow"
        assert dq_deny_resp["behavior"] == "deny"
        assert "exec-compound" in dq_deny_resp["message"]
        assert dq_allow_resp["behavior"] == "allow"
