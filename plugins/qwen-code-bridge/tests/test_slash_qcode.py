# -*- coding: utf-8 -*-
"""`/qcode` slash command tests: usage, CLI bridge, session continuity."""

# pylint: disable=redefined-outer-name
from __future__ import annotations

import pytest

from qwen_code_bridge import slash


def _write_stub(path, answer: str) -> None:
    path.write_text(
        "#!/bin/sh\n"
        f'printf "%s\\n" "$@" > {path.parent}/argv.txt\n'
        f'echo \'{{"type":"result","result":"{answer}","is_error":false}}\'\n',
        encoding="utf-8",
    )
    path.chmod(0o755)


@pytest.fixture
def stub(tmp_path, monkeypatch):
    path = tmp_path / "qwen"
    _write_stub(path, "STUB-ANSWER")
    monkeypatch.setenv(slash.CLI_ENV, str(path))
    monkeypatch.setenv(slash.TIMEOUT_ENV, "20")
    monkeypatch.setenv("QWEN_CODE_BRIDGE_STATE_DIR", str(tmp_path / "state"))
    return path


def _argv(stub_path) -> list[str]:
    args = stub_path.parent / "argv.txt"
    return args.read_text(encoding="utf-8").splitlines() if args.exists() else []


@pytest.mark.asyncio
async def test_usage_when_empty() -> None:
    handler = slash.make_handler()

    class Ctx:
        session_id = "s-usage"
        workspace_dir = ""

    msg = await handler(Ctx(), "")
    assert "用法" in msg.content[0].text


@pytest.mark.asyncio
async def test_first_turn_creates_cli_session(stub, tmp_path) -> None:
    answer = await slash.run_cli("hello", cwd=str(tmp_path), qwenpaw_session_id="s1")
    assert answer == "STUB-ANSWER"
    argv = _argv(stub)
    assert "--session-id" in argv and "--chat-recording" in argv
    assert "-r" not in argv
    cli_sid = slash.load_cli_session("s1")
    assert cli_sid and cli_sid in argv


@pytest.mark.asyncio
async def test_second_turn_resumes_cli_session(stub, tmp_path) -> None:
    await slash.run_cli("first", cwd=str(tmp_path), qwenpaw_session_id="s2")
    first_sid = slash.load_cli_session("s2")
    assert first_sid
    _write_stub(stub, "RESUMED")
    answer = await slash.run_cli("second", cwd=str(tmp_path), qwenpaw_session_id="s2")
    assert answer == "RESUMED"
    argv = _argv(stub)
    assert "-r" in argv
    assert argv[argv.index("-r") + 1] == first_sid
    assert "--session-id" not in argv


@pytest.mark.asyncio
async def test_handler_returns_msg(stub, tmp_path) -> None:
    handler = slash.make_handler()

    class Ctx:
        session_id = "s-handler"
        workspace_dir = str(tmp_path)

    msg = await handler(Ctx(), "what is up")
    assert msg.content[0].text == "STUB-ANSWER"
    assert getattr(msg, "name", "") == "qwen-code"


@pytest.mark.asyncio
async def test_missing_cli_reports_error(monkeypatch, tmp_path) -> None:
    monkeypatch.setenv(slash.CLI_ENV, str(tmp_path / "does-not-exist"))
    monkeypatch.setenv("QWEN_CODE_BRIDGE_STATE_DIR", str(tmp_path / "state"))
    answer = await slash.run_cli("x", cwd=str(tmp_path), qwenpaw_session_id="s3")
    assert "启动失败" in answer


@pytest.mark.asyncio
async def test_handler_persists_turn_into_session(stub, tmp_path) -> None:
    """The slash turn is appended to agent.state.context (chat history)."""
    recorded: dict = {"saves": []}

    class FakeSession:
        async def get_session_state_dict(self, session_id, user_id="", channel=""):
            recorded["load"] = (session_id, user_id, channel)
            return {}

        async def update_session_state(self, session_id, key, value, user_id="", channel=""):
            recorded["saves"].append((session_id, key, value, user_id, channel))

    class FakeWorkspace:
        session = FakeSession()

    class FakeRequest:
        user_id = "default"
        channel = "qwen-code-bridge"

    class Ctx:
        session_id = "qwen-code-bridge:direct"
        workspace_dir = str(tmp_path)
        workspace = FakeWorkspace()
        request = FakeRequest()

    handler = slash.make_handler()
    msg = await handler(Ctx(), "hello cli")
    assert msg.content[0].text == "STUB-ANSWER"
    saves = recorded["saves"]
    assert len(saves) == 2
    (sid_u, key_u, val_u, uid_u, ch_u) = saves[0]
    (sid_a, key_a, val_a, uid_a, ch_a) = saves[1]
    assert sid_u == sid_a == "qwen-code-bridge:direct"
    assert key_u == key_a == "agent.state"
    assert uid_u == uid_a == "default" and ch_u == ch_a == "qwen-code-bridge"
    assert val_u["context"][-1]["role"] == "user"
    assert val_u["context"][-1]["content"][0]["text"] == "hello cli"
    assert val_a["context"][-1]["role"] == "assistant"
    assert val_a["context"][-1]["content"][0]["text"] == "STUB-ANSWER"


@pytest.mark.asyncio
async def test_slash_uses_mapped_level(stub, tmp_path, monkeypatch) -> None:
    """The slash turn runs the CLI with the mapped approval tier."""
    import qwen_code_bridge.slash as slash_mod

    monkeypatch.setattr(
        slash_mod,
        "_effective_approval_level",
        lambda _agent: "STRICT",
    )
    answer = await slash.run_cli(
        "x",
        cwd=str(tmp_path),
        qwenpaw_session_id="s-map",
        agent_id="agent-x",
    )
    assert answer == "STUB-ANSWER"
    argv = _argv(stub)
    assert argv[argv.index("--approval-mode") + 1] == "plan"
