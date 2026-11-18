"""Native bridge must preserve shared ownership and delivery-only fences."""
import hashlib
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'shared/lib'))
import task_recovery_rpc as rpc


class RecoveryRPCTest(unittest.TestCase):
    def setUp(self):
        self.identity = {'task_id': 'T', 'team_id': 'team', 'generation': 2, 'instance_id': 'container/start'}
        self.task = {'task_id': 'T', 'status': 'in_progress', 'execution': {
            'generation': 2, 'instance_id': 'container/start', 'process_id': 'boot',
            'checkpoint': {'version': 4, 'completed_steps': ['one']}}}
        self.recovery = Mock(process='boot')
        self.recovery.current.return_value = self.task
        self.addCleanup(patch.stopall)
        patch.object(rpc, 'enabled', return_value=True).start()
        patch.object(rpc, 'Recovery', return_value=self.recovery,
                     digest=rpc.Recovery.digest, resume_text=rpc.Recovery.resume_text).start()

    def test_claim_preserves_authoritative_context_and_checkpoint_digest(self):
        self.recovery.claim.return_value = {'task': self.task, 'spec': 'Continue two and three',
                                            'execution': self.task['execution']}
        result = rpc.execute({'action': 'claim', 'snapshot': {'instance_id': 'container/start'},
                              'entry': {'team_id': 'team'}})
        self.assertIn('Continue two and three', result['resume_text'])
        self.assertIn('"completed_steps": ["one"]', result['resume_text'])
        self.assertIn('checkpoint_digest', result)

    def test_replaced_or_reassigned_execution_cannot_commit_a_reply(self):
        for field in ['generation', 'instance_id', 'process_id']:
            with self.subTest(field=field):
                previous = self.task['execution'][field]
                self.task['execution'][field] = 'successor'
                with self.assertRaises(rpc.RecoveryConflict):
                    rpc.execute({'action': 'delivered', 'identity': self.identity, 'event_id': '$event'})
                self.task['execution'][field] = previous
        self.recovery.delivered.assert_not_called()
        self.recovery.current.return_value = None
        with self.assertRaises(rpc.RecoveryConflict):
            rpc.execute({'action': 'current', 'identity': self.identity})

    def test_native_sync_verifies_receipts_and_preserves_accepted_input_classification(self):
        from task_coordination import IgnoredNotification, NotificationUnavailable
        matrix = Mock()
        matrix.input.side_effect = [None, {'intent': 'context'}, None, None]
        matrix.prepare_input.side_effect = lambda room, event, source, intent, task_id=None: (event, intent, task_id)
        patch('worker_matrix_state.WorkerMatrix', return_value=matrix).start()
        verify = patch('task_coordination.notification', side_effect=[
            {'meta': {'teamharness_coordination': {'kind':'task_assigned', 'task_id':'T'}}},
            IgnoredNotification(), NotificationUnavailable(),
        ]).start()
        event = {'event_id':'$one', 'input':{'sender':'@manager:d', 'content':{'org.agentteams.coordination':{}}}}
        request = dict(action='matrix.prepare', runtime='openhuman', room_id='!r:d', account='@worker:d',
                       events=[event, dict(event, event_id='$two'), dict(event, event_id='$three')])
        self.assertEqual(rpc.execute(request), [('$one','task_notification','T'), ('$two','context',None)])
        self.assertEqual(verify.call_count, 2)
        with self.assertRaises(NotificationUnavailable):
            rpc.execute(dict(request, events=[event]))

    def test_native_sender_must_bind_device_before_publish_and_verify_after_device_change(self):
        execution = self.task['execution']
        cp = dict(version=4, delivery='pending', reply_txn='txn',
                  result={'size': 2, 'sha256': hashlib.sha256(b'ok').hexdigest()}, reply_attempt={'state': 'ready'})
        execution['checkpoint'] = cp
        request = dict(action='plan_reply', runtime='openhuman', identity={**self.identity,'process_id':'boot'},
                       device_delivery=True, text='ok', sender='@worker:d', device='A')
        self.recovery.checkpoint.side_effect = lambda task,team,cp: {'execution':{
            **execution,'runtime':'openhuman','state':'active','checkpoint':{**cp,'saved_at':'server-clock'}}}
        self.assertEqual(rpc.execute(request), {'mode': 'send', 'reply_txn': 'txn', 'legacy': False})
        committed = self.recovery.checkpoint.call_args.args[2]
        self.assertEqual(committed['reply_attempt'], {'state': 'sending', 'user_id': '@worker:d', 'device_id': 'A'})
        execution['checkpoint'] = committed
        self.assertEqual(rpc.execute(dict(request, device='B'))['mode'], 'find')
        self.recovery.checkpoint.assert_called_once()
        with self.assertRaises(rpc.RecoveryConflict):
            rpc.execute(dict(request, text='different'))

    def test_empty_native_final_requires_a_submitted_durable_report(self):
        with self.assertRaisesRegex(rpc.RecoveryConflict, 'durable result'):
            rpc.execute({'action': 'prepare_reply', 'identity': self.identity, 'text': ''})
        self.recovery.prepare_reply.assert_not_called()
        self.task.update(status='submitted', result_path='shared/tasks/T/report.md')
        self.recovery.prepare_reply.return_value = (self.task, 'verified saved report')
        result = rpc.execute({'action': 'prepare_reply', 'identity': self.identity, 'text': ''})
        self.assertEqual(result['text'], 'verified saved report')

    def test_delivery_uses_shared_implementation_and_original_identity(self):
        rpc.execute({'action': 'delivered', 'identity': self.identity, 'event_id': '$event'})
        self.recovery.delivered.assert_called_once_with('T', 'team', '$event')

    def test_authority_unavailability_is_not_reclassified_as_replacement(self):
        self.recovery.current.side_effect = rpc.RecoveryUnavailable('offline')
        with self.assertRaises(rpc.RecoveryUnavailable):
            rpc.execute({'action': 'current', 'identity': self.identity})

    def test_manager_or_unconfigured_process_cannot_start_recovery(self):
        with patch.object(rpc, 'enabled', return_value=False):
            with self.assertRaises(rpc.RecoveryConflict):
                rpc.execute({'action': 'discover'})
        self.recovery.discover.assert_not_called()


if __name__ == '__main__':
    unittest.main()
