"""Read acknowledgement is emitted at the durable input boundary, not enqueue."""
import asyncio
import importlib.util
import sys
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

ROOT = Path(__file__).resolve().parents[3]
PATH = ROOT / 'plugins/agentteams-matrix-channel/agentteams_matrix/session_receipts.py'
spec = importlib.util.spec_from_file_location('session_receipts', PATH)
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)


def test_checkpoint_precedes_receipt_and_model_even_if_model_fails():
    async def run():
        order = []
        state = {'state': {'context': [{'role': 'user'}], 'middle_context': {}}}
        agent = SimpleNamespace(state=SimpleNamespace(middle_context=state['state']['middle_context']), state_dict=lambda: state)
        async def save(**kwargs):
            assert kwargs['agent'].state_dict()['state']['context']
            assert kwargs['agent'].state_dict()['state']['middle_context'][module.PENDING_KEY]
            order.append('save')
        delivery = SimpleNamespace(submit=AsyncMock(side_effect=lambda *args: order.append('receipt')))
        channel = SimpleNamespace(_session_receipts=delivery)
        ctx = SimpleNamespace(
            request=SimpleNamespace(channel='agentteams_matrix', user_id='!room:hs', channel_meta={'room_id': '!room:hs', 'session_source_event_ids': ['$1', '$3']}),
            session_id='matrix:!room:hs', mode_state={},
            workspace=SimpleNamespace(session=SimpleNamespace(save_session_state=save), channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=channel))),
        )
        async def reply(**kwargs):
            yield SimpleNamespace(type='REPLY_START')
            order.append('model')
            raise RuntimeError('model failed')
        middleware = module.session_receipt_middleware(ctx, None)
        with pytest.raises(RuntimeError, match='model failed'):
            async for _ in middleware.on_reply(agent, {}, reply):
                pass
        assert order == ['save', 'receipt', 'model']
    asyncio.run(run())


def test_failed_save_never_acknowledges_read():
    async def run():
        state = SimpleNamespace(middle_context={})
        agent = SimpleNamespace(state=state, state_dict=lambda: {'state': {'middle_context': state.middle_context}})
        delivery = SimpleNamespace(submit=AsyncMock())
        ctx = SimpleNamespace(
            request=SimpleNamespace(channel='agentteams_matrix', user_id='u', channel_meta={'room_id': '!r', 'session_source_event_ids': ['$m']}),
            session_id='s', mode_state={},
            workspace=SimpleNamespace(session=SimpleNamespace(save_session_state=AsyncMock(side_effect=OSError('disk full'))), channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=SimpleNamespace(_session_receipts=delivery)))),
        )
        async def reply(**kwargs):
            yield SimpleNamespace(type='REPLY_START')
        async for _ in module.session_receipt_middleware(ctx, None).on_reply(agent, {}, reply):
            pass
        delivery.submit.assert_not_awaited()
        assert not state.middle_context.get(module.PENDING_KEY)
    asyncio.run(run())


def test_other_channels_do_not_install_middleware():
    assert module.session_receipt_middleware(SimpleNamespace(request=SimpleNamespace(channel='console')), None) is None


def test_real_agentscope_input_is_saved_before_reasoning(tmp_path):
    from agentscope.agent import Agent
    from agentscope.message import Msg

    async def run():
        saved = []
        async def save(**kwargs):
            saved.append(kwargs['agent'].state_dict())
        delivery = SimpleNamespace(submit=AsyncMock())
        ctx = SimpleNamespace(
            request=SimpleNamespace(channel='agentteams_matrix', user_id='u', channel_meta={'room_id': '!r', 'session_source_event_ids': ['$real']}),
            session_id='s', mode_state={},
            workspace=SimpleNamespace(session=SimpleNamespace(save_session_state=save), channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=SimpleNamespace(_session_receipts=delivery)))),
        )
        agent = Agent(name='reader', system_prompt='', model=SimpleNamespace(formatter=SimpleNamespace(supported_input_media_types=[])), middlewares=[module.session_receipt_middleware(ctx, None)])
        from qwenpaw.agents.react_agent import QwenPawAgent
        from types import MethodType
        agent.state_dict = MethodType(QwenPawAgent.state_dict, agent)
        stream = agent._reply(inputs=Msg(name='sender', role='user', content=[{'type': 'text', 'text': 'actual input'}]))
        event = await anext(stream)
        assert str(event.type) == 'REPLY_START'
        assert saved[0]['state']['context'][-1]['role'] == 'user'
        delivery.submit.assert_awaited_once()
        await stream.aclose()
    asyncio.run(run())


