"""Exercise Task-owned native tools and trusted MCP delegation with real SDK types."""
import asyncio
import importlib.util
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

pytest.importorskip('agentscope.agent')
from agentscope.agent import Agent
from agentscope.message import ToolCallBlock, TextBlock
from agentscope.tool import ToolResponse, MCPTool
from agentscope.mcp import MCPClient
from agentscope.mcp._config import StdioMCPConfig
import mcp.types

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / 'shared/lib'))
from qwenpaw_message_execution import canonical_registration
from task_recovery import Recovery, RecoveryConflict


def middleware(ctx):
    path = ROOT / 'plugins/agentteams-matrix/adapters/qwenpaw/agentteams_matrix/session_receipts.py'
    spec = importlib.util.spec_from_file_location('_qwen_native_journal', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.SessionReceiptMiddleware(ctx)


@pytest.mark.parametrize('outcome', ['success', 'timeout', 'late_timeout', 'error'])
def test_task_owned_tool_checkpoints_before_effect_and_after_complete_stream(outcome):
    order = []
    identity = dict(task_id='T', team_id='team', generation=2, process_id='p', instance_id='i')
    call = ToolCallBlock(id='write', name='write_file', input='{"path":"output.txt"}')
    def begin(actual, calls):
        assert actual == identity and calls[0]['arguments'] == {'path': 'output.txt'}
        order.append('started')
        return 'op'
    def finish(actual, operation, results):
        assert actual == identity and operation == 'op' and results[0]['id'] == 'write'
        order.append('verified')
        return {'execution': {'checkpoint': {'version': 2}}}
    recovery = SimpleNamespace(begin_tools=begin, finish_tools=finish)
    def verify(actual):
        assert actual == identity
        order.append('owned')
    channel = SimpleNamespace(_task_execution=SimpleNamespace(recovery=recovery, verify=verify))
    ctx = SimpleNamespace(extras={}, request=SimpleNamespace(channel_meta={'task_execution': identity}),
        workspace=SimpleNamespace(channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=channel))))
    async def invoke(actual):
        assert actual is call
        order.append('effect')
        if outcome == 'timeout':
            raise TimeoutError('unconfirmed effect')
        yield ToolResponse(state='error' if outcome == 'error' else 'success', content=[TextBlock(text='saved')])
        if outcome == 'late_timeout':
            raise TimeoutError('stream finalization failed')
    agent = SimpleNamespace(_acting_middlewares=[middleware(ctx)], _acting_impl=invoke)
    async def scenario():
        if outcome == 'success':
            results = [item async for item in Agent._acting(agent, call)]
            assert len(results) == 1
            assert ctx.extras['agentteams_task_tool_turn']['checkpoint_digest'] == Recovery.digest({'version': 2})
            assert order == ['owned', 'started', 'effect', 'verified']
        else:
            with pytest.raises(TimeoutError if 'timeout' in outcome else RecoveryConflict):
                async for _ in Agent._acting(agent, call):
                    pass
            assert order == ['owned', 'started', 'effect']
    asyncio.run(scenario())


@pytest.mark.parametrize('scope', ['task', 'message', 'unmanaged'])
def test_composite_tool_does_not_enter_a_managed_journal_without_child_admission(scope):
    managed = scope != 'unmanaged'
    identity = {'event_key': 'M'} if scope == 'message' else {'task_id': 'T', 'team_id': 'team'}
    def start_batch(*_):
        pytest.fail('Composite tool was journaled as a single operation')
    recovery = SimpleNamespace(tools_current=lambda _: {'execution': {}}, begin_tools=start_batch)
    matrix = SimpleNamespace(message_current=lambda _: {'execution': {}}, message_begin_tools=start_batch)
    messages = SimpleNamespace(matrix=matrix, turns={'M': {'identity': identity}})
    channel = SimpleNamespace(_task_execution=SimpleNamespace(recovery=recovery, messages=messages, verify=lambda _: None))
    meta = {scope + '_execution': identity} if managed else {}
    ctx = SimpleNamespace(extras={}, request=SimpleNamespace(channel_meta=meta),
        workspace=SimpleNamespace(channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=channel))))
    hook = middleware(ctx)
    tools = [{'function': {'name': name}} for name in ['run_tool_batch', 'write_file']]
    batch = ToolCallBlock(id='batch', name='run_tool_batch', input='{}')
    native_invoked = False

    async def native(**kwargs):
        nonlocal native_invoked
        native_invoked = True
        yield ToolResponse(state='success', content=[TextBlock(text='native')])

    async def scenario():
        model = AsyncMock()
        await hook.on_model_call(None, {'tools': tools}, model)
        visible = model.await_args.kwargs['tools']
        assert [schema['function']['name'] for schema in visible] == (
            ['write_file'] if managed else ['run_tool_batch', 'write_file'])
        assert len(tools) == 2
        results = [item async for item in hook.on_acting(None, {'tool_call': batch}, native)]
        assert len(results) == 1
        assert native_invoked == (not managed)
        assert results[0].state == ('error' if managed else 'success')
    asyncio.run(scenario())


