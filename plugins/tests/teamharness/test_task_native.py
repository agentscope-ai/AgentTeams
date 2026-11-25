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


def test_model_result_retains_terminal_facts_without_repeating_canonical_snapshot(bridge):
    outcome = {'phaseComplete': True, 'receiptId': 'saved', 'fact': 'Submission queued',
               'extraReceipts': [{'receiptId': 'sent', 'fact': 'Actual send confirmed'}],
               'result': {'task': {'id': 'own', 'status': 'completed', 'spec': 'Large repeated request'},
                          'acceptedResults': [{'report': 'Large repeated report'}]}}
    projected = bridge.invoke({'operation': 'model_result', 'outcome': outcome})
    assert projected == {'phaseComplete': True, 'receiptId': 'saved', 'fact': 'Submission queued',
                         'extraReceipts': outcome['extraReceipts'],
                         'task': {'id': 'own', 'status': 'completed'}}
    assert outcome['result']['task']['spec'] == 'Large repeated request'
    for result in ({'ok': False, 'effect': 'none', 'message': 'Fix the input'},
                   {'task': {'id': 'own'}, 'files': [{'content': 'Requested file content'}]}):
        assert bridge.invoke({'operation': 'model_result', 'outcome': {'result': result}}) == result


def test_model_begin_result_keeps_prepared_paths_without_control_or_file_snapshots(bridge):
    result = {'task': {'id': 'own', 'spec': 'Current assignment'}, 'generation': 6,
              'checkpoint': {'operation': {'id': 'tools:internal', 'state': 'verified'},
                             'result': 'Repeated old result'},
              'outputDirectory': '/worker/task/output',
              'inputs': [{'directory': '/worker/task/inputs', 'files': [
                  {'artifactId': 'accepted', 'localPath': '/worker/task/inputs/report.md',
                   'preview': {'text': 'Repeated upstream report'}}]}]}
    projected = bridge.invoke({'operation': 'model_result', 'outcome': {'result': result}})
    assert 'checkpoint' not in projected and 'generation' not in projected
    assert projected['task'] == result['task']
    assert projected['outputDirectory'] == result['outputDirectory']
    assert projected['inputs'][0]['files'] == [
        {'artifactId': 'accepted', 'localPath': '/worker/task/inputs/report.md'}]
    assert result['checkpoint']['result'] == 'Repeated old result'
    assert result['inputs'][0]['files'][0]['preview']['text'] == 'Repeated upstream report'


def test_json_roster_uses_trusted_native_team_scope_once(bridge):
    calls = []
    def get(route):
        calls.append(route)
        return {'name': 'alpha', 'members': [{'name': 'dev', 'role': 'worker', 'matrixUserID': '@dev:test'}]}
    result = bridge.invoke({'operation': 'roster', 'role': 'leader', 'team': 'alpha'}, get_json=get)
    assert calls == ['teams/alpha']
    assert result['members'][0]['name'] == 'dev'


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


def test_completion_requires_explicit_selection_for_competing_output_names(bridge, monkeypatch):
    import task_canonical
    monkeypatch.setattr(task_canonical, 'admit', lambda _: {'ok': True})
    context = {'role': 'leader', 'completion': 'main', 'context': {'acceptedResults': [
        {'candidate': {'artifacts': [{'name': 'app.py', 'artifactId': 'old', 'version': 'v1'}]}},
        {'candidate': {'artifacts': [{'name': 'app.py', 'artifactId': 'final', 'version': 'v2'}]}}]}}
    arguments = {'action': 'complete', 'taskId': 'main', 'report': 'Final project'}
    calls = []
    def call(args):
        calls.append(args)
        return {'ok': False, 'effect': 'none'}
    result = bridge.invoke({'operation': 'task', 'arguments': arguments, 'context': context}, call=call)
    assert calls == []
    assert result['result']['error'] == 'final_artifact_selection_required'
    assert result['result']['effect'] == 'none' and result['phaseComplete'] is False
    bridge.invoke({'operation': 'task', 'arguments': {**arguments, 'artifacts': ['final']}, 'context': context}, call=call)
    assert calls == [{**arguments, 'artifacts': ['final']}]


