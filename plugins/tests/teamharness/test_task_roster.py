"""Registered identities are public data; no Harness or model heuristics."""
import importlib.util
from pathlib import Path

import pytest

SOURCE = Path(__file__).resolve().parents[3] / 'plugins/teamharness/mcp/common/task_roster.py'


def load():
    spec = importlib.util.spec_from_file_location('task_roster_fixture', SOURCE)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_manager_reads_registered_teams_and_workers_once_and_projects_no_secrets():
    calls = []
    def get(route):
        calls.append(route)
        return {'teams': [{'name': 'alpha', 'leaderName': 'lead', 'description': 'delivery',
                           'phase': 'Active', 'admin': {'secret': 'private'}}]} if route == 'teams' else {
            'workers': [{'name': 'lead', 'matrixUserID': '@lead:test', 'secret': 'private'},
                        {'name': 'other', 'matrixUserId': '@other:test'}]}
    result = load().roster('manager', get)
    assert calls == ['teams', 'workers']
    assert result == {'role': 'manager', 'teams': [
        {'team': 'alpha', 'description': 'delivery', 'leader': 'lead', 'matrix_id': '@lead:test', 'phase': 'Active'}]}
    assert 'private' not in str(result)
    assert 'other' not in str(result)


def test_leader_reads_only_the_bound_team_and_uses_public_member_identities():
    calls = []
    def get(route):
        calls.append(route)
        return {'name': 'alpha', 'members': [
            {'name': 'lead', 'role': 'team_leader', 'matrixUserID': '@lead:test', 'phase': 'Running', 'config': 'private'},
            {'name': 'dev', 'role': 'worker', 'matrixUserId': '@dev:test', 'phase': 'Running', 'state': 'idle'},
            {'name': 'missing', 'role': 'worker', 'phase': 'Missing'}]}
    result = load().roster('team_leader', get, team='alpha')
    assert calls == ['teams/alpha']
    assert result['role'] == 'leader'
    assert result['team'] == 'alpha'
    assert result['members'][1] == {'name': 'dev', 'role': 'worker', 'matrix_id': '@dev:test', 'phase': 'Running', 'state': 'idle'}
    assert result['members'][2] == {'name': 'missing', 'role': 'worker', 'matrix_id': None, 'phase': 'Missing'}
    assert 'private' not in str(result)


@pytest.mark.parametrize('role,team', [('worker', 'alpha'), ('manager', 'alpha'), ('leader', None),
                                      ('leader', '../other'), ('unknown', None)])
def test_invalid_role_or_missing_trusted_scope_does_not_query(role, team):
    with pytest.raises(ValueError):
        load().roster(role, lambda _: pytest.fail('unscoped roster lookup'), team=team)


@pytest.mark.parametrize('payload', [{}, {'teams': None}, {'teams': 'unavailable'}, {'teams': [{}]}])
def test_malformed_manager_api_response_is_not_silently_an_empty_roster(payload):
    with pytest.raises(ValueError):
        load().roster('manager', lambda route: payload if route == 'teams' else {'workers': []})


def test_api_failure_is_not_hidden_as_no_teams():
    def unavailable(_):
        raise OSError('own fixture unavailable')
    with pytest.raises(OSError):
        load().roster('manager', unavailable)


def test_a_different_team_cannot_supply_leader_members():
    with pytest.raises(ValueError):
        load().roster('leader', lambda _: {'name': 'other', 'members': []}, team='alpha')


def test_missing_member_projection_is_not_replaced_with_worker_names():
    with pytest.raises(ValueError):
        load().roster('leader', lambda _: {'name': 'alpha', 'workerMembers': [{'name': 'guessed'}]}, team='alpha')
    with pytest.raises(ValueError):
        load().roster('leader', lambda _: {'name': 'alpha'}, team='alpha')
    assert load().roster('leader', lambda _: {'name': 'alpha', 'workerMembers': None}, team='alpha')['members'] == []


def test_empty_registered_state_and_unready_teams_remain_explicit():
    result = load().roster('manager', lambda route: {'teams': [
        {'name': 'pending', 'leaderName': 'new-lead', 'phase': 'Pending'}]} if route == 'teams' else {'workers': []})
    assert result['teams'][0]['matrix_id'] is None
    assert result['teams'][0]['leader'] == 'new-lead'
    assert result['teams'][0]['phase'] == 'Pending'
    assert load().roster('manager', lambda route: {route: []})['teams'] == []


def test_conflicting_worker_identity_does_not_select_an_arbitrary_duplicate():
    with pytest.raises(ValueError):
        load().roster('manager', lambda route: {'teams': []} if route == 'teams' else {'workers': [
            {'name': 'dev', 'matrixUserID': '@dev:test'}, {'name': 'dev', 'matrixUserID': '@other:test'}]})
