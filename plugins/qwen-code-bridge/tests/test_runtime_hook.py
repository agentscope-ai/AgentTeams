# -*- coding: utf-8 -*-
"""PRE_DISPATCH hook: zero-prefix direct chat + run-chat status view."""

# pylint: disable=redefined-outer-name
from __future__ import annotations

import pytest

from qwen_code_bridge import runtime_hook as hook_mod


class _Ctx:
    def __init__(self, session_id: str, text: str = "hello") -> None:
        from agentscope.message import Msg, TextBlock

        self.session_id = session_id
        self.agent_id = "agent-x"
        self.workspace_dir = ""
        self.input_msgs = [
            Msg(
                name="user",
                role="user",
                content=[TextBlock(type="text", text=text)],
            ),
        ]


@pytest.mark.asyncio
async def test_other_sessions_pass_through() -> None:
    result = await hook_mod.BridgeChatHook().run(_Ctx("matrix:!room:mx"))
    assert result.action.value == "continue"


@pytest.mark.asyncio
async def test_direct_chat_runs_cli_and_persists(monkeypatch) -> None:
    calls: dict = {}

    async def fake_run(prompt, **kw):
        calls["prompt"] = prompt
        calls["kw"] = kw
        return "DIRECT-ANSWER"

    async def fake_persist(ctx, text, answer):
        calls["persist"] = (text, answer)

    monkeypatch.setattr(hook_mod, "run_cli", fake_run)
    monkeypatch.setattr(hook_mod, "_persist_turn", fake_persist)
    result = await hook_mod.BridgeChatHook().run(
        _Ctx("qwen-code-bridge:direct", "hi there"),
    )
    assert result.action.value == "short_circuit"
    assert result.payload.content[0].text == "DIRECT-ANSWER"
    assert calls["prompt"] == "hi there"
    assert calls["persist"] == ("hi there", "DIRECT-ANSWER")


@pytest.mark.asyncio
async def test_run_chat_renders_status_card(monkeypatch) -> None:
    task = {
        "task": "demo-task",
        "cli": "qwen",
        "state": "done",
        "exit_code": 0,
        "wall_s": 12.5,
        "approval_mode": "auto",
        "log_tail": '{"type":"result","result":"OK"}\n',
    }
    monkeypatch.setattr(hook_mod, "load_task", lambda _tid: task)

    async def fake_persist(ctx, text, answer):
        return None

    monkeypatch.setattr(hook_mod, "_persist_turn", fake_persist)
    session = "qwen-code-bridge:" + "a" * 32
    result = await hook_mod.BridgeChatHook().run(_Ctx(session, "refresh"))
    assert result.action.value == "short_circuit"
    text = result.payload.content[0].text
    assert "demo-task" in text and "done" in text and "exit=0" in text
    assert '{"type":"result"' in text


@pytest.mark.asyncio
async def test_run_chat_missing_task(monkeypatch) -> None:
    monkeypatch.setattr(hook_mod, "load_task", lambda _tid: None)

    async def fake_persist(ctx, text, answer):
        return None

    monkeypatch.setattr(hook_mod, "_persist_turn", fake_persist)
    session = "qwen-code-bridge:" + "b" * 32
    result = await hook_mod.BridgeChatHook().run(_Ctx(session, "x"))
    assert "找不到派单记录" in result.payload.content[0].text