@pytest.mark.parametrize("role", ["manager", "leader"])
def test_creation_coverage_tracks_exact_sources_without_forcing_a_task_for_questions(bridge, role):
    sources = [
        {"inputId": "input-a", "eventId": "$first", "roomId": "!room:test", "userId": "@human:test"},
        {"inputId": "input-b", "eventId": "$second", "roomId": "!room:test", "userId": "@human:test"},
    ]
    context = {"role": role, "sources": sources}

    def call(arguments):
        return {"sourceEventId": arguments["_nativeSource"]["eventId"],
                "task": {"id": "main", "kind": "main", "status": "planned"},
                "notification": {"status": "queued", "recipient": "lead", "eventId": "notice"}}

    args = {"action": "create", "leader": "lead", "title": "Same business request", "inputId": "input-b"}
    second = bridge.invoke({"operation": "task", "arguments": args, "context": context}, call=call)
    assert 'supplied input IDs: ["input-a"]' in second["fact"]
    assert "Answer questions or handle withdrawals" in second["fact"]
    assert second["contextUpdate"] == {"registeredInputs": ["$second"]}
    assert second["phaseComplete"] is False

    first = bridge.invoke({"operation": "task", "arguments": {**args, "inputId": "input-a"},
                           "context": {**context, **second["contextUpdate"]}}, call=call)
    assert "[Task input coverage]" not in first["fact"]
    assert first["contextUpdate"] == {"registeredInputs": ["$first", "$second"]}
    assert first["phaseComplete"] is (role == "leader")

    amendment = bridge.invoke({"operation": "task",
                               "arguments": {**args, "inputId": "input-a", "relatedInputIds": ["input-b"]},
                               "context": context}, call=call)
    assert "[Task input coverage]" not in amendment["fact"]
    assert amendment["contextUpdate"] == first["contextUpdate"]
    assert amendment["phaseComplete"] is (role == "leader")


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
    ({'action': 'plan', 'taskId': 'main', 'nodes': [], '_nativeRunId': 'foreign'}, {'role': 'leader', 'planning': ['main']}),
])
def test_wrong_scope_or_model_private_fields_never_reach_cli(bridge, arguments, context):
    with pytest.raises(ValueError):
        bridge.invoke({'operation': 'task', 'arguments': arguments, 'context': context},
                      call=lambda _: pytest.fail('unbound operation executed'))


@pytest.mark.parametrize('action', ['accept', 'reject'])
def test_review_uses_prepared_submission_when_model_omits_it(bridge, action):
    arguments = {'action': action, 'taskId': 't1', 'note': 'Verified the file.'}
    calls = []

    def call(bound):
        calls.append(bound)
        return {'ok': False, 'effect': 'none'}

    bridge.invoke({'operation': 'task', 'arguments': arguments,
                   'context': {'role': 'leader', 'review': ['t1', 'current']}}, call=call)
    assert calls == [{**arguments, 'submissionId': 'current'}]
    assert 'submissionId' not in arguments


@pytest.mark.parametrize("role", ["manager", "leader"])
def test_create_keeps_source_binding_and_finishes_only_leader_intake(bridge, role):
    original = {'inputId': 'ref', 'eventId': '$source', 'roomId': '!room:test', 'userId': '@human:test'}
    calls = []
    def call(arguments):
        calls.append(arguments)
        return {'sourceEventId': '$source', 'task': {'id': 'main', 'kind': 'main', 'status': 'planned'},
                'notification': {'status': 'queued', 'recipient': 'lead', 'eventId': 'notice'}}
    args = {'action': 'create', 'leader': 'lead', 'title': 'work'}
    result = bridge.invoke({'operation': 'task', 'arguments': args,
                            'context': {'role': role, 'sources': [original]}}, call=call)
    assert calls[0]['_nativeSource'] == {'eventId': '$source', 'roomId': '!room:test', 'userId': '@human:test'}
    assert '_nativeSource' not in args
    assert result['fact'].startswith('[Task system receipt]')
    assert result['phaseComplete'] is (role == 'leader')


@pytest.mark.parametrize("role", ["manager", "leader"])
def test_late_inputs_remain_user_inputs_and_require_native_checkpoint(bridge, role):
    source = {'inputId': 'ref', 'eventId': '$source', 'roomId': '!room:test', 'userId': '@human:test'}
    update = {'ok': True, 'outcome': 'input_changed', 'noTaskRegistered': True,
              'sourceEventId': '$source', 'inputs': [{'eventId': '$new', 'roomId': '!room:test',
                                                    'userId': '@human:test', 'body': 'Unique amendment fixture 83f2'}]}
    result = bridge.invoke({'operation': 'task', 'arguments': {'action': 'create', 'leader': 'lead', 'title': 'work'},
                            'context': {'role': role, 'sources': [source]}}, call=lambda _: update)
    assert result['phaseComplete'] is False
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


