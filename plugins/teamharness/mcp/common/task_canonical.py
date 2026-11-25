"""Controller-backed fence for canonical CLI effects, shared by every Harness.

The ledger is separate from generic execution operations, so it never blocks a
canonical submit's own transaction. Absence of a visible change is not proof
that an ambiguous request had no effect.
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import urllib.request
import urllib.error

if __package__:
    from . import task_receipt as receipts
else:
    import task_receipt as receipts

_MUTATIONS = frozenset({'create', 'plan', 'begin', 'submit', 'accept', 'reject', 'complete', 'cancel', 'files_put'})


def request(payload):
    token = os.getenv('AGENTTEAMS_AUTH_TOKEN') or Path(
        os.getenv('AGENTTEAMS_AUTH_TOKEN_FILE') or '/var/run/secrets/agentteams/token').read_text().strip()
    target = os.environ['AGENTTEAMS_CONTROLLER_URL'].rstrip('/') + '/api/v1/tasks/native-operations'
    req = urllib.request.Request(target, data=json.dumps(payload).encode(), method='POST',
        headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json',
                 'X-AgentTeams-Process-ID': os.getenv('AGENTTEAMS_PROCESS_ID', '')})
    try:
        with urllib.request.urlopen(req, timeout=15) as response:
            result = json.load(response)
    except urllib.error.HTTPError as error:
        # Only this Controller's explicit pre-ledger rejection is correctable.
        # A status code alone says nothing about an earlier durable operation.
        if payload.get('mode') == 'begin' and error.code in {403, 409}:
            result = json.load(error)
            if (isinstance(result, dict) and result.get('ok') is False
                    and result.get('effect') == 'none'
                    and result.get('error') in {'invalid_task_handoff', 'invalid_task_plan_assignee', 'repeated_task_request'}):
                return result
        raise
    if not isinstance(result, dict):
        raise ValueError('invalid canonical operation ledger response')
    return result


def _failure(authority=None, *, stage=None, http_status=None):
    result = {'ok': False, 'effect': 'unknown', 'error': 'canonical_operation_requires_verification',
              'message': 'Canonical Task outcome requires authoritative verification; do not replay the call.'}
    if authority is not None:
        result['authority'] = authority
    if stage is not None:
        result['failureStage'] = stage
    if type(http_status) is int and 400 <= http_status <= 599:
        result['httpStatus'] = http_status
    return result


def receipt(arguments, result):
    action, task = arguments.get('action'), arguments.get('taskId')
    if action == 'create':
        return receipts.creation_receipt(result, (arguments.get('_nativeSource') or {}).get('eventId'))
    if action == 'plan':
        return receipts.planning_receipt(result, task)
    if action in {'accept', 'reject'}:
        return receipts.review_receipt(result, task, arguments.get('submissionId'), action)
    return receipts.task_receipt(result, task, action)


def confirmed(arguments, result):
    if not isinstance(result, dict) or result.get('ok') is False:
        return False
    if arguments['action'] == 'create':
        source = arguments.get('_nativeSource') or {}
        if source and receipts.refresh_creation_inputs(result, [source], source.get('eventId')):
            return True  # Successful API control result explicitly registered no Task.
    if arguments['action'] in {'create', 'plan', 'submit', 'accept', 'reject', 'complete'}:
        return receipt(arguments, result) is not None
    task = result.get('task') or {}
    if arguments['action'] == 'begin':
        return task.get('id') == arguments.get('taskId') and task.get('status') == 'in_progress'
    if arguments['action'] == 'cancel':
        return task.get('id') == arguments.get('taskId') and task.get('status') == 'cancelled'
    return arguments['action'] == 'files_put' and bool(result.get('artifactId')) and bool(result.get('version'))


def fingerprints(arguments):
    names = arguments.get('files', []) if arguments.get('action') == 'submit' else [arguments['source']] if arguments.get('action') == 'files_put' else []
    if not names:
        return {}
    root = Path(os.environ.get('TEAMHARNESS_SHARED_DIR') or os.environ['AGENTTEAMS_SHARED_DIR']).resolve() / 'tasks' / arguments['taskId'] / 'workspace'
    found = {}
    for name in names:
        target = (root / name).resolve()
        if Path(name).is_absolute() or '..' in Path(name).parts or not target.is_relative_to(root):
            raise ValueError('output escapes prepared Task directory')
        found[name] = hashlib.sha256(target.read_bytes()).hexdigest()
    return found


def run(arguments, call, *, validate=None, rpc=None):
    if arguments.get('action') not in _MUTATIONS or not os.getenv('AGENTTEAMS_CONTROLLER_URL'):
        return call(arguments)
    # Known pre-validation failures happen before any durable start or CLI.
    try:
        if validate:
            validate(arguments)
        content = fingerprints(arguments)
    except (OSError, ValueError, KeyError) as error:
        return {'ok': False, 'effect': 'none', 'error': 'invalid_task_arguments', 'message': str(error)}
    rpc = rpc or request
    payload = {'arguments': arguments, 'contentFingerprints': content}
    try:
        admission = rpc(dict(payload, mode='begin'))
    except (OSError, ValueError) as error:
        return _failure(stage='begin', http_status=getattr(error, 'code', None))
    if admission.get('effect') == 'none' and admission.get('ok') is False:
        return admission
    if admission.get('mode') == 'recovered':
        result = admission.get('result')
        return result if confirmed(arguments, result) else _failure()
    if admission.get('mode') != 'execute' or not admission.get('operationId'):
        return _failure(admission.get('authority'))
    payload['operationId'] = admission['operationId']
    stage, http_status = 'cli', None
    try:
        result = call(arguments)
        if confirmed(arguments, result):
            stage = 'finish'
            saved = rpc(dict(payload, mode='finish', result=result))
            if saved.get('mode') == 'verified':
                return result
        elif isinstance(result, dict) and result.get('ok') is False:
            http_status = result.get('httpStatus')
        else:
            stage = 'receipt'
    except (OSError, ValueError) as error:
        http_status = getattr(error, 'code', None)
    # A failed marker write cannot authorize replay: the earlier start remains.
    try:
        rpc(dict(payload, mode='unknown'))
    except (OSError, ValueError):
        pass
    return _failure(stage=stage, http_status=http_status)


def admit(context, *, rpc=None):
    if not os.getenv('AGENTTEAMS_CONTROLLER_URL'):
        return {'ok': True, 'receipts': []}
    rpc = rpc or request
    requests = []
    if context.get('role') in {'manager', 'leader', 'team_leader'}:
        requests.extend({'action': 'create', '_nativeSource': {key: source[key] for key in ('eventId', 'roomId', 'userId')}}
                        for source in context.get('sources') or [])
    ids = [context.get('execution'), context.get('completion'), *(context.get('planning') or [])]
    review = context.get('review')
    if isinstance(review, (list, tuple)) and review:
        ids.append(review[0])
    prepared = context.get('context') or {}
    team = context.get('team') or prepared.get('team') or (prepared.get('task') or {}).get('team')
    requests.extend(dict({'action': 'get', 'taskId': identity}, **({'team': team} if team else {}))
                    for identity in dict.fromkeys(identity for identity in ids if identity))
    facts = []
    for arguments in requests:
        try:
            admission = rpc({'mode': 'admit', 'arguments': arguments})
        except (OSError, ValueError):
            return _failure()
        if admission.get('mode') != 'admitted':
            return _failure(admission.get('authority'))
        facts.extend(admission_receipts(admission, arguments.get('taskId')))
    return {'ok': True, 'receipts': facts}


def admission_receipts(admission, task_id=None):
    if not isinstance(admission, dict) or not admission.get('result'):
        return []
    original = {'action': admission.get('action'), 'taskId': task_id,
                'submissionId': admission.get('submissionId'),
                '_nativeSource': {'eventId': admission.get('sourceEventId')}}
    saved = receipt(original, admission['result'])
    return [{'receiptId': saved[0], 'fact': saved[1]}] if saved else []
