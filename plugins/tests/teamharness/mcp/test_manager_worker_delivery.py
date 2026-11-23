"""Standalone Worker messages must not enter Leader-only coordination."""
import json
from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / 'teamharness'))
from mcp import message_tool
from mcp.room.mention import _matrix_target


class Response:
    def __init__(self, data): self.data = data
    def __enter__(self): return self
    def __exit__(self, *args): return False
    def read(self): return json.dumps(self.data).encode()


@pytest.fixture
def delivery(monkeypatch, tmp_path):
    token = tmp_path / 'token'
    token.write_text('test-controller-token')
    for key, value in {
        'AGENTTEAMS_CONTROLLER_URL': 'http://controller',
        'AGENTTEAMS_AUTH_TOKEN_FILE': str(token),
        'TEAMHARNESS_COORDINATION_URL': 'http://controller/api/v1/coordination',
        'AGENTTEAMS_MATRIX_URL': 'http://matrix',
        'AGENTTEAMS_WORKER_MATRIX_TOKEN': 'test-matrix-token',
    }.items():
        monkeypatch.setenv(key, value)
    state = {'workers': [], 'direct': [], 'coordination': [], 'matrix': [], 'event': '$sent'}
    original_resolve = message_tool.refs.resolve
    monkeypatch.setattr(message_tool.refs, 'resolve', lambda name: (lambda: 'manager') if name == '_runtime_role' else original_resolve(name))
    monkeypatch.setattr(message_tool, '_matrix_user_id', lambda: '@manager:test')
    def direct(server, token, recipient):
        state['direct'].append(recipient)
        return '!private:test'
    monkeypatch.setattr(message_tool, '_direct_message_room', direct)
    def coordinate(kind, **event):
        state['coordination'].append(event)
        return {'sent': True, 'eventId': '$handoff'}
    monkeypatch.setattr(message_tool, 'deliver_event', coordinate)
    def request(req, timeout):
        if req.full_url == 'http://controller/api/v1/workers':
            assert req.get_header('Authorization') == 'Bearer test-controller-token'
            return Response({'workers': state['workers']})
        assert '/rooms/%21private%3Atest/send/m.room.message/' in req.full_url
        state['matrix'].append(json.loads(req.data))
        return Response({'event_id': state['event']} if state['event'] else {})
    monkeypatch.setattr(message_tool.urllib.request, 'urlopen', request)
    deps = message_tool.MessageToolDeps(
        reply_route=lambda args: {}, qwenpaw_message=lambda *args: pytest.fail('unexpected non-Matrix route'),
        matrix_target=_matrix_target, mentions=lambda text, room: ['@alice:test'],
        ping_pong_error=lambda *args: None,
        matrix_content=lambda text, mentions: {'msgtype': 'm.text', 'body': text, 'm.mentions': {'user_ids': mentions}},
        record_matrix_outbound_to_session=lambda *args: True,
    )
    state['send'] = lambda target='user:alice': message_tool.message({'target': target, 'message': '@alice:test create the requested README'}, deps)
    return state


def test_standalone_worker_name_resolves_to_executable_private_message(delivery):
    delivery['workers'] = [{'name': 'alice', 'matrixUserID': '@alice:test'}]
    result = delivery['send']()
    assert result['ok'] and result['messageId'] == '$sent'
    assert delivery['direct'] == ['@alice:test']
    assert not delivery['coordination']
    assert 'org.agentteams.progress_only' not in delivery['matrix'][0]


def test_registered_leader_alias_keeps_native_handoff(delivery):
    delivery['workers'] = [{'name': 'alice', 'matrixUserID': '@alice:test', 'team': 'alpha', 'role': 'team_leader'}]
    result = delivery['send']()
    assert result['ok'] and result['transport'] == 'agent-api'
    assert delivery['coordination'][0]['recipient'] == '@alice:test'
    assert not delivery['matrix']


def test_manager_cannot_bypass_a_team_leader(delivery):
    delivery['workers'] = [{'name': 'alice', 'matrixUserID': '@alice:test', 'team': 'alpha', 'role': 'worker'}]
    result = delivery['send']()
    assert not result['ok']
    assert 'Leader' in result['error']
    assert not delivery['direct'] and not delivery['coordination'] and not delivery['matrix']


def test_ambiguous_registered_name_fails_without_sending(delivery):
    delivery['workers'] = [{'name': 'alice', 'matrixUserID': '@alice:test'}, {'name': 'other', 'workerName': 'alice', 'matrixUserID': '@other:test'}]
    result = delivery['send']()
    assert not result['ok']
    assert not delivery['direct'] and not delivery['coordination'] and not delivery['matrix']


def test_matrix_reply_without_event_id_is_not_success(delivery):
    delivery['workers'] = [{'name': 'alice', 'matrixUserID': '@alice:test'}]
    delivery['event'] = ''
    assert not delivery['send']()['ok']