def test_pending_read_recovered_and_retry_uses_same_transaction(tmp_path):
    import json
    import httpx
    path = tmp_path / 'agentteams_matrix' / 'session.json'
    path.parent.mkdir()
    path.write_text(json.dumps({'agent': {'state': {'middle_context': {module.PENDING_KEY: [{'room_id': '!r', 'event_id': '$m'}]}}}}))

    async def run():
        urls = []
        async def put(url, **kwargs):
            urls.append(url)
            assert kwargs['json']['status'] == 'read'
            if len(urls) == 1:
                raise OSError('offline')
            return httpx.Response(200, request=httpx.Request('PUT', url), json={'event_id': '$ack'})
        channel = SimpleNamespace(homeserver='http://matrix.test', _client=SimpleNamespace(access_token='token'), _http_client=SimpleNamespace(put=put))
        delivery = module.SessionReceipts(channel, tmp_path)
        await delivery.start()
        await delivery.stop()
        await delivery.flush()
        assert delivery.pending
        await delivery.flush()
        assert not delivery.pending
        assert urls[0] == urls[1]
        assert json.loads(path.read_text())['agent']['state']['middle_context'][module.PENDING_KEY]
    asyncio.run(run())


@pytest.mark.parametrize('relative', ['plugins/agentteams-matrix-channel/agentteams_matrix/channel.py', 'plugins/agentteams-matrix/adapters/qwenpaw/agentteams_matrix/channel.py'])
def test_history_gaps_threads_and_debounce_preserve_only_included_ids(relative):
    spec = importlib.util.spec_from_file_location('receipt_test_channel', ROOT / relative)
    channel_module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = channel_module
    spec.loader.exec_module(channel_module)
    channel = channel_module.AgentTeamsMatrixChannel(process=lambda *a, **kw: None, history_limit=2)
    for event_id in ['$dropped', '$kept1', '$kept2']:
        channel._record_history('!r', channel_module.HistoryEntry(sender='sender', body='text', message_id=event_id))
    assert channel._session_source_ids('!r', '$current', True) == ['$kept1', '$kept2', '$current']
    assert channel._session_source_ids('!r', '$thread', False) == ['$thread']
    first = {'channel_id': 'agentteams_matrix', 'sender_id': 'u', 'content_parts': [channel_module.ImageContent(type=channel_module.ContentType.IMAGE, image_url='file:///tmp/image.png')], 'meta': {'room_id': '!r', 'session_source_event_ids': ['$image']}}
    second = {'channel_id': 'agentteams_matrix', 'sender_id': 'u', 'content_parts': [channel_module.TextContent(type=channel_module.ContentType.TEXT, text='look')], 'meta': {'room_id': '!r', 'session_source_event_ids': ['$text']}}
    assert not channel._debounce_payload(first)
    assert channel._debounce_payload(second)
    assert second['meta']['session_source_event_ids'] == ['$image', '$text']
    assert channel.merge_native_items([first, second])['meta']['session_source_event_ids'] == ['$image', '$text']


@pytest.mark.parametrize('relative', ['plugins/agentteams-matrix-channel/agentteams_matrix/channel.py', 'plugins/agentteams-matrix/adapters/qwenpaw/agentteams_matrix/channel.py'])
def test_started_receipt_service_never_sends_legacy_read_at_enqueue(relative):
    from nio import RoomMessageText
    spec = importlib.util.spec_from_file_location('receipt_active_channel', ROOT / relative)
    channel_module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = channel_module
    spec.loader.exec_module(channel_module)
    channel = channel_module.AgentTeamsMatrixChannel(process=lambda *a, **kw: None)
    channel._user_id = '@bot:hs'
    channel._session_receipts = SimpleNamespace(submit=AsyncMock())
    channel._is_dm_room = AsyncMock(return_value=True)
    channel._send_typing = AsyncMock()
    channel._send_read_receipt = AsyncMock()
    queued = []
    channel._enqueue = queued.append
    event = RoomMessageText.from_dict({'type': 'm.room.message', 'event_id': '$modern', 'sender': '@human:hs', 'origin_server_ts': 100, 'content': {'msgtype': 'm.text', 'body': 'hello'}})
    asyncio.run(channel._on_room_event(SimpleNamespace(room_id='!r', users={}), event))
    assert len(queued) == 1
    channel._session_receipts.submit.assert_awaited_once_with('!r', ['$modern'], status='received')
    channel._send_read_receipt.assert_not_awaited()