def test_verified_submission_displays_saved_business_conclusion_and_artifact_names(bridge):
    result = bridge.invoke({'operation': 'task', 'arguments': {'action': 'submit', 'taskId': 't1', 'summary': 'uncommitted wording'},
                            'context': {'role': 'worker', 'execution': 't1'}},
                           call=lambda _: {'task': {'id': 't1', 'status': 'submitted', 'submissionId': 's1',
                             'candidate': {'id': 's1', 'summary': '页面搜索功能已实现，独立检查通过。',
                               'artifacts': [{'artifactId': 'html', 'name': 'index.html'}], 'report': 'Large repeated report'}},
                             'notification': {'status': 'queued', 'recipient': 'lead', 'eventId': 'notice'}})
    assert result['phaseComplete'] is True
    assert result['displayText'] == '页面搜索功能已实现，独立检查通过。\n\n产物：index.html'
    assert 'uncommitted wording' not in result['displayText']
    assert 'Large repeated report' not in result['displayText']
    assert 'Task system receipt' not in result['displayText']


def test_direct_handoff_ends_worker_phase_without_notifying_leader(bridge):
    result = bridge.invoke({'operation': 'task', 'arguments': {'action': 'submit', 'taskId': 't1', 'summary': 'done'},
                            'context': {'role': 'worker', 'execution': 't1'}},
        call=lambda _: {'task': {'id': 't1', 'status': 'completed', 'submissionId': 's1',
                                 'handoff': {'submissionId': 's1', 'actorName': 'dev', 'actorRole': 'worker'}},
                         'mainProjection': 'applied', 'dispatchStatus': 'ready',
                         'assignments': [{'taskId': 'review', 'status': 'queued', 'recipient': 'reviewer', 'eventId': 'notice'}]})
    assert result['phaseComplete'] is True
    assert 'immutable delivery' in result['fact']
    assert 'Do not notify the Leader' in result['fact']


def test_incomplete_handoff_proof_cannot_end_native_execution(bridge):
    result = bridge.invoke({'operation': 'task', 'arguments': {'action': 'submit', 'taskId': 't1', 'summary': 'done'},
                            'context': {'role': 'worker', 'execution': 't1'}},
        call=lambda _: {'task': {'id': 't1', 'status': 'completed', 'submissionId': 's1',
                                 'handoff': {'submissionId': 'other', 'actorName': 'dev', 'actorRole': 'worker'}},
                         'mainProjection': 'applied', 'dispatchStatus': 'ready', 'assignments': []})
    assert result['phaseComplete'] is False


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
    assert 'displayText' not in second


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


def test_delivery_followup_rehydrates_seen_task_in_the_current_turn(bridge):
    task = 'task-' + 'a' * 32
    room = '!room:test'
    old = {'eventId': 'seen', 'status': 'completed', 'roomId': room, 'taskId': task,
           'reportMessageId': '$old', 'report': 'CSV report with 12 passing tests', 'artifacts': []}
    new = dict(old, eventId='new', taskId='task-' + 'b' * 32, report='Unrelated report',
               reportMessageId='$new', deliveredAt='2026-11-22T02:00:00Z')
    other_room = dict(old, eventId='outside', roomId='!outside:test', report='Unrelated private report')
    result = bridge.invoke({'operation': 'delivery_context', 'room': room,
                            'middle': {'teamharness.sent_deliveries': {room: {'seen': True}}},
                            'inputText': 'What was delivered for ' + task + '?'},
                           get_json=lambda _: {'deliveries': [new, other_room, old]})
    assert 'CSV report with 12 passing tests' in result['facts'][0]
    assert len(result['facts']) == 1
    assert result['acknowledgements'] == {}


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


