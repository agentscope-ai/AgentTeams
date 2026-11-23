"""The same operation contract fences both native Task and Matrix tools."""
import copy
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'shared/lib'))
from task_recovery import Recovery, RecoveryConflict
import task_operation as operation
import task_recovery_rpc as rpc


class OperationTest(unittest.TestCase):
    def setUp(self):
        self.identity = dict(task_id='T', team_id='team', generation=2, instance_id='instance', process_id='process')
        self.task = {'task_id': 'T', 'status': 'in_progress', 'execution': {
            **{key: self.identity[key] for key in ('generation', 'instance_id', 'process_id')}, 'state': 'active',
            'checkpoint': {'version': 3, 'completed_steps': ['earlier']}}}
        self.calls = [{'id': 'call-1', 'name': 'exec', 'arguments': {'command': 'private-command'}}]
        self.results = [{'id': 'call-1', 'content': [{'type': 'text', 'text': 'confirmed output'}]}]
        self.addCleanup(patch.stopall)
        patch.dict(os.environ, AGENTTEAMS_PROCESS_ID='process', AGENTTEAMS_STORAGE_PREFIX='storage').start()
        self.writes = []
        self.request = Mock(side_effect=self.transport)
        self.recovery = Recovery(request=self.request)

    def transport(self, method, path, body=None, etag=None, **kwargs):
        if method == 'GET':
            return {'instance_id': 'instance', 'tasks': [{'task': copy.deepcopy(self.task), 'team_id': 'team', 'etag': 'etag'}]}
        self.assertEqual(etag, 'etag')
        self.writes.append(copy.deepcopy(body['checkpoint']))
        self.task['execution']['checkpoint'] = copy.deepcopy(body['checkpoint'])
        self.task['execution']['state'] = 'needs_verification' if body['checkpoint'].get('operation', {}).get('state') in {'started', 'unknown'} else 'active'
        return copy.deepcopy(self.task)

    def test_native_start_is_durable_before_execution_and_contains_only_argument_digest(self):
        op = self.recovery.begin_tools(self.identity, self.calls)
        self.assertEqual(self.writes[0]['operation']['state'], 'started')
        self.assertEqual(self.writes[0]['operation']['id'], op)
        self.assertNotIn('private-command', self.writes[0]['operation']['evidence'])
        with self.assertRaisesRegex(RecoveryConflict, 'unconfirmed'):
            self.recovery.begin_tools(self.identity, self.calls)
        self.assertEqual(len(self.writes), 1)

    def test_confirmed_receipt_bytes_are_saved_before_verified_checkpoint(self):
        op = self.recovery.begin_tools(self.identity, self.calls)
        def upload(*args, **kwargs):
            self.assertEqual(len(self.writes), 1)
            self.assertIn(b'confirmed output', kwargs['input'])
        with patch('task_recovery.subprocess.run', side_effect=upload) as saved:
            self.recovery.finish_tools(self.identity, op, self.results)
        saved.assert_called_once()
        self.assertEqual(self.writes[-1]['operation']['state'], 'verified')
        self.assertEqual(self.writes[-1]['completed_steps'], ['earlier', op])
        with self.assertRaisesRegex(RecoveryConflict, 'already completed'):
            self.recovery.begin_tools(self.identity, self.calls)

    def test_unknown_or_mismatched_receipts_do_not_upload_or_clear_the_fence(self):
        op = self.recovery.begin_tools(self.identity, self.calls)
        for results in ([dict(self.results[0], effect='unknown')], [dict(self.results[0], effect='none')],
                        [], [dict(self.results[0], id='different')]):
            with self.subTest(results=results), patch('task_recovery.subprocess.run') as upload:
                with self.assertRaises(RecoveryConflict):
                    self.recovery.finish_tools(self.identity, op, results)
                upload.assert_not_called()
        self.assertEqual(self.task['execution']['checkpoint']['operation']['state'], 'started')

    def test_confirmed_native_error_receipt_remains_available_for_model_correction(self):
        op = self.recovery.begin_tools(self.identity, self.calls)
        with patch('task_recovery.subprocess.run'):
            self.recovery.finish_tools(self.identity, op, [dict(self.results[0], isError=True)])
        self.assertEqual(self.task['execution']['checkpoint']['operation']['state'], 'verified')

    def test_needs_verification_finishes_only_the_exact_owned_pending_operation(self):
        op = self.recovery.begin_tools(self.identity, self.calls)
        self.assertEqual(self.task['execution']['state'], 'needs_verification')
        with self.assertRaises(RecoveryConflict):
            self.recovery.tools_current(self.identity)
        with patch('task_recovery.subprocess.run') as upload:
            with self.assertRaises(RecoveryConflict):
                self.recovery.finish_tools(self.identity, 'different-operation', self.results)
            upload.assert_not_called()
            self.recovery.finish_tools(self.identity, op, self.results)
        self.assertEqual(self.recovery.tools_current(self.identity)['execution']['state'], 'active')

    def test_upload_failure_retains_started_operation(self):
        op = self.recovery.begin_tools(self.identity, self.calls)
        with patch('task_recovery.subprocess.run', side_effect=OSError('own fixture')):
            with self.assertRaises(OSError):
                self.recovery.finish_tools(self.identity, op, self.results)
        self.assertEqual(len(self.writes), 1)

    def test_checkpoint_fences_changed_ownership_and_concurrent_checkpoint(self):
        original = self.task['execution']['checkpoint']
        for changed in ('generation', 'instance_id', 'process_id'):
            identity = dict(self.identity, **{changed: 'replaced'})
            with self.subTest(changed=changed), self.assertRaises(RecoveryConflict):
                self.recovery.checkpoint('T', 'team', {'version': 4}, expected_identity=identity)
        self.task['execution']['checkpoint'] = {'version': 4}
        with self.assertRaisesRegex(RecoveryConflict, 'checkpoint changed'):
            self.recovery.checkpoint('T', 'team', {'version': 4}, expected_identity=self.identity,
                                     expected_digest=Recovery.digest(original))
        self.assertEqual(self.writes, [])

    def test_unknown_after_restart_requires_verification_not_replay(self):
        cp = {'version': 4, 'operation': {'id': 'old', 'state': 'unknown', 'evidence': ''}}
        self.task['execution']['checkpoint'] = cp
        with self.assertRaises(RecoveryConflict):
            self.recovery.begin_tools(self.identity, self.calls)
        self.assertEqual(self.writes, [])

    def test_native_rpc_dispatch_preserves_all_owned_identity_fields(self):
        with patch.object(rpc, 'enabled', return_value=True), patch.object(rpc, 'Recovery', return_value=self.recovery):
            op = rpc.execute(dict(action='begin_tools', identity=self.identity, calls=self.calls))
            with patch('task_recovery.subprocess.run'):
                rpc.execute(dict(action='finish_tools', identity=self.identity, operation_id=op, results=self.results))
        self.assertEqual(len(self.writes), 2)

    def test_duplicate_call_ids_and_malformed_batches_never_start(self):
        for calls in ([], self.calls * 2, [{'id': 'one', 'name': 'exec'}]):
            with self.subTest(calls=calls), self.assertRaises(RecoveryConflict):
                operation.begin({}, 'anchor', calls)


