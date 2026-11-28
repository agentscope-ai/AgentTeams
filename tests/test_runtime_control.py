"""Task steering authority belongs to Core, independent of the native SDK."""
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'shared/lib'))
from runtime_control import task_supplement


def test_agent_approval_mode_is_shared_and_optional(monkeypatch):
    import pytest
    from runtime_control import approval_mode, mcp_approval
    monkeypatch.delenv('TEAMHARNESS_RUNTIME_CONFIG', raising=False)
    assert approval_mode({}) is None
    for mode in ('ask', 'allow'):
        config = {'desired': {'approvalMode': mode, 'mcpServers': [{'name': 'docs'}]}}
        assert approval_mode(config) == mode
        assert mcp_approval(config, 'docs') == mode
        config['desired']['mcpServers'][0]['approvals'] = {'tools': '*', 'mode': 'ask'}
        assert mcp_approval(config, 'docs') == 'ask'
    with pytest.raises(ValueError, match='approvalMode'):
        approval_mode({'desired': {'approvalMode': 'typo'}})


def test_matrix_mention_survives_transport_for_task_supplements():
    sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'deepseek-harness/scripts'))
    from matrix_channel import matrix_events
    config, task, _ = fixture()
    content = {'msgtype': 'm.text', 'body': '@dev include the third-click check',
               'm.mentions': {'user_ids': ['@dev:test']}}
    payload = {'rooms': {'join': {'!task:test': {'timeline': {'events': [
        {'type': 'm.room.message', 'event_id': '$supplement', 'sender': '@lead:test', 'content': content}
    ]}}}}}
    decoded = matrix_events(payload, {'!task:test': 'team'}, '@dev:test', {'@lead:test'})
    assert len(decoded) == 1
    assert task_supplement(config, task, decoded[0])


def fixture():
    config = {'member': {'matrixUserId': '@dev:test'}, 'team': {'name': 'alpha', 'leaderName': 'lead',
        'members': [{'name': 'lead', 'role': 'team_leader', 'matrixUserId': '@lead:test'}]}}
    task = {'task_id': 'child', 'team_id': 'alpha', 'room_id': '!task:test'}
    source = {'event_id': '$input', 'room_id': '!task:test', 'sender': '@lead:test',
              'body': '@dev:test include the third-click check'}
    return config, task, source


def test_registered_task_owner_can_supplement_an_active_assignment():
    config, task, source = fixture()
    assert task_supplement(config, task, source)
    assert task_supplement(config, task, dict(source, body='/steer ' + source['body']))


def test_room_team_assignment_and_actor_are_all_required():
    config, task, source = fixture()
    for changed in ({}, dict(task, room_id='!other:test'), dict(task, team_id='other'), dict(task, task_id='')):
        assert not task_supplement(config, changed, source)
    for changed in (dict(source, sender='@other:test'), dict(source, sender='@dev:test'), dict(source, event_id='')):
        assert not task_supplement(config, task, changed)
    config['team']['leaderName'] = 'different'
    assert not task_supplement(config, task, source)


def test_supplements_do_not_grant_stop_approval_or_process_control():
    config, task, source = fixture()
    for text in ('', ' ', '/cancel', '/approve request', '/answer request', '/steer '):
        assert not task_supplement(config, task, dict(source, body=text))
    for key in ('answer', 'execution_control'):
        assert not task_supplement(config, task, dict(source, **{key: {'action': 'stop'}}))


def test_unaddressed_chatter_does_not_steer_another_worker():
    config, task, source = fixture()
    assert not task_supplement(config, task, dict(source, body='@tester:test check this'))
    assert not task_supplement(config, task, dict(source, body='@dev:test-other check this'))
    assert task_supplement(config, task, dict(source, body='supplement', mentions=['@dev:test']))


def test_task_admin_uses_the_same_directed_supplement_authority():
    config, task, source = fixture()
    config['team']['admin'] = {'matrixUserId': '@admin:test'}
    assert task_supplement(config, task, dict(source, sender='@admin:test'))
    assert not task_supplement(config, task, dict(source, sender='@admin:test', room_id='!other:test'))


def test_mcp_policy_reads_current_projection_and_rejects_member_change(monkeypatch, tmp_path):
    import pytest
    import yaml
    from runtime_control import mcp_approval
    config = {'member': {'runtimeName': 'dev', 'runtime': 'test'}, 'desired': {'mcpServers': [
        {'name': 'github', 'approvals': {'tools': '*', 'mode': 'ask'}}]}}
    path = tmp_path / 'runtime.yaml'
    path.write_text(yaml.safe_dump(config))
    monkeypatch.setenv('TEAMHARNESS_RUNTIME_CONFIG', str(path))
    assert mcp_approval(config, 'github') == 'ask'
    assert mcp_approval(config, 'other') is None
    current = yaml.safe_load(path.read_text())
    current['desired']['mcpServers'][0]['approvals']['mode'] = 'allow'
    path.write_text(yaml.safe_dump(current))
    assert mcp_approval(config, 'github') == 'allow'
    current['member']['runtimeName'] = 'other'
    path.write_text(yaml.safe_dump(current))
    with pytest.raises(ValueError, match='different runtime member'):
        mcp_approval(config, 'github')