def test_leader_inline_create_continues_to_plan_without_self_handoff(bridge):
    source = {'inputId': 'ref', 'eventId': '$source', 'roomId': '!room:test', 'userId': '@human:test'}
    context = {'role': 'leader', 'sources': [source]}
    created = {'sourceEventId': '$source', 'task': {'id': 'main', 'kind': 'main', 'status': 'planned', 'originEventId': '$source'},
               'notification': {'status': 'inline', 'recipient': 'lead'}}
    result = bridge.invoke({'operation': 'task', 'arguments': {'action': 'create', 'leader': 'lead', 'title': 'work'},
                            'context': context}, call=lambda _: created)
    assert result['phaseComplete'] is False
    assert result['contextUpdate']['planning'] == ['main']
    context.update(result['contextUpdate'])
    planned = {'task': {'id': 'main', 'kind': 'main', 'status': 'in_progress', 'planId': 'plan'},
               'progress': 'ready', 'assignments': []}
    result = bridge.invoke({'operation': 'task', 'arguments': {'action': 'plan', 'taskId': 'main',
                            'nodes': [{'id': 'a', 'assignedTo': 'dev', 'spec': 'Work', 'dependsOn': []}]},
                            'context': context}, call=lambda _: planned)
    assert result['phaseComplete'] is True


def test_leader_creation_ends_intake_and_moves_planning_to_task_room(bridge):
    source = {'inputId':'ref','eventId':'$source','roomId':'!dm:test','userId':'@human:test'}
    created = {'sourceEventId':'$source','task':{'id':'main','kind':'main','title':'HTML page','status':'planned','roomId':'!task:test'},
               'notification':{'status':'queued','recipient':'lead','eventId':'event'}}
    result=bridge.invoke({'operation':'task','arguments':{'action':'create','leader':'lead','title':'HTML page'},
        'context':{'role':'leader','sources':[source]}},call=lambda _:created)
    assert result['phaseComplete'] is True
    assert 'planning' not in result['contextUpdate']
    assert 'HTML page' in result['displayText'] and '任务房间' in result['displayText']
    assert 'main/collaboration?room=' in result['displayText']
    assert 'Task system receipt' in result['fact']


def test_dispatch_receipts_confirm_actual_sender_event_without_a_model_turn(bridge):
    result={'task':{'id':'source','mainTaskId':'main','submissionId':'s1'},
            'assignments':[{'taskId':'next','status':'queued','eventId':'notice'}]}
    saved={'event_id':'notice','kind':'task_assigned','task_id':'next','actor':'@sender:test',
           'recipient':'@tester:test','room_id':'!task:test','native_id':'matrix:$sent'}
    rows=bridge.dispatched_receipts(result,'@sender:test',lambda _:saved)
    assert len(rows)==1
    assert '"status":"sent"' in rows[0]['fact'] and '"matrixEventId":"$sent"' in rows[0]['fact']
    for current in ["", "matrix:$internal-recovery"]:
        retried = {**saved, "previous_native_ids": ["matrix:$sent"], "native_id": current}
        assert bridge.dispatched_receipts(result, "@sender:test", lambda _: retried) == rows
    for change in [{'actor':'@another:test'},{'native_id':''},{'event_id':'foreign'},{'task_id':'other'}]:
        assert bridge.dispatched_receipts(result,'@sender:test',lambda _:{**saved,**change})==[]
    def unavailable(_):raise OSError('temporarily unavailable')
    assert bridge.dispatched_receipts(result,'@sender:test',unavailable)==[]


def test_model_context_bridge_uses_public_projection_and_rejects_unprepared_values(bridge):
    value={"spec":"Your node", "originalInputs":[{"body":"Original requirement"}],
           "inputs":[{"summary":"Reviewed", "files":[{"localPath":"/tmp/versioned/result.json", "preview":"large file body"}]}],
           "acceptedResults":[{"candidate":{"report":"large report body", "reportFile":{"artifactId":"report-id"}}}]}
    result=bridge.invoke({"operation":"model_context","context":value})
    assert result==bridge.task_context.model_context(value)
    assert "large file body" not in json.dumps(result)
    assert "large report body" not in json.dumps(result)
    assert value["acceptedResults"][0]["candidate"]["report"]=="large report body"
    with pytest.raises(ValueError,match="prepared Task context"):
        bridge.invoke({"operation":"model_context","context":[]})


