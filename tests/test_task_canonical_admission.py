"""A native restart must consult canonical authority before normal inference."""
from pathlib import Path
import sys
from unittest.mock import Mock, patch

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'shared/lib'))
from task_recovery import Recovery, RecoveryConflict
import task_recovery_rpc as rpc


def test_active_execution_does_not_bypass_durable_canonical_unknown():
    identity = {'task_id': 'T', 'team_id': 'own', 'generation': 2, 'instance_id': 'instance', 'process_id': 'process'}
    task = {'task_id': 'T', 'task_plan_id': 'plan', 'status': 'in_progress', 'execution': {
        'generation': 2, 'instance_id': 'instance', 'process_id': 'process', 'state': 'active', 'checkpoint': {'version': 1}}}
    recovery = Mock(process='process')
    recovery.current.return_value = recovery.tools_current.return_value = task
    recovery.request.return_value = {'mode': 'verify', 'authority': {'task': {'status': 'in_progress'}}}
    with patch.object(rpc, 'enabled', return_value=True), patch.object(rpc, 'Recovery', return_value=recovery):
        with pytest.raises(RecoveryConflict, match='Canonical'):
            rpc.execute({'action': 'admit_tools', 'identity': identity})
    recovery.request.assert_called_once_with('POST', 'tasks/native-operations', {
        'mode': 'admit', 'arguments': {'action': 'get', 'taskId': 'T', 'team': 'own'}})


def test_managed_recovery_uses_only_public_protocol_while_legacy_guidance_is_retained():
    task = {'task_id': 'T', 'task_plan_id': 'plan', 'status': 'in_progress', 'execution': {'generation': 2}}
    managed = Recovery.resume_text(task)
    assert 'common task protocol' in managed
    assert 'taskflow.checkpoint_task' not in managed
    task.pop('task_plan_id')
    assert 'taskflow.checkpoint_task' in Recovery.resume_text(task)


def test_execution_admission_returns_same_common_receipt_projection():
    identity = {'task_id': 'T', 'team_id': 'own', 'generation': 2, 'instance_id': 'instance', 'process_id': 'process'}
    task = {'task_id': 'T', 'task_plan_id': 'plan', 'status': 'in_progress', 'execution': {
        'generation': 2, 'instance_id': 'instance', 'process_id': 'process', 'state': 'active', 'checkpoint': {'version': 1}}}
    recovery = Mock(process='process')
    recovery.current.return_value = recovery.tools_current.return_value = task
    recovery.request.return_value = {'mode': 'admitted', 'action': 'submit', 'result': {'task': {'id': 'T'}}}
    expected = [{'receiptId': 'own-receipt', 'fact': 'Own verified shared fact'}]
    from subprocess import CompletedProcess
    with patch.object(rpc, 'enabled', return_value=True), patch.object(rpc, 'Recovery', return_value=recovery), \
            patch.object(rpc.subprocess, 'run', return_value=CompletedProcess([], 0, '{"receipts":[{"receiptId":"own-receipt","fact":"Own verified shared fact"}]}', '')) as project:
        result = rpc.execute({'action': 'admit_tools', 'identity': identity})
    assert result['receipts'] == expected
    assert 'canonical_receipts' in project.call_args.kwargs['input']
