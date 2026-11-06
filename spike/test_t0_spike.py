# -*- coding: utf-8 -*-
"""T0 spike: middleware 会话注入可行性（设计文档 §5，P0 交付）。

H1: on_acting 里修改 ``input_kwargs["tool_call"]`` 的 input 字段后调用
    ``next_handler()``，tool 执行层（``Toolkit.call_tool`` → 工具函数 kwargs）
    收到的是改后的值？
H2: ``factory(ctx, agent_config)`` 的 ``ctx`` 在 shell 工具执行链上
    ``session_id`` 有值？

驱动方式（不用真 LLM，参照 SC/QwenPaw/tests 现有 middleware 测试的驱动方式）：
- H1 直接驱动 ``Agent._acting(tool_call)`` —— 这是 ``on_acting`` 中间件包裹的
  纯执行层入口（``_acting_impl`` → ``toolkit.call_tool``），无模型调用。
- ``Toolkit`` + ``FunctionTool`` 为真实 agentscope 执行层；``fake_shell`` 是
  假 shell 工具，只记录它实际收到的 ``command`` kwarg。
- H2 走真实注册链路：``PluginRegistry``（单例）.register_middleware →
  ``AgentBuilder._build_middlewares(ctx, agent_config)``（builder.py L1420
  ``reg.factory(ctx, agent_config)``），ctx 为真实 ``HookContext``。
"""

from __future__ import annotations

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


# ---------------------------------------------------------------------------
# 假 shell 工具 + 真实执行层
# ---------------------------------------------------------------------------

#: 记录 fake_shell 实际收到的 command（测试间清空）。
RECEIVED: dict[str, Any] = {}

SHELL_INPUT_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {"command": {"type": "string"}},
    "required": ["command"],
}


async def fake_shell(command: str) -> AsyncGenerator[ToolChunk, None]:
    """假 execute_shell_command：不跑子进程，只记录收到的 command。"""
    RECEIVED["command"] = command
    yield ToolChunk(
        is_last=True,
        state=ToolResultState.SUCCESS,
        content=[TextBlock(type="text", text="ok")],
    )


