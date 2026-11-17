"""JSON/stdin bridge for native Harnesses using the shared recovery contract."""
import json
import sys

from task_recovery import Recovery, RecoveryConflict, RecoveryUnavailable, enabled


def owned(recovery, identity):
    task = recovery.current(identity['task_id'], identity['team_id'])
    execution = task.get('execution', {}) if task else {}
    if (execution.get('process_id') != recovery.process
            or execution.get('generation') != identity['generation']
            or execution.get('instance_id') != identity['instance_id']):
        raise RecoveryConflict('Task was cancelled, reassigned or replaced')
    return task


def execute(request):
    if not enabled():
        raise RecoveryConflict('Worker process recovery is not configured')
    recovery = Recovery()
    action = request['action']
    if action.startswith('matrix.'):
        from worker_matrix_state import WorkerMatrix
        matrix = WorkerMatrix(request['runtime'], recovery=recovery)
        if action == 'matrix.load':
            return matrix.load()
        if action == 'matrix.start':
            return matrix.start()
        if action == 'matrix.ingest':
            return matrix.ingest(request['snapshot'], request.get('consumer'), request['inputs'], request.get('rooms'))
        if action == 'matrix.pending':
            return matrix.pending(request['snapshot'])
        raise ValueError('Unsupported Matrix state action')
    if action == 'discover':
        return recovery.discover()
    if action == 'claim':
        claimed = recovery.claim(request['snapshot'], request['entry'])
        task = {**claimed['task'], 'team_id': request['entry']['team_id'], 'spec': claimed['spec']}
        return {**claimed, 'resume_text': Recovery.resume_text(task),
                'checkpoint_digest': Recovery.digest(task['execution'].get('checkpoint'))}
    identity = request['identity']
    task = owned(recovery, identity)
    if action == 'current':
        return {'task': task, 'checkpoint_digest': Recovery.digest(task['execution'].get('checkpoint'))}
    if action == 'prepare_reply':
        text = request['text']
        cp = task['execution'].get('checkpoint') or {}
        if not text and (task['status'] != 'submitted' or not (task.get('result_path') or cp.get('result'))):
            raise RecoveryConflict('Native turn ended without a durable result')
        saved, text = recovery.prepare_reply(identity['task_id'], identity['team_id'], text)
        return {'saved': saved, 'text': text}
    if action == 'delivered':
        return recovery.delivered(identity['task_id'], identity['team_id'], request['event_id'])
    if action == 'interrupted':
        return recovery.interrupted(identity['task_id'], identity['team_id'])
    raise ValueError('Unsupported recovery action')


if __name__ == '__main__':
    try:
        print(json.dumps({'value': execute(json.load(sys.stdin))}))
    except RecoveryConflict as error:
        print(json.dumps({'error': 'conflict', 'message': str(error)}))
        sys.exit(3)
    except RecoveryUnavailable as error:
        print(json.dumps({'error': 'unavailable', 'message': str(error)}))
        sys.exit(4)
    except (KeyError, TypeError, ValueError):
        print(json.dumps({'error': 'invalid_request'}))
        sys.exit(2)
