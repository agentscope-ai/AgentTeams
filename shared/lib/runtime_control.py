"""Common AT authority for task supplements; native adapters only dispatch them.

Inputs are Controller-generated member configuration, a locally active Task
identity, and decoded transport fields. Never populate them from model text.
"""
import re


def task_supplement(config, task, source):
    """Authorize a directed supplement without granting stop or approval rights."""
    if not task or not task.get('task_id'):
        return False
    team = config.get('team') or {}
    own = (config.get('member') or {}).get('matrixUserId')
    sender = source.get('sender')
    if (not own or sender == own or not source.get('event_id')
            or not team.get('name') or task.get('team_id') != team['name']
            or not task.get('room_id') or source.get('room_id') != task['room_id']
            or source.get('answer') is not None or source.get('execution_control') is not None):
        return False
    owners = {m.get('matrixUserId') for m in team.get('members', [])
              if team.get('leaderName') and m.get('name') == team['leaderName'] and m.get('role') == 'team_leader'}
    owners.add((team.get('admin') or {}).get('matrixUserId'))
    if not sender or sender not in owners:
        return False
    text = str(source.get('body') or '').strip()
    if text.startswith('/steer '):
        text = text.removeprefix('/steer ').strip()
    if not text or text.startswith('/'):
        return False
    return own in (source.get('mentions') or []) or bool(re.search(
        r'(?<!\S)' + re.escape(own) + r'(?=$|[\s,，。;；!?！？])', text))


def _current_member_config(config):
    """Read only the current projection belonging to this runtime member."""
    import os
    from pathlib import Path
    path = os.getenv('TEAMHARNESS_RUNTIME_CONFIG')
    if path:
        import yaml
        current = yaml.safe_load(Path(path).read_text())
        member = config.get('member') or {}
        actual = current.get('member') or {}
        if any(member.get(key) and member[key] != actual.get(key) for key in ('runtimeName', 'runtime')):
            raise ValueError('Approval policy belongs to a different runtime member')
        config = current
    return config


def _approval_mode(config):
    mode = (config.get('desired') or {}).get('approvalMode') or None
    if mode not in {None, 'ask', 'allow'}:
        raise ValueError('approvalMode must be ask or allow')
    return mode


def approval_mode(config):
    """Resolve the optional Agent-wide native tool approval setting."""
    return _approval_mode(_current_member_config(config))


def mcp_approval(config, server_name):
    """Resolve an assigned server's override, then the Agent-wide policy."""
    config = _current_member_config(config)
    for server in (config.get('desired') or {}).get('mcpServers') or []:
        if server.get('name') != server_name:
            continue
        policy = server.get('approvals')
        if policy is None:
            return _approval_mode(config)
        if policy.get('tools') != '*' or policy.get('mode') not in {'ask', 'allow'}:
            raise ValueError('MCP approvals require tools * and mode ask or allow')
        return policy['mode']
    return None


def admit_native_supplement(matrix, key):
    """Claim an ordinary input before its first Task-native write or steer."""
    from task_recovery import RecoveryConflict
    claimed = matrix.message_claim(key)
    if claimed['recovery'] == 'done':
        return False
    if claimed['recovery'] != 'resume':
        raise RecoveryConflict('Task supplement requires ordinary input recovery before native steering')
    return True


def retire_native_supplement(matrix, key):
    """Retire ordinary input only after this process has a durable native receipt.

    Native SDK adapters must read back their real session before committing that
    receipt. An unfinished ordinary tool operation remains fenced for review.
    """
    from task_recovery import RecoveryConflict
    snapshot = matrix.native_input(key)
    receipt = snapshot['input'].get('native_commit') or {}
    if not matrix.native_current(snapshot, receipt.get('native_ref'), receipt.get('native_digest')):
        raise RecoveryConflict('Task supplement has no current native persistence receipt')
    claimed = matrix.message_claim(key)
    if claimed['recovery'] == 'resume':
        matrix.message_cancel(claimed['identity'])
    elif claimed['recovery'] == 'verify':
        raise RecoveryConflict('Task supplement input still has an unverified ordinary operation')


def task_room_assignment(config, snapshot, room):
    """Select only one unfinished, assigned execution from an authorized scan.

    A native adapter supplies Controller discovery results, never model text.
    An ambiguous room stays pending rather than attaching to an arbitrary node.
    """
    from task_recovery import RecoveryConflict
    member = config.get('member') or {}
    team = config.get('team') or {}
    owners = {member.get('runtimeName'), member.get('name'), member.get('matrixUserId')} - {None, ''}
    candidates = []
    for entry in snapshot.get('tasks') or []:
        task = entry.get('task') or {}
        if (task.get('room_id') != room or task.get('task_kind') != 'execution'
                or task.get('status') not in {'assigned', 'in_progress'}
                or task.get('assigned_to') not in owners or entry.get('team_id') != team.get('name')):
            continue
        candidates.append(entry)
    if len(candidates) > 1:
        raise RecoveryConflict('Task room has multiple unfinished assignments; select the execution explicitly')
    return candidates[0] if candidates else None


def task_run_id(team, task, generation):
    """Opaque common display identity; native session identity stays independent."""
    return f'task:{team}:{task}:{generation}'


_published_retired_runs = set()


def retired_task_runs(team, task, process):
    """Render Controller-persisted retired generations, including a lost send.

    Neither ending a display run nor acknowledging its Matrix delivery verifies
    an unknown tool effect or changes the Task's business status.
    """
    import hashlib
    execution = task.get('execution') or {}
    if not process or execution.get('process_id') != process or not task.get('room_id'):
        return []
    endings = []
    for previous in execution.get('retired_runs') or []:
        generation = previous.get('generation')
        if (type(generation) is not int or generation <= 0 or generation >= execution.get('generation', 0)
                or previous.get('kind') not in {'quiet', 'failed'}):
            raise ValueError('Invalid Controller retired execution record')
        run = task_run_id(team, task['task_id'], generation)
        transaction = 'retired-run-' + hashlib.sha256(run.encode()).hexdigest()
        if (process, transaction) in _published_retired_runs:
            continue
        kind = previous['kind']
        title = '原执行已结束' if kind == 'quiet' else '原执行已中断，正在恢复'
        endings.append({'room_id': task['room_id'], 'transaction': transaction,
            'content': {'msgtype': 'm.notice', 'body': title, 'org.agentteams.progress_only': True,
                'm.mentions': {'user_ids': []}, 'org.agentteams.run': {'version': '1', 'run_id': run,
                    'blocks': [{'type': 'error', 'kind': kind, 'title': title}]}}})
    return endings


def retired_run_delivered(process, transaction, event_id):
    """Call only after the native Matrix client acknowledges the send."""
    if not isinstance(event_id, str) or not event_id.startswith('$'):
        raise ValueError('Matrix has not acknowledged the retired run ending')
    _published_retired_runs.add((process, transaction))
