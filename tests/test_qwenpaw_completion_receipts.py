"""Use the native channel with own inputs; never read real sessions or send Matrix traffic."""
import asyncio
import importlib.util
from pathlib import Path
import sys
from unittest.mock import AsyncMock

import pytest

if importlib.util.find_spec('qwenpaw') is None:
    pytest.skip('native QwenPaw SDK required', allow_module_level=True)

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT / 'shared/lib/telemetry'),
               str(ROOT / 'plugins/agentteams-matrix/adapters/qwenpaw')]
from agentteams_matrix import channel as matrix
from at_trace_transport import remember_reply
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from qwenpaw.schemas import ContentType, TextContent


@pytest.mark.parametrize('fails', [False, True])
def test_only_delivered_final_reply_completes_all_admitted_sources(monkeypatch, fails):
    monkeypatch.setenv('AGENTTEAMS_OTEL_ENABLED', 'true')
    monkeypatch.setenv('AGENTTEAMS_AGENT_ROLE', 'manager')
    native = matrix.AgentTeamsMatrixChannel.__new__(matrix.AgentTeamsMatrixChannel)
    native.share_session_in_group = True
    payload = {'sender_id': '@human:fixture', 'content_parts': [TextContent(type=ContentType.TEXT, text='Own input')],
               'meta': {'room_id': '!room:fixture', 'event_id': '$one', 'session_source_event_ids': ['$one']}}
    request = native.build_agent_request_from_native(payload)
    send_meta = dict(payload['meta'])
    # Runtime copies metadata; BaseChannel has already copied send metadata.
    provider = TracerProvider()
    turn = provider.get_tracer('fixture').start_span('own model turn')
    remember_reply(turn, {'agentteams.turn.id': 'own-turn'}, dict(request.channel_meta))
    turn.end()
    # Verified late Human input is appended after BaseChannel's initial snapshot.
    request.channel_meta['session_source_event_ids'] = ['$one', '$two', '$three']
    receipts = type('Receipts', (), {'complete': AsyncMock()})()
    native._session_receipts = receipts
    native._evaluate_send_gate = lambda *args: ('send', 0)
    native._send_typing = AsyncMock()
    seen = []
    async def send(*args, **kwargs):
        seen.append(trace.get_current_span().get_span_context().trace_id)
        if fails:
            raise OSError('Own synthetic Matrix failure')
        matrix._MANAGER_FINAL_DELIVERY.get()[1].append(('m.text', True))
    native.send = send
    send_meta[matrix._MATRIX_STREAMING_FINAL_TEXT_KEY] = 'Own final response'
    with provider.get_tracer('fixture').start_as_current_span('unrelated config request') as ambient:
        if fails:
            with pytest.raises(OSError):
                asyncio.run(native._on_process_completed(request, '!room:fixture', send_meta))
        else:
            asyncio.run(native._on_process_completed(request, '!room:fixture', send_meta))
        assert trace.get_current_span().get_span_context().trace_id == ambient.get_span_context().trace_id
    assert seen == [turn.get_span_context().trace_id]
    calls = receipts.complete.await_args_list
    assert [call.args for call in calls] == ([] if fails else [('!room:fixture', source) for source in ['$one', '$two', '$three']])
    if not fails:
        assert all(call.kwargs['trace_context'].get('otel_reply') for call in calls)
