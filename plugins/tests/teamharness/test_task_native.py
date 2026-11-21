"""Native bindings share one public protocol; no SDK or live state required."""
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys

import pytest

COMMON = Path(__file__).resolve().parents[3] / 'plugins/teamharness/mcp/common'


@pytest.fixture
def bridge(monkeypatch):
    monkeypatch.syspath_prepend(str(COMMON))
    spec = importlib.util.spec_from_file_location('native_bridge_fixture', COMMON / 'task_native.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_source_references_reuse_the_public_canonical_identity(bridge):
    meta = {'room_id': '!source:test', 'event_id': '$request', 'sender_id': '@human:test'}
    assert bridge.invoke({'operation': 'sources', 'meta': meta}) == bridge.receipts.creation_sources(meta)
    with pytest.raises(ValueError):
        bridge.invoke({'operation': 'sources', 'meta': {'event_id': '$unbound'}})


def test_only_pre_call_validation_is_proven_to_have_no_effect(bridge):
    payload = {'operation': 'task', 'arguments': {'action': 'submit', 'taskId': 'foreign', 'summary': 'done'},
               'context': {'role': 'worker', 'execution': 'own'}}
    calls = []
    with pytest.raises(bridge.NativeTaskValidationError) as error:
        bridge.invoke(payload, call=lambda args: calls.append(args))
    assert error.value.effect == 'none' and calls == []
    payload['arguments']['taskId'] = 'own'
    def unknown(_):
        raise ValueError('invalid response after a possible write')
    with pytest.raises(ValueError) as error:
        bridge.invoke(payload, call=unknown)
    assert getattr(error.value, 'effect', 'unknown') == 'unknown'


def test_phase_admission_reuses_the_public_lifecycle_without_model(bridge):
    calls = []
    async def notify(*args):
        calls.append(args)
        return {'active': True, 'pending_task_ids': ['main']}
    payload = {'operation': 'admit_event', 'eventId': 'a' * 32, 'nativeEventId': 'matrix:$native'}
    assert bridge.invoke(payload, notify=notify)['pending_task_ids'] == ['main']
    assert calls == [('a' * 32, 'matrix:$native', 'begin')]
    for change in [{'eventId': '../foreign'}, {'nativeEventId': ''}, {'action': 'complete'}]:
        with pytest.raises(ValueError):
            bridge.invoke({**payload, **change}, notify=notify)
    assert len(calls) == 1


def test_handoff_uses_only_admitted_ids_without_another_task_call(bridge):
    def unexpected(_):
        pytest.fail('handoff must not call the Task CLI')
    response = bridge.invoke({'operation': 'prepare_event', 'event':
                             {'kind': 'handoff', 'pending_task_ids': ['main-a', 'main-b']}}, call=unexpected)
    assert response['planning'] == ['main-a', 'main-b']
    for pending in [None, [], ['../other'], ['main-a', 'main-a']]:
        with pytest.raises(ValueError):
            bridge.invoke({'operation': 'prepare_event', 'event':
                           {'kind': 'handoff', 'pending_task_ids': pending}}, call=unexpected)


def test_assignment_prepares_once_and_serializes_authoritative_io(bridge):
    calls = []
    def call(arguments):
        calls.append(arguments)
        return {'task': {'id': 't1', 'mainTaskId': 'main', 'projectId': '',
                         'status': 'in_progress', 'assignmentId': 'dispatch', 'assignedTo': 'dev'},
                'taskDirectory': '/fixture/tasks/t1', 'outputDirectory': '/fixture/tasks/t1/workspace',
                'scratchDirectory': '/fixture/tasks/t1/workspace/.scratch'}
    result = bridge.invoke({'operation': 'prepare_event', 'event':
                            {'kind': 'task_assigned', 'task_id': 't1', 'main_task_id': 'main',
                             'project_id': '', 'delivery_id': 'dispatch'}}, call=call)
    assert calls == [{'action': 'begin', 'taskId': 't1'}]
    assert result['execution'] == 't1'
    assert json.loads(json.dumps(result))['ack'][1] == '/fixture/tasks/t1'


def test_preparation_transport_failure_retains_retryability_without_execution_scope(bridge):
    failed = {'ok': False, 'retryable': True, 'error': 'task_operation_failed', 'message': 'HTTP 503'}
    result = bridge.invoke({'operation': 'prepare_event', 'event':
                            {'kind': 'task_assigned', 'task_id': 't1'}}, call=lambda _: failed)
    assert result == {'ok': False, 'retryable': True, 'result': failed}
    assert 'execution' not in result
    assert 'fact' not in result


@pytest.mark.parametrize('arguments,context', [
    ({'action': 'submit', 'taskId': 'other', 'summary': 'done'}, {'role': 'worker', 'execution': 't1'}),
    ({'action': 'accept', 'taskId': 't1', 'submissionId': 'stale', 'note': 'ok'},
     {'role': 'leader', 'review': ['t1', 'current']}),
    ({'action': 'plan', 'taskId': 'other', 'nodes': []}, {'role': 'leader', 'planning': ['main']}),
    ({'action': 'complete', 'taskId': 'other', 'report': 'done'}, {'role': 'leader', 'completion': 'main'}),
    ({'action': 'cancel', 'taskId': 'main', 'reason': 'stop'}, {'role': 'worker'}),
    ({'action': 'create', 'leader': 'lead', 'title': 'work', '_nativeSource': {}}, {'role': 'manager'}),
])
def test_wrong_scope_or_model_private_fields_never_reach_cli(bridge, arguments, context):
    with pytest.raises(ValueError):
        bridge.invoke({'operation': 'task', 'arguments': arguments, 'context': context},
                      call=lambda _: pytest.fail('unbound operation executed'))


def test_create_keeps_source_binding_per_call_and_does_not_end_human_turn(bridge):
    original = {'inputId': 'ref', 'eventId': '$source', 'roomId': '!room:test', 'userId': '@human:test'}
    calls = []
    def call(arguments):
        calls.append(arguments)
        return {'sourceEventId': '$source', 'task': {'id': 'main', 'kind': 'main', 'status': 'planned'},
                'notification': {'status': 'queued', 'recipient': 'lead', 'eventId': 'notice'}}
    args = {'action': 'create', 'leader': 'lead', 'title': 'work'}
    result = bridge.invoke({'operation': 'task', 'arguments': args,
                            'context': {'role': 'manager', 'sources': [original]}}, call=call)
    assert calls[0]['_nativeSource'] == {'eventId': '$source', 'roomId': '!room:test', 'userId': '@human:test'}
    assert '_nativeSource' not in args
    assert result['fact'].startswith('[Task system receipt]')
    assert result['phaseComplete'] is False


def test_late_inputs_remain_user_inputs_and_require_native_checkpoint(bridge):
    source = {'inputId': 'ref', 'eventId': '$source', 'roomId': '!room:test', 'userId': '@human:test'}
    update = {'ok': True, 'outcome': 'input_changed', 'noTaskRegistered': True,
              'sourceEventId': '$source', 'inputs': [{'eventId': '$new', 'roomId': '!room:test',
                                                    'userId': '@human:test', 'body': 'Unique amendment fixture 83f2'}]}
    result = bridge.invoke({'operation': 'task', 'arguments': {'action': 'create', 'leader': 'lead', 'title': 'work'},
                            'context': {'role': 'manager', 'sources': [source]}}, call=lambda _: update)
    assert result['refreshedInputs'][0]['body'] == 'Unique amendment fixture 83f2'
    assert 'Unique amendment fixture 83f2' not in result['fact']
    assert result['requiresInputCheckpoint'] is True
    assert result['phaseComplete'] is False
    with pytest.raises(ValueError, match='checkpoint'):
        bridge.invoke({'operation': 'task', 'arguments': {'action': 'create', 'leader': 'lead', 'title': 'work'},
                       'context': {'role': 'manager', 'sources': [source], 'inputRefreshPending': True}},
                      call=lambda _: pytest.fail('create before native checkpoint'))


def test_unverified_result_does_not_generate_a_completion_fact(bridge):
    result = bridge.invoke({'operation': 'task', 'arguments': {'action': 'submit', 'taskId': 't1', 'summary': 'done'},
                            'context': {'role': 'worker', 'execution': 't1'}},
                           call=lambda _: {'ok': False, 'retryable': True, 'message': 'unavailable'})
    assert result['phaseComplete'] is False
    assert 'fact' not in result


def test_verified_submission_adds_the_shared_receipt_fact(bridge):
    result = bridge.invoke({'operation': 'task', 'arguments': {'action': 'submit', 'taskId': 't1', 'summary': 'done'},
                            'context': {'role': 'worker', 'execution': 't1'}},
                           call=lambda _: {'task': {'id': 't1', 'status': 'submitted', 'submissionId': 's1'},
                                           'notification': {'status': 'queued', 'recipient': 'lead', 'eventId': 'notice'}})
    assert result['phaseComplete'] is True
    assert 'Delivery and acceptance are not yet confirmed' in result['fact']


def test_planning_ends_only_after_every_admitted_request_is_planned(bridge):
    context = {'role': 'leader', 'planning': ['main-a', 'main-b']}
    def call(arguments):
        return {'task': {'id': arguments['taskId'], 'kind': 'main', 'planId': 'plan-one'},
                'progress': 'ready', 'assignments': []}
    first = bridge.invoke({'operation': 'task', 'arguments': {'action': 'plan', 'taskId': 'main-a', 'nodes': []},
                            'context': context}, call=call)
    assert first['phaseComplete'] is False
    context.update(first['contextUpdate'])
    second = bridge.invoke({'operation': 'task', 'arguments': {'action': 'plan', 'taskId': 'main-b', 'nodes': []},
                             'context': context}, call=call)
    assert second['phaseComplete'] is True


def test_delivery_acknowledgement_is_returned_without_mutating_native_state(bridge):
    middle = {}
    queries = []
    def get(route):
        queries.append(route)
        return {'deliveries': [{'eventId': 'delivery', 'status': 'completed', 'roomId': '!room:test',
                                'taskId': 'main', 'reportMessageId': '$report', 'report': 'Accepted report',
                                'artifacts': []}]}
    result = bridge.invoke({'operation': 'delivery_context', 'middle': middle, 'room': '!room:test'}, get_json=get)
    assert queries == ['task-deliveries?roomId=%21room%3Atest']
    assert result['acknowledgements'] == {'delivery': '!room:test'}
    assert 'Accepted report' in result['facts'][0]
    assert middle == {}
    persisted = bridge.invoke({'operation': 'ack_delivery_context', 'middle': middle,
                              'acknowledgements': result['acknowledgements']})
    assert middle == {}
    assert persisted['teamharness.sent_deliveries']['!room:test']['delivery'] is True
    repeated = bridge.invoke({'operation': 'delivery_context', 'middle': persisted, 'room': '!room:test'}, get_json=get)
    assert repeated['facts'] == [] and repeated['acknowledgements'] == {}


@pytest.mark.parametrize('direct,configured,expected_path', [
    ('direct-token', None, None), ('', '/fixture/worker-token', '/fixture/worker-token'),
    ('', None, '/var/run/secrets/agentteams/token'),
])
def test_controller_auth_uses_worker_credentials(bridge, monkeypatch, direct, configured, expected_path):
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://controller.fixture')
    monkeypatch.setenv('AGENTTEAMS_AUTH_TOKEN', direct)
    monkeypatch.delenv('AGENTTEAMS_AUTH_TOKEN_FILE', raising=False)
    if configured:
        monkeypatch.setenv('AGENTTEAMS_AUTH_TOKEN_FILE', configured)
    reads, requests = [], []
    def read(path):
        reads.append(str(path))
        return 'file-token\n'
    def open_request(request, **kwargs):
        requests.append(request)
        return io.BytesIO(b'{"deliveries":[]}')
    monkeypatch.setattr(bridge.Path, 'read_text', read)
    monkeypatch.setattr(bridge.urllib.request, 'urlopen', open_request)
    assert bridge.controller_json('task-deliveries?roomId=fixture') == {'deliveries': []}
    assert reads == ([] if expected_path is None else [expected_path])
    assert requests[0].get_header('Authorization') == 'Bearer ' + (direct or 'file-token')
    assert requests[0].full_url == 'http://controller.fixture/api/v1/task-deliveries?roomId=fixture'


def test_cli_is_json_only_and_unknown_operations_fail_closed():
    result = subprocess.run([sys.executable, str(COMMON / 'task_native.py')],
                            input=json.dumps({'operation': 'schema', 'role': 'manager'}),
                            text=True, capture_output=True)
    assert result.returncode == 0, result.stderr
    assert 'create' in json.loads(result.stdout)['inputSchema']['properties']['action']['enum']
    result = subprocess.run([sys.executable, str(COMMON / 'task_native.py')],
                            input=json.dumps({'operation': 'arbitrary_shell'}), text=True, capture_output=True)
    assert result.returncode != 0
    assert json.loads(result.stdout)['ok'] is False
    assert 'Traceback' not in result.stderr