@pytest.mark.parametrize('scope', ['task', 'message'])
def test_unconfirmed_operation_blocks_actual_model_boundary(scope):
    identity = ({'event_key': 'key'} if scope == 'message' else {'task_id': 'T', 'team_id': 'team'})
    snapshot = {'execution': {'checkpoint': {'operation': {'state': 'unknown'}}}}
    matrix = SimpleNamespace(message_current=lambda _: snapshot)
    channel = SimpleNamespace(_task_execution=SimpleNamespace(
        messages=SimpleNamespace(matrix=matrix, turns={'key': {'identity': identity}}),
        recovery=SimpleNamespace(tools_current=lambda _: snapshot)))
    ctx = SimpleNamespace(extras={}, request=SimpleNamespace(channel_meta={scope + '_execution': identity}),
        workspace=SimpleNamespace(channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=channel))))
    calls = []
    async def model(**kwargs):
        calls.append('model')
        return 'generation'
    async def scenario():
        with pytest.raises(RecoveryConflict, match='unconfirmed'):
            await middleware(ctx).on_model_call(None, {}, model)
        assert calls == []
    asyncio.run(scenario())


@pytest.mark.parametrize('variation', ['valid', 'wrong_file', 'wrong_session', 'wrong_command', 'extra_argument', 'name_only'])
def test_only_actual_builtin_mcp_connection_delegates(variation):
    server = ROOT / 'plugins/teamharness/mcp/server.py'
    session = SimpleNamespace()
    tool = MCPTool('teamharness', mcp.types.Tool(name='task', inputSchema={'type': 'object'}), session=session)
    config = StdioMCPConfig(command='/tmp/evil' if variation == 'wrong_command' else 'python',
        args=[str(server if variation != 'wrong_file' else ROOT / 'fake.py')])
    if variation == 'extra_argument':
        config.args.insert(0, '-c')
    client = MCPClient(name='teamharness', is_stateful=True, mcp_config=config)
    client._session = SimpleNamespace() if variation == 'wrong_session' else session
    actual = SimpleNamespace(name=tool.name) if variation == 'name_only' else tool
    agent = SimpleNamespace(toolkit=SimpleNamespace(get_tool=AsyncMock(return_value=actual),
        tool_groups=[SimpleNamespace(mcps=[client])]))
    result = asyncio.run(canonical_registration(agent, SimpleNamespace(name=tool.name), server))
    assert (result is not None) == (variation == 'valid')


def test_canonical_unknown_stops_before_generic_start_and_model():
    calls = []
    async def admission(agent):
        calls.append('canonical')
        raise RecoveryConflict('canonical unknown')
    ctx = SimpleNamespace(extras={'agentteams_canonical_admit': admission},
        request=SimpleNamespace(channel_meta={}))
    async def native(**kwargs):
        calls.append('native')
        yield 'response'
    async def scenario():
        hook = middleware(ctx)
        with pytest.raises(RecoveryConflict, match='canonical unknown'):
            async for _ in hook.on_acting(None, {}, native):
                pass
        with pytest.raises(RecoveryConflict, match='canonical unknown'):
            await hook.on_model_call(None, {}, native)
        assert calls == ['canonical', 'canonical']
    asyncio.run(scenario())


