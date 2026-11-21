"""Canonical ambiguity is durable, and never converted to an automatic replay."""
import hashlib
import json
import os
from pathlib import Path
import sys
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'teamharness/mcp/common'))
import task_canonical as guard


def plan_result():
    return {'task': {'id': 'main', 'kind': 'main', 'planId': 'saved-plan', 'status': 'in_progress'},
            'progress': 'pending', 'assignments': []}


class Ledger:
    def __init__(self):
        self.saved = None
        self.fail_finish_response = False
        self.calls = []

    def __call__(self, payload):
        self.calls.append(payload)
        if payload['mode'] == 'begin':
            if self.saved:
                return {'mode': 'recovered', 'result': self.saved['result']} if self.saved.get('result') else {'mode': 'verify', 'authority': {'task': {'id': 'main', 'status': 'in_progress'}}}
            self.saved = {'id': 'canonical-id', 'state': 'started'}
            return {'mode': 'execute', 'operationId': 'canonical-id'}
        if payload['mode'] == 'finish':
            self.saved.update(state='verified', result=payload['result'])
            if self.fail_finish_response:
                raise TimeoutError('own injected lost finish response')
            return {'mode': 'verified'}
        if payload['mode'] == 'unknown':
            if self.saved['state'] != 'verified':
                self.saved['state'] = 'unknown'
            return {'mode': self.saved['state']}
        return {'mode': 'verify'}


def test_timeout_and_new_native_instance_never_replay_even_when_task_still_active(monkeypatch):
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://own-fixture')
    ledger = Ledger()
    call = Mock(side_effect=TimeoutError('own CLI response lost'))
    args = {'action': 'plan', 'taskId': 'main', 'nodes': []}
    first = guard.run(args, call, rpc=ledger)
    second = guard.run(dict(args, nodes=[{'id': 'different'}]), call, rpc=ledger)
    assert first['effect'] == second['effect'] == 'unknown'
    assert second['authority']['task']['status'] == 'in_progress'
    call.assert_called_once()
    assert ledger.saved['state'] == 'unknown'


def test_known_prevalidation_rejection_creates_no_ledger_or_cli_call(monkeypatch):
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://own-fixture')
    rpc, call = Mock(), Mock()
    def invalid(_):
        raise ValueError('invalid node scope')
    assert guard.run({'action': 'plan'}, call, validate=invalid, rpc=rpc)['effect'] == 'none'
    rpc.assert_not_called()
    call.assert_not_called()


def test_cli_nonzero_or_malformed_result_keeps_unknown_marker(monkeypatch):
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://own-fixture')
    for result in ({'ok': False, 'effect': 'unknown'}, {'task': {'id': 'different'}}):
        ledger = Ledger()
        assert guard.run({'action': 'plan', 'taskId': 'main'}, lambda _: result, rpc=ledger)['effect'] == 'unknown'
        assert ledger.saved['state'] == 'unknown'


def test_durably_cached_receipt_survives_lost_finish_response_without_second_cli(monkeypatch):
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://own-fixture')
    ledger = Ledger()
    ledger.fail_finish_response = True
    call = Mock(return_value=plan_result())
    args = {'action': 'plan', 'taskId': 'main'}
    assert guard.run(args, call, rpc=ledger)['effect'] == 'unknown'
    assert guard.run(args, call, rpc=ledger) == plan_result()
    call.assert_called_once()


def test_readonly_authority_lookup_is_not_recorded_as_a_mutation(monkeypatch):
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://own-fixture')
    call, rpc = Mock(return_value={'task': {'id': 'main'}}), Mock()
    assert guard.run({'action': 'get', 'taskId': 'main'}, call, rpc=rpc)['task']['id'] == 'main'
    rpc.assert_not_called()


def test_admission_checks_every_trusted_task_and_never_treats_active_as_no_effect(monkeypatch):
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://own-fixture')
    rpc = Mock(side_effect=[{'mode': 'admitted'}, {'mode': 'verify', 'authority': {'task': {'status': 'in_progress'}}}])
    result = guard.admit({'role': 'leader', 'planning': ['first', 'second'], 'team': 'own-team'}, rpc=rpc)
    assert result['effect'] == 'unknown'
    assert rpc.call_args_list[1].args[0]['arguments'] == {'action': 'get', 'taskId': 'second', 'team': 'own-team'}


def test_manager_source_admission_has_no_model_supplied_leader_or_path(monkeypatch):
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://own-fixture')
    rpc = Mock(return_value={'mode': 'admitted'})
    source = {'eventId': '$own', 'roomId': '!own:test', 'userId': '@human:test', 'inputId': 'ref'}
    assert guard.admit({'role': 'manager', 'sources': [source]}, rpc=rpc)['ok']
    assert rpc.call_args.args[0]['arguments'] == {'action': 'create', '_nativeSource': {key: source[key] for key in ('eventId', 'roomId', 'userId')}}


def test_file_fingerprint_uses_actual_prepared_output_content(monkeypatch, tmp_path):
    monkeypatch.setenv('TEAMHARNESS_SHARED_DIR', str(tmp_path))
    outputs = tmp_path / 'tasks/T/workspace'
    outputs.mkdir(parents=True)
    target = outputs / 'result.txt'
    target.write_bytes(b'first')
    args = {'action': 'submit', 'taskId': 'T', 'files': ['result.txt']}
    assert guard.fingerprints(args)['result.txt'] == hashlib.sha256(b'first').hexdigest()
    target.write_bytes(b'second')
    assert guard.fingerprints(args)['result.txt'] == hashlib.sha256(b'second').hexdigest()


def test_recovered_creation_receipt_is_available_for_native_context_injection(monkeypatch):
    monkeypatch.setenv('AGENTTEAMS_CONTROLLER_URL', 'http://own-fixture')
    source = {'eventId': '$own', 'roomId': '!own:test', 'userId': '@human:test'}
    result = {'sourceEventId': '$own', 'task': {'id': 'main', 'kind': 'main', 'status': 'planned'},
              'notification': {'status': 'queued', 'recipient': 'leader', 'eventId': 'coordination'}}
    rpc = Mock(return_value={'mode': 'admitted', 'action': 'create', 'sourceEventId': '$own', 'result': result})
    admission = guard.admit({'role': 'manager', 'sources': [source]}, rpc=rpc)
    assert admission['ok'] and len(admission['receipts']) == 1
    assert 'queued its handoff' in admission['receipts'][0]['fact']
    assert admission['receipts'][0]['receiptId']


def test_native_public_tool_checks_durable_admission_before_any_cli(monkeypatch):
    import task_native
    blocked = {'ok': False, 'effect': 'unknown', 'error': 'canonical_operation_requires_verification'}
    monkeypatch.setattr(guard, 'admit', lambda context: blocked)
    call = Mock()
    result = task_native.invoke({'operation': 'task', 'arguments': {'action': 'get', 'taskId': 'T'},
                                 'context': {'role': 'worker', 'execution': 'T'}}, call=call)
    assert result == blocked
    call.assert_not_called()
