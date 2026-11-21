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


def native_reply_plan(checkpoint, request, bind):
    from worker_matrix_delivery import confirm_bound, plan_saved
    mode, pending = plan_saved(checkpoint, request['text'], request['sender'], request['device'])
    if pending is not None:
        committed = bind(pending)
        confirm_bound(committed["execution"], request["identity"], request["runtime"], pending)
    return {'mode': mode, 'reply_txn': checkpoint['reply_txn'], 'legacy': not checkpoint.get('reply_attempt')}


def execute(request):
    if request.get('action') == 'tool_policy':
        from task_operation import tool_policy
        return tool_policy(request.get('registration'))
    if not enabled():
        raise RecoveryConflict('Worker process recovery is not configured')
    recovery = Recovery(device_delivery=request.get("device_delivery") is True)
    action = request['action']
    if not isinstance(action, str):
        raise ValueError('Recovery action must be a string')
    if action == 'recovery.scanned':
        from worker_handoff import recovery_scanned
        recovery_scanned()
        return True
    if action.startswith('session.'):
        from worker_sessions import WorkerSessions
        sessions = WorkerSessions(request['runtime'], recovery=recovery)
        if action == 'session.load':
            return sessions.load(request['work'])
        if action == 'session.usable':
            return sessions.usable(request['snapshot'], request.get('native_ref'), request.get('native_digest'))
        if action == 'session.remember':
            return sessions.remember(request['work'], request['snapshot'], request['identity'],
                                     request['native_ref'], request['native_digest'], request['expires_at'])
        raise ValueError('Unsupported session action')
    if action.startswith('matrix.'):
        from worker_matrix_state import WorkerMatrix
        matrix = WorkerMatrix(request['runtime'], recovery=recovery)
        if action == 'matrix.load':
            return matrix.load()
        if action == 'matrix.start':
            return matrix.start()
        if action == 'matrix.ingest':
            return matrix.ingest(request['snapshot'], request.get('consumer'), request['inputs'], request.get('rooms'))
        if action == 'matrix.accept':
            return matrix.accept(request['room_id'], request['event_id'], request['source'],
                                 request['intent'], task_id=request.get('task_id'))
        if action == 'matrix.consumer':
            return matrix.consumer(request['snapshot'], account=request['account'],
                                   device=request.get('device', ''), format=request['format'])
        if action == 'matrix.prepare':
            from worker_matrix_state import matrix_event_key
            from task_coordination import IgnoredNotification, notification
            room = request['room_id']
            result = []
            for item in request['events']:
                event_id, source = item['event_id'], item['input']
                intent, task_id = item.get('intent'), None
                previous = matrix.input(matrix_event_key(room, event_id))
                if previous:
                    intent = previous['intent']
                elif source['content'].get('org.agentteams.coordination') is not None:
                    try:
                        packet = notification(source['content'], room, source['sender'], request['account'])
                    except IgnoredNotification:
                        continue
                    if not packet:
                        continue
                    meta = packet['meta']['teamharness_coordination']
                    if meta['kind'] == 'task_assigned':
                        intent, task_id = 'task_notification', meta['task_id']
                    else:
                        intent = 'message'
                if intent:
                    result.append(matrix.prepare_input(room, event_id, source, intent, task_id=task_id))
            return result
        if action == 'matrix.sync':
            return matrix.sync(request['snapshot'], request['consumer'], request['next_batch'], request['timelines'])
        if action == 'matrix.pending':
            return matrix.pending(request['snapshot'])
        if action == 'matrix.input':
            return matrix.input(request['event_key'])
        if action == 'matrix.backfill':
            return matrix.backfill(request['snapshot'], request['page'])
        if action == 'matrix.native.get':
            return matrix.native_input(request['event_key'])
        if action == 'matrix.native.commit':
            return matrix.native_committed(request['event_key'], request['snapshot'], request['native_ref'], request['native_digest'])
        if action == 'matrix.native.current':
            return matrix.native_current(request['snapshot'], request.get('native_ref'), request.get('native_digest'))
        if action == 'matrix.message.get':
            return matrix.message(request['event_key'])
        if action == 'matrix.message.claim':
            return matrix.message_claim(request['event_key'])
        if action == 'matrix.message.current':
            return matrix.message_current(request['identity'])
        if action == 'matrix.message.owned':
            return matrix._message_owned(request['identity'])
        if action == 'matrix.message.begin_tools':
            return matrix.message_begin_tools(request['identity'], request['calls'])
        if action == 'matrix.message.finish_tools':
            return matrix.message_finish_tools(request['identity'], request['operation_id'], request['results'])
        if action == 'matrix.message.plan_reply':
            identity = request['identity']
            entry = matrix._message_owned(identity)
            cp = entry['execution'].get('checkpoint') or {}
            return native_reply_plan(cp, request, lambda pending: matrix.message_checkpoint(identity, pending))
        if action == 'matrix.message.checkpoint':
            return matrix.message_checkpoint(request['identity'], request['checkpoint'], release=request.get('release', False))
        if action == 'matrix.message.save_reply':
            return matrix.message_save_reply(request['identity'], request['text'])
        if action == 'matrix.message.delivered':
            return matrix.message_delivered(request['identity'], request['event_id'])
        if action == 'matrix.message.cancel':
            return matrix.message_cancel(request['identity'])
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
    if action == 'begin_tools':
        return recovery.begin_tools(identity, request['calls'])
    if action == 'finish_tools':
        return recovery.finish_tools(identity, request['operation_id'], request['results'])
    if action == 'current':
        return {'task': task, 'checkpoint_digest': Recovery.digest(task['execution'].get('checkpoint'))}
    if action == 'plan_reply':
        cp = task['execution'].get('checkpoint') or {}
        return native_reply_plan(cp, request, lambda pending: recovery.checkpoint(identity['task_id'], identity['team_id'], pending))
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