def _make_agent(middleware: MiddlewareBase) -> Agent:
    """绕过 __init__（不需要 model），只装配 _acting 链所需三件套。"""
    toolkit = Toolkit(
        tools=[
            FunctionTool(
                fake_shell,
                name="execute_shell_command",
                description="fake shell for spike",
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
    """驱动一次 shell 工具调用（真实 _acting 链），返回工具收到的 command。"""
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


# ---------------------------------------------------------------------------
# H1: on_acting 修改 input 是否传到执行层
# ---------------------------------------------------------------------------


class _InPlaceObjectMiddleware(MiddlewareBase):
    """策略 A：json.loads(tc.input) → 改字段 → json.dumps 原地写回 tc.input。"""

    async def on_acting(
        self,
        agent: Any,
        input_kwargs: dict,
        next_handler: Any,
    ) -> AsyncGenerator:
        tc = input_kwargs["tool_call"]
        data = json.loads(tc.input)
        data["command"] = (
            f"export QWENPAW_SESSION_ID='spike-sid' && {data['command']}"
        )
        tc.input = json.dumps(data)
        async for ev in next_handler():
            yield ev


class _DictRebindMiddleware(MiddlewareBase):
    """策略 B：向 input_kwargs 字典写回新对象（闭包 dict 可见）。"""

    async def on_acting(
        self,
        agent: Any,
        input_kwargs: dict,
        next_handler: Any,
    ) -> AsyncGenerator:
        tc = input_kwargs["tool_call"]
        data = json.loads(tc.input)
        data["command"] = (
            f"export QWENPAW_SESSION_ID='spike-sid' && {data['command']}"
        )
        input_kwargs["tool_call"] = tc.model_copy(
            update={"input": json.dumps(data)},
        )
        async for ev in next_handler():
            yield ev


class _LocalRebindMiddleware(MiddlewareBase):
    """反例：只改局部变量，不写回 input_kwargs —— 预期不生效。"""

    async def on_acting(
        self,
        agent: Any,
        input_kwargs: dict,
        next_handler: Any,
    ) -> AsyncGenerator:
        tc = input_kwargs["tool_call"]
        data = json.loads(tc.input)
        data["command"] = (
            f"export QWENPAW_SESSION_ID='spike-sid' && {data['command']}"
        )
        _modified = tc.model_copy(update={"input": json.dumps(data)})
        async for ev in next_handler():
            yield ev


@pytest.mark.asyncio
async def test_h1_inplace_object_mutation_propagates() -> None:
    """H1-A：json.loads→改→json.dumps 原地写回 tc.input → 工具收到改后值。"""
    agent = _make_agent(_InPlaceObjectMiddleware())
    received = await _drive_agent(agent)
    assert received == "export QWENPAW_SESSION_ID='spike-sid' && echo hi"


@pytest.mark.asyncio
async def test_h1_dict_rebind_propagates() -> None:
    """H1-B：input_kwargs["tool_call"] = 新对象 → 工具收到改后值。"""
    agent = _make_agent(_DictRebindMiddleware())
    received = await _drive_agent(agent)
    assert received == "export QWENPAW_SESSION_ID='spike-sid' && echo hi"


@pytest.mark.asyncio
async def test_h1_local_rebind_does_not_propagate() -> None:
    """H1 反例：只改局部变量 → 工具收到原始值（链只认 input_kwargs）。"""
    agent = _make_agent(_LocalRebindMiddleware())
    received = await _drive_agent(agent)
    assert received == "echo hi"


# ---------------------------------------------------------------------------
# H2: factory(ctx, agent_config) 的 ctx.session_id 可达性
# ---------------------------------------------------------------------------


class SpikeSessionMiddleware(MiddlewareBase):
    """工厂产物：构造时持有 ctx，on_acting 用 ctx.session_id 注入命令。"""

    def __init__(self, ctx: Any) -> None:
        self._ctx = ctx

    async def on_acting(
        self,
        agent: Any,
        input_kwargs: dict,
        next_handler: Any,
    ) -> AsyncGenerator:
        tc = input_kwargs["tool_call"]
        if tc.name == "execute_shell_command":
            data = json.loads(tc.input)
            command = data.get("command", "")
            sid = self._ctx.session_id
            if sid and "QWENPAW_SESSION_ID" not in command:
                data["command"] = (
                    f"export QWENPAW_SESSION_ID='{sid}' && {command}"
                )
                input_kwargs["tool_call"] = tc.model_copy(
                    update={"input": json.dumps(data)},
                )
        async for ev in next_handler():
            yield ev


def _make_agent_config(tmp_path: Path) -> SimpleNamespace:
    """与 SC/QwenPaw/tests/unit/agents/test_tool_result_pruning_middleware.py
    中 AgentBuilder._build_middlewares 测试同款最小 stub。"""
    from qwenpaw.config.config import (
        LightContextConfig,
        ToolResultPruningConfig,
    )

    return SimpleNamespace(
        id="worker-1",
        running=SimpleNamespace(
            light_context_config=LightContextConfig(
                strategy="native",
                tool_result_pruning_config=ToolResultPruningConfig(),
            ),
        ),
    )


def _make_ctx(session_id: str, workspace_dir: Path) -> Any:
    """真实 HookContext（与 runtime._build_context 同款字段装配）。"""
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


def _register_factory(factory: Any, registry: Any) -> None:
    registry.register_middleware(
        plugin_id="spike-t0",
        factory=factory,
        priority=50,
    )


def _unregister_spike(registry: Any) -> None:
    registry._middleware_registrations = [
        r for r in registry._middleware_registrations
        if r.plugin_id != "spike-t0"
    ]


def _unwrap_plugin_middleware(mw: Any) -> Any:
    """New framework (#7565+) wraps plugin middlewares in PluginMiddlewareGuard.

    The guard forwards hooks to ``.middleware``; unwrap it so assertions and
    driving target the plugin instance itself. Old framework returns raw.
    """
    inner = getattr(mw, "middleware", None)
    return inner if inner is not None else mw


def test_h2_factory_receives_ctx_with_session_id(tmp_path) -> None:
    """H2：PluginRegistry 注册 → AgentBuilder._build_middlewares 调工厂，
    工厂收到的 ctx 即请求 HookContext 且 session_id 有值。"""
    from qwenpaw.plugins.registry import PluginRegistry
    from qwenpaw.runtime.builder import AgentBuilder

    session_id = "matrix:!room123:matrix.example"
    ctx = _make_ctx(session_id, tmp_path)
    agent_config = _make_agent_config(tmp_path)
    received_ctxs: list[Any] = []

    def factory(ctx_arg: Any, agent_config_arg: Any) -> MiddlewareBase:
        received_ctxs.append(ctx_arg)
        return SpikeSessionMiddleware(ctx_arg)

    registry = PluginRegistry()  # 单例
    _register_factory(factory, registry)
    try:
        mws = AgentBuilder._build_middlewares(ctx, agent_config)
    finally:
        _unregister_spike(registry)

    assert len(received_ctxs) == 1
    factory_ctx = received_ctxs[0]
    assert factory_ctx is ctx
    assert factory_ctx.session_id == session_id
    assert any(
        isinstance(_unwrap_plugin_middleware(mw), SpikeSessionMiddleware)
        for mw in mws
    ), f"plugin middleware missing from built list: {mws!r}"


@pytest.mark.asyncio
async def test_h2_closed_loop_session_id_reaches_shell_tool(tmp_path) -> None:
    """H2 闭环：请求 session_id → HookContext → factory ctx → 中间件实例 →
    shell 工具执行层收到注入后的 command（与 H1 同链）。"""
    from qwenpaw.plugins.registry import PluginRegistry
    from qwenpaw.runtime.builder import AgentBuilder

    session_id = "matrix:!room456:matrix.example"
    ctx = _make_ctx(session_id, tmp_path)
    agent_config = _make_agent_config(tmp_path)

    def factory(ctx_arg: Any, agent_config_arg: Any) -> MiddlewareBase:
        return SpikeSessionMiddleware(ctx_arg)

    registry = PluginRegistry()
    _register_factory(factory, registry)
    try:
        mws = AgentBuilder._build_middlewares(ctx, agent_config)
    finally:
        _unregister_spike(registry)

    mw = next(
        _unwrap_plugin_middleware(m)
        for m in mws
        if isinstance(_unwrap_plugin_middleware(m), SpikeSessionMiddleware)
    )
    agent = _make_agent(mw)
    received = await _drive_agent(agent, command="qwen --version")
    assert (
        received
        == f"export QWENPAW_SESSION_ID='{session_id}' && qwen --version"
    )


def test_h2_no_session_id_factory_skips(tmp_path) -> None:
    """session_id 为空时工厂返回 None（builder 跳过该 middleware，零副作用）。"""
    from qwenpaw.plugins.registry import PluginRegistry
    from qwenpaw.runtime.builder import AgentBuilder

    def factory(ctx_arg: Any, agent_config_arg: Any) -> MiddlewareBase | None:
        return (
            SpikeSessionMiddleware(ctx_arg) if ctx_arg.session_id else None
        )

    registry = PluginRegistry()
    _register_factory(factory, registry)
    try:
        ctx = _make_ctx("", tmp_path)
        mws = AgentBuilder._build_middlewares(ctx, _make_agent_config(tmp_path))
    finally:
        _unregister_spike(registry)

    assert not any(isinstance(mw, SpikeSessionMiddleware) for mw in mws)