class NativePolicyTest(unittest.TestCase):
    def test_same_name_and_model_kind_cannot_delegate(self):
        for binding in (None, {'name': 'task'}, {'kind': 'task_protocol'},
                        {'origin': 'other.server', 'entrypoint': '/opt/agentteams/plugins/teamharness/mcp/common/task_native.py'},
                        {'origin': 'agentteams.common.task_native', 'entrypoint': '/tmp/other-task.py'}):
            with self.subTest(binding=binding):
                self.assertEqual(operation.tool_policy(binding)['mode'], 'journal')
        result = rpc.execute({'action': 'tool_policy', 'arguments': {'registration': {
            'origin': 'agentteams.common.task_native', 'entrypoint': '/opt/agentteams/plugins/teamharness/mcp/common/task_native.py'}}})
        self.assertEqual(result['mode'], 'journal')

    def test_fixed_native_registration_delegates_with_canonical_verification_on_unknown(self):
        registration = {'origin': 'agentteams.common.task_native',
                        'entrypoint': '/opt/agentteams/plugins/teamharness/mcp/common/task_native.py'}
        self.assertEqual(rpc.execute({'action': 'tool_policy', 'registration': registration}),
                         {'mode': 'delegate', 'unknown': 'verify_canonical_state'})


class OperationAdmissionTest(unittest.TestCase):
    setUp = OperationTest.setUp
    transport = OperationTest.transport

    def test_registered_pre_effect_error_requires_exact_code_and_call(self):
        calls = [dict(self.calls[0], pre_effect_error_codes=['NATIVE_PRECONDITION'])]
        op = self.recovery.begin_tools(self.identity, calls)
        result = dict(self.results[0], effect='none', status='error', error_code='NATIVE_PRECONDITION')
        with patch('task_recovery.subprocess.run') as upload:
            for invalid in (dict(result, id='different'), dict(result, error_code='unknown'),
                            dict(result, effect='unknown'), dict(result, status='success')):
                with self.assertRaises(RecoveryConflict):
                    self.recovery.finish_tools(self.identity, op, [invalid])
            upload.assert_not_called()
            self.recovery.finish_tools(self.identity, op, [result])
        self.assertEqual(self.recovery.tools_current(self.identity)['execution']['state'], 'active')

    def test_read_only_terminal_error_allows_followup_under_same_identity(self):
        calls = [dict(self.calls[0], read_only=True)]
        failed = [dict(self.results[0], effect='none', status='error')]
        op = self.recovery.begin_tools(self.identity, calls)
        with patch('task_recovery.subprocess.run'):
            self.recovery.finish_tools(self.identity, op, failed)
        self.assertEqual(self.recovery.tools_current(self.identity)['execution']['state'], 'active')
        self.recovery.begin_tools(self.identity, [dict(calls[0], id='followup')])

    def test_read_only_claim_inside_model_arguments_cannot_settle_error(self):
        calls = [dict(self.calls[0], arguments={'read_only': True})]
        op = self.recovery.begin_tools(self.identity, calls)
        with self.assertRaises(RecoveryConflict):
            self.recovery.finish_tools(self.identity, op,
                                       [dict(self.results[0], effect='none', status='error')])
    def test_generic_and_canonical_calls_share_the_same_post_restart_admission(self):
        with patch.object(rpc, 'enabled', return_value=True), patch.object(rpc, 'Recovery', return_value=self.recovery):
            self.assertEqual(rpc.execute(dict(action='admit_tools', identity=self.identity))['task']['task_id'], 'T')
            self.recovery.begin_tools(self.identity, self.calls)
            with self.assertRaisesRegex(RecoveryConflict, 'verification'):
                rpc.execute(dict(action='admit_tools', identity=self.identity))

    def test_delivery_only_and_unknown_never_enter_normal_inference(self):
        for checkpoint in ({'delivery': 'pending'}, {'operation': {'state': 'unknown'}},
                           {'operation': {'state': 'started'}}, {'operation': {'state': 'unexpected'}}):
            with self.subTest(checkpoint=checkpoint), self.assertRaises(RecoveryConflict):
                operation.admit(checkpoint)
        self.assertTrue(operation.admit({'operation': {'state': 'verified'}}))