def test_retire_supplement_requires_native_receipt_and_preserves_unknown_effects():
    import pytest
    from unittest.mock import Mock
    from task_recovery import RecoveryConflict
    from runtime_control import retire_native_supplement
    matrix = Mock()
    matrix.native_input.return_value = {'input': {'native_commit': {'native_ref': 'session', 'native_digest': 'digest'}}}
    matrix.native_current.return_value = False
    with pytest.raises(RecoveryConflict, match='persistence receipt'):
        retire_native_supplement(matrix, 'key')
    matrix.message_claim.assert_not_called()
    matrix.native_current.return_value = True
    matrix.message_claim.return_value = {'recovery': 'verify'}
    with pytest.raises(RecoveryConflict, match='unverified ordinary operation'):
        retire_native_supplement(matrix, 'key')
    matrix.message_cancel.assert_not_called()
    matrix.message_claim.return_value = {'recovery': 'resume', 'identity': {'event_key': 'key'}}
    retire_native_supplement(matrix, 'key')
    matrix.message_cancel.assert_called_once_with({'event_key': 'key'})


def test_task_room_selection_is_owned_unfinished_and_unambiguous():
    import pytest
    from runtime_control import task_room_assignment
    from task_recovery import RecoveryConflict
    config, _, _ = fixture()
    config['member']['runtimeName'] = 'dev'
    task = {'task_id': 'child', 'room_id': '!task:test', 'assigned_to': 'dev',
            'task_kind': 'execution', 'status': 'in_progress'}
    entry = {'team_id': 'alpha', 'task': task}
    assert task_room_assignment(config, {'tasks': [entry]}, '!task:test') == entry
    for change in ({'assigned_to': 'other'}, {'task_kind': 'main'}, {'status': 'completed'}, {'room_id': '!other:test'}):
        assert task_room_assignment(config, {'tasks': [{**entry, 'task': {**task, **change}}]}, '!task:test') is None
    assert task_room_assignment(config, {'tasks': [{**entry, 'team_id': 'other'}]}, '!task:test') is None
    with pytest.raises(RecoveryConflict, match='multiple unfinished assignments'):
        task_room_assignment(config, {'tasks': [entry, {**entry, 'task': {**task, 'task_id': 'other'}}]}, '!task:test')


def test_retired_runs_survive_a_lost_send_and_do_not_resolve_unknown_effects():
    import copy
    import pytest
    from runtime_control import retired_task_runs, retired_run_delivered
    task = {'task_id': 'retirement-fixture', 'room_id': '!task:test', 'status': 'in_progress',
            'execution': {'process_id': 'replacement-process', 'generation': 3, 'state': 'needs_verification',
                'checkpoint': {'operation': {'state': 'unknown'}},
                'retired_runs': [{'generation': 1, 'kind': 'quiet'}, {'generation': 2, 'kind': 'failed'}]}}
    original = copy.deepcopy(task)
    assert retired_task_runs('alpha', task, 'old-process') == []
    pending = retired_task_runs('alpha', task, 'replacement-process')
    assert len(pending) == 2
    assert pending[1]['content']['org.agentteams.run']['run_id'] == 'task:alpha:retirement-fixture:2'
    assert retired_task_runs('alpha', task, 'replacement-process') == pending
    with pytest.raises(ValueError, match='has not acknowledged'):
        retired_run_delivered('replacement-process', pending[0]['transaction'], None)
    assert retired_task_runs('alpha', task, 'replacement-process') == pending
    retired_run_delivered('replacement-process', pending[0]['transaction'], '$confirmed')
    assert retired_task_runs('alpha', task, 'replacement-process') == [pending[1]]
    assert task == original  # Display cleanup never mutates Task or tool evidence.


def test_supplement_claim_precedes_native_persistence_and_preserves_unknowns():
    import pytest
    from unittest.mock import Mock
    from task_recovery import RecoveryConflict
    from runtime_control import admit_native_supplement, retire_native_supplement
    matrix = Mock()
    persisted, claimed = False, False
    def claim(_):
        nonlocal claimed
        if persisted and not claimed:
            return {'recovery': 'verify'}
        claimed = True
        return {'recovery': 'resume', 'identity': {'event_key': 'key'}}
    matrix.message_claim.side_effect = claim
    assert admit_native_supplement(matrix, 'key') is True
    persisted = True
    matrix.native_input.return_value = {'input': {'native_commit': {'native_ref': 'session', 'native_digest': 'digest'}}}
    matrix.native_current.return_value = True
    retire_native_supplement(matrix, 'key')
    matrix.message_cancel.assert_called_once_with({'event_key': 'key'})
    matrix.message_claim.side_effect = None
    for mode in ('verify', 'deliver'):
        matrix.message_claim.return_value = {'recovery': mode}
        with pytest.raises(RecoveryConflict):
            admit_native_supplement(matrix, 'other')
    matrix.message_claim.return_value = {'recovery': 'done'}
    assert admit_native_supplement(matrix, 'key') is False
