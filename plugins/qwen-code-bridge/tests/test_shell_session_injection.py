# -*- coding: utf-8 -*-
"""P2 acceptance tests: session-injection middleware (design doc §5).

P1 ships only the management surface. The session-injection middleware
(``qwen_code_bridge.middleware.SessionEnvMiddleware``) lands in
P2. The T0 spike (``spike-report.md``, ``spike/test_t0_spike.py``) has
already verified both premises — H1 (an ``on_acting`` input mutation
reaches the tool execution layer) and H2 (the factory's
``ctx.session_id`` is populated on the shell tool execution path),
6/6 green.

These tests are the P2 acceptance criteria and are marked
``xfail(strict=True)``: they xfail while
``qwen_code_bridge/middleware.py`` does not exist, and should go
green in P2 without test changes once the module is implemented
(remove the marker at that time).

Note: upstream PR 8156's file of this name tests the core contextvar
injection path (``qwenpaw.agents.tools.shell``). The plugin does not
modify core — injection is a plugin middleware per design doc §5 — so
the upstream version is intentionally not ported.

Contract assumed of the P2 module (design doc §5 naming): a
``SessionEnvMiddleware`` class constructed with the request
``HookContext``, whose ``on_acting`` prepends
``export QWENPAW_SESSION_ID='<ctx.session_id>' &&`` to shell commands
when the variable is not already present (idempotent).
"""

import json
from pathlib import Path
from types import SimpleNamespace
from typing import Any, AsyncGenerator

import pytest
from agentscope.agent import Agent
from agentscope.message import (
    TextBlock,
    ToolCallBlock,
    ToolCallState,
    ToolResultState,
)
from agentscope.middleware import MiddlewareBase
from agentscope.state import AgentState
from agentscope.tool import FunctionTool, Toolkit, ToolChunk


RECEIVED: dict[str, Any] = {}

SHELL_INPUT_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {"command": {"type": "string"}},
    "required": ["command"],
}


async def fake_shell(command: str) -> AsyncGenerator[ToolChunk, None]:
    """Fake execute_shell_command: record the command it actually gets."""
    RECEIVED["command"] = command
    yield ToolChunk(
        is_last=True,
        state=ToolResultState.SUCCESS,
        content=[TextBlock(type="text", text="ok")],
    )


def _make_agent(middleware: MiddlewareBase) -> Agent:
    """Same driver as the T0 spike: real ``_acting`` chain, no model."""
    toolkit = Toolkit(
        tools=[
            FunctionTool(
                fake_shell,
                name="execute_shell_command",
                description="fake shell for P2 session injection",
                input_schema=SHELL_INPUT_SCHEMA,
            ),
        ],
    )
    agent = Agent.__new__(Agent)
    agent._acting_middlewares = [middleware]
    agent.toolkit = toolkit
    agent.state = AgentState()
    agent.state.tool_context.activated_groups = ["basic"]
    return agent


async def _drive_agent(agent: Agent, command: str = "echo hi") -> str:
    RECEIVED.clear()
    tool_call = ToolCallBlock(
        id="call-1",
        name="execute_shell_command",
        input=json.dumps({"command": command}),
        state=ToolCallState.ALLOWED,
    )
    async for _ in agent._acting(tool_call):
        pass
    return RECEIVED["command"]


def _make_ctx(session_id: str, workspace_dir: Path) -> Any:
    """Real HookContext (same field wiring as runtime._build_context)."""
    from qwenpaw.runtime.hooks import HookContext
    from qwenpaw.schemas import AgentRequest

    request = AgentRequest(
        input=[{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
        session_id=session_id,
    )
    return HookContext(
        request=request,
        session_id=session_id,
        agent_id="worker-1",
        root_session_id=session_id,
        root_agent_id="worker-1",
        workspace_dir=workspace_dir,
        workspace=SimpleNamespace(workspace_dir=str(workspace_dir)),
        app_services=SimpleNamespace(tool_coordinator=None),
    )


@pytest.mark.asyncio
async def test_session_id_injected_into_shell_command(tmp_path) -> None:
    """Closed loop: ctx.session_id → middleware → shell tool command."""
    from qwen_code_bridge.middleware import (  # noqa: F401  (P2)
        SessionEnvMiddleware,
    )

    session_id = "matrix:!room42:matrix.example"
    mw = SessionEnvMiddleware(_make_ctx(session_id, tmp_path))
    received = await _drive_agent(
        _make_agent(mw),
        command="qwen --version",
    )
    assert (
        received
        == f"export QWENPAW_SESSION_ID='{session_id}' && qwen --version"
    )


@pytest.mark.asyncio
async def test_injection_is_idempotent(tmp_path) -> None:
    """A command that already sets QWENPAW_SESSION_ID is not re-prefixed."""
    from qwen_code_bridge.middleware import (  # noqa: F401  (P2)
        SessionEnvMiddleware,
    )

    session_id = "matrix:!room42:matrix.example"
    mw = SessionEnvMiddleware(_make_ctx(session_id, tmp_path))
    pre = "export QWENPAW_SESSION_ID='pre-existing' && echo hi"
    received = await _drive_agent(_make_agent(mw), command=pre)
    assert received == pre
