"""Shared native tool-operation checkpoint construction, with no native state."""
import json
from task_recovery import Recovery, RecoveryConflict


def begin(checkpoint, anchor, calls):
    if (not isinstance(calls, list) or not calls
            or any(not isinstance(call, dict) or not call.get('id') or not call.get('name')
                   or 'arguments' not in call for call in calls)):
        raise RecoveryConflict('Native tool calls require IDs, names and arguments')
    ids = [call['id'] for call in calls]
    if len(set(ids)) != len(ids):
        raise RecoveryConflict('Native tool call IDs must be unique')
    cp = dict(checkpoint or {})
    if (cp.get('operation') or {}).get('state') in {'started', 'unknown'}:
        raise RecoveryConflict('An unconfirmed native operation requires verification')
    operation = 'tools:' + Recovery.digest([anchor, calls])
    if operation in (cp.get('completed_steps') or []):
        raise RecoveryConflict('Tool operation already completed')
    expected = [{'id': call['id'], 'name': call['name'],
                 'arguments_sha256': Recovery.digest(call['arguments'])} for call in calls]
    for call, evidence in zip(calls, expected):
        if call.get('read_only') is True:
            evidence['read_only'] = True
        codes = call.get('pre_effect_error_codes', [])
        if not isinstance(codes, list) or any(not isinstance(code, str) or not code for code in codes):
            raise RecoveryConflict('Native pre-effect error codes must be nonempty strings')
        if codes:
            evidence['pre_effect_error_codes'] = codes
    cp.update(version=cp.get('version', 0) + 1,
              operation={'id': operation, 'state': 'started', 'evidence': json.dumps(expected)})
    return operation, cp


def pending(checkpoint, operation):
    cp = dict(checkpoint or {})
    saved = cp.get('operation') or {}
    if saved.get('id') != operation or saved.get('state') != 'started':
        raise RecoveryConflict('Native results do not own an outstanding tool operation')
    return cp


def result_bytes(checkpoint, operation, results):
    cp = pending(checkpoint, operation)
    expected = json.loads(cp['operation']['evidence'])
    read_only = {call['id'] for call in expected if call.get('read_only') is True}
    pre_effect_codes = {call['id']: call.get('pre_effect_error_codes', []) for call in expected}
    if (not isinstance(results, list) or any(not isinstance(result, dict) or 'id' not in result
            or 'content' not in result or result.get('effect') == 'unknown'
            or (result.get('effect') == 'none'
                and (result.get('status') != 'error' or
                     (result['id'] not in read_only
                      and result.get('error_code') not in pre_effect_codes.get(result['id'], []))))
            for result in results)
            or sorted(result['id'] for result in results) != sorted(call['id'] for call in expected)):
        raise RecoveryConflict('Native tool receipts differ from the admitted calls or remain unconfirmed')
    return json.dumps({'calls': expected, 'results': results}, ensure_ascii=False).encode()


def finish(checkpoint, operation, artifact):
    cp = pending(checkpoint, operation)
    cp.update(version=cp['version'] + 1,
              completed_steps=[*(cp.get('completed_steps') or []), operation],
              artifacts=[*(cp.get('artifacts') or []), artifact],
              operation={'id': operation, 'state': 'verified', 'evidence': artifact['key']})
    return cp


def tool_policy(registration):
    """Only a native-owned binding to the fixed canonical bridge delegates.

    Callers construct registration from their SDK registration, separately from
    model arguments. A tool name or a supplied kind marker is not authority.
    Delegation does not permit replay after an unknown canonical RPC result.
    """
    if registration == {'origin': 'agentteams.common.task_native',
                        'entrypoint': '/opt/agentteams/plugins/teamharness/mcp/common/task_native.py'}:
        return {'mode': 'delegate', 'unknown': 'verify_canonical_state'}
    return {'mode': 'journal', 'unknown': 'verify_started_operation'}


def admit(checkpoint):
    """Permit normal inference/tools only after known effects and delivery."""
    cp = checkpoint or {}
    saved = cp.get('operation')
    if cp.get('delivery') == 'pending':
        raise RecoveryConflict('A saved result requires delivery-only recovery')
    if saved and saved.get('state') != 'verified':
        raise RecoveryConflict('An unconfirmed native operation requires verification')
    return True