def test_dispatch_receipt_waits_for_actual_send_without_starting_another_turn(bridge,monkeypatch):
    result={"task":{"id":"source","mainTaskId":"main","submissionId":"s1"},
            "assignments":[{"taskId":"next","status":"queued","eventId":"notice"}]}
    saved={"event_id":"notice","kind":"task_assigned","task_id":"next","actor":"@sender:test",
           "recipient":"@tester:test","room_id":"!task:test","native_id":""}
    reads=[]
    def fetch(_):
        reads.append(1)
        return dict(saved,native_id="matrix:$sent" if len(reads)>1 else "")
    sleeps=[]
    monkeypatch.setattr(bridge.time,"sleep",sleeps.append)
    rows=bridge.dispatched_receipts(result,"@sender:test",fetch,wait_for_send=True)
    assert len(rows)==1 and len(reads)==2 and len(sleeps)==1
    assert '"status":"sent"' in rows[0]["fact"]


def test_dispatch_wait_expires_without_claiming_delivery(bridge,monkeypatch):
    result={"task":{"id":"source","submissionId":"s1"},"assignments":[{"taskId":"next","status":"queued","eventId":"notice"}]}
    saved={"event_id":"notice","kind":"task_assigned","task_id":"next","actor":"@sender:test","native_id":""}
    ticks=iter([0,1,3]);monkeypatch.setattr(bridge.time,"monotonic",lambda:next(ticks))
    monkeypatch.setattr(bridge.time,"sleep",lambda _:None)
    assert bridge.dispatched_receipts(result,"@sender:test",lambda _:saved,wait_for_send=True)==[]


def test_confirmed_handoff_only_reaches_the_submitting_native_session(bridge, tmp_path, monkeypatch):
    monkeypatch.setenv('AGENTTEAMS_WORKER_HOME', str(tmp_path))
    result = {'task': {'id': 'stage', 'mainTaskId': 'main', 'submissionId': 's1', 'spec': 'private business input'},
              'assignments': [{'taskId': 'next', 'status': 'queued', 'eventId': 'dispatch'}]}
    owner = '@developer:test'
    target = {'runtime': 'hermes', 'session': 'native-session'}
    for _ in range(2):
        bridge.task_handoff_context.remember(result, owner, target)
    files = list((tmp_path / '.agentteams/native-handoffs').glob('*.json'))
    assert len(files) == 1 and 'private business input' not in files[0].read_text()
    receipt = {'event_id': 'dispatch', 'kind': 'task_assigned', 'task_id': 'next', 'actor': owner,
               'recipient': '@tester:test', 'room_id': '!room:test', 'native_id': ''}
    def read(runtime):
        return bridge.invoke({'operation': 'confirmed_handoffs', 'runtime': runtime}, get_json=lambda _: receipt)['items']
    assert read('hermes') == []
    receipt['native_id'] = 'matrix:$actual'
    assert read('openhuman') == []
    items = read('hermes')
    assert len(items) == 1 and items[0]['target'] == target
    assert (items[0]['taskId'], items[0]['submissionId']) == ('stage', 's1')
    assert len(items[0]['facts']) == 1 and '"matrixEventId":"$actual"' in items[0]['facts'][0]


@pytest.mark.parametrize("requested,expected", [("page", "dev"), ("test-report", "test"), ("shared", None), ("unknown", None)])
def test_final_review_reads_only_unambiguous_accepted_artifacts(bridge, monkeypatch, requested, expected):
    import task_canonical
    monkeypatch.setattr(task_canonical, "admit", lambda _: {"ok": True})
    accepted = [{"id": "dev", "candidate": {"artifacts": [{"artifactId": "page"}, {"artifactId": "shared"}]}},
                {"id": "test", "candidate": {"artifacts": [{"artifactId": "shared"}], "reportFile": {"artifactId": "test-report"}}}]
    calls = []
    def call(args):
        calls.append(args)
        return {"ok": False, "effect": "none"}
    bridge.invoke({"operation": "task", "arguments": {"action": "files_read", "artifactId": requested},
                   "context": {"role": "leader", "completion": "main", "context": {"acceptedResults": accepted}}}, call=call)
    assert calls[0].get("taskId") == expected
    for input_id, resolved in [("main", expected or "main"), ("explicit-child", "explicit-child")]:
        bridge.invoke({"operation": "task", "arguments": {"action": "files_read", "taskId": input_id, "artifactId": requested},
                       "context": {"role": "leader", "completion": "main", "context": {"acceptedResults": accepted}}}, call=call)
        assert calls[-1]["taskId"] == resolved