def test_canonical_cached_receipt_enters_native_context_once(monkeypatch):
    path = ROOT / 'plugins/teamharness/adapters/qwenpaw/plugin.py'
    spec = importlib.util.spec_from_file_location('_qwen_canonical_context', path)
    plugin = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(plugin)
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://own-fixture')
    monkeypatch.setenv('AGENTTEAMS_AGENT_ROLE', 'worker')
    contexts = []
    def admit(context):
        contexts.append(context)
        return {'ok': True, 'receipts': [{'receiptId': 'receipt-one', 'fact': 'Task T submitted; downstream notification confirmed.'}]}
    monkeypatch.setattr(plugin, '_canonical_module', lambda: SimpleNamespace(admit=admit))
    ctx = SimpleNamespace(extras={}, request=SimpleNamespace(channel_meta={'task_execution': {'team_id': 'team'}}))
    plugin._sanitizer_factory(ctx, None)
    plugin._execution_task.set('T')
    agent = SimpleNamespace(name='Worker', state=SimpleNamespace(middle_context={}, context=[]))
    async def scenario():
        await ctx.extras['agentteams_canonical_admit'](agent)
        await ctx.extras['agentteams_canonical_admit'](agent)
    asyncio.run(scenario())
    assert all(c['execution'] == 'T' and c['team'] == 'team' for c in contexts)
    assert len(agent.state.context) == 1
    assert agent.state.context[0].role == 'assistant'
    assert 'downstream notification confirmed' in agent.state.context[0].content[0].text
    assert agent.state.middle_context['teamharness.canonical_receipts'] == {'receipt-one': True}


@pytest.mark.parametrize('scope', ['unmanaged', 'task', 'message', 'canonical'])
@pytest.mark.parametrize('blocked', [False, True])
def test_real_agent_awaits_model_admission_before_provider(scope, blocked):
    from agentscope.model import ChatModelBase, ChatResponse
    from agentscope.message import Msg
    from agentscope.formatter import OpenAIChatFormatter
    from pydantic import BaseModel
    class Parameters(BaseModel):
        pass
    class Model(ChatModelBase):
        def __init__(self):
            super().__init__(None, 'native-model-admission-fixture', Parameters(), stream=False)
            self.formatter = OpenAIChatFormatter()
            self.calls = 0
        async def __call__(self, *args, **kwargs):
            self.calls += 1
            return ChatResponse(content=[TextBlock(text='Done.')], is_last=True)
    identity = {'event_key': 'key'} if scope == 'message' else {'task_id': 'T', 'team_id': 'team'}
    checkpoint = {'operation': {'state': 'unknown'}} if blocked else {}
    snapshot = {'execution': {'checkpoint': checkpoint}}
    channel = SimpleNamespace(_task_execution=SimpleNamespace(
        messages=SimpleNamespace(matrix=SimpleNamespace(message_current=lambda _: snapshot),
                                 turns={'key': {'identity': identity}}),
        assignments=SimpleNamespace(before_reasoning=AsyncMock()),
        recovery=SimpleNamespace(tools_current=lambda _: snapshot)))
    meta = {scope + '_execution': identity} if scope in {'task', 'message'} else {}
    extras = {}
    if scope == 'canonical':
        extras['agentteams_canonical_admit'] = AsyncMock(
            side_effect=RecoveryConflict('canonical unknown') if blocked else None)
    ctx = SimpleNamespace(extras=extras, request=SimpleNamespace(channel_meta=meta),
        workspace=SimpleNamespace(channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=channel))))
    model = Model()
    agent = Agent('Worker', 'Answer briefly.', model, middlewares=[middleware(ctx)])
    async def scenario():
        inputs = [Msg(name='Human', role='user', content=[TextBlock(text='Reply done.')])]
        if blocked and scope != 'unmanaged':
            with pytest.raises(RecoveryConflict):
                await agent.reply(inputs)
            assert model.calls == 0
        else:
            await agent.reply(inputs)
            assert model.calls == 1
    asyncio.run(scenario())
