"""Case26's Human bootstrap must use the registered Team administrator."""
import json
import copy
import os
from pathlib import Path
import subprocess


def test_case26_registers_the_bootstrap_sender_as_team_admin():
    source = (Path(__file__).parent / 'test-26-qwenpaw-teamharness-plugin-mode.sh').read_text()
    start = source.index('_create_team_cr() {')
    function = source[start:source.index('\n_patch_worker_package_ref()', start)]
    script = '''_controller_labels_json() { echo '{}'; }
_k8s_create() { printf '%s' "$2"; }
''' + function + '\n_create_team_cr\n'
    env = {**os.environ, 'TEST_TEAM': 'fixture-team', 'TEST_LEADER': 'fixture-lead',
           'TEST_WORKER': 'fixture-worker', 'K8S_NAMESPACE': 'default',
           'TEST_ADMIN_USER': 'fixture-admin', 'TEST_MATRIX_DOMAIN': 'matrix.local:18080'}
    result = subprocess.run(['bash'], input=script, env=env, text=True, capture_output=True, check=True)
    team = json.loads(result.stdout)
    assert team['spec']['admin'] == {'name': 'fixture-admin', 'matrixUserId': '@fixture-admin:matrix.local:18080'}
    assert team['spec']['workerMembers'] == [
        {'name': 'fixture-lead', 'role': 'team_leader'}, {'name': 'fixture-worker', 'role': 'worker'}]


def test_case26_accepts_verified_multi_step_delivery_without_weakening_file_checks():
    source = (Path(__file__).parent / 'test-26-qwenpaw-teamharness-plugin-mode.sh').read_text()
    start = source.index('_task_check_succeeded() {')
    function = source[start:source.index('\n_delivered_result()', start)]
    artifact = {'artifactId': 'file', 'name': 'readiness.txt', 'version': 'v1'}
    report = {'artifactId': 'report', 'name': 'verification-report.md', 'version': 'v2'}
    def result(identity, output):
        return {'assignedTo': 'worker', 'status': 'completed', 'resultStatus': 'SUCCESS',
                'submissionId': identity, 'candidate': {'id': identity, 'artifacts': [output]},
                'handoff': {'submissionId': identity, 'actorName': 'worker', 'actorRole': 'worker'}}
    task = {'id': 'task', 'team': 'team', 'kind': 'main', 'status': 'completed',
            'assignedTo': '@leader:local', 'completionId': 'completion',
            'acceptedResults': [result('submission-1', artifact), result('submission-2', report)],
            'artifacts': copy.deepcopy([report, artifact])}
    env = {**os.environ, 'TASK_ID': 'task', 'TEST_TEAM': 'team', 'TEST_WORKER': 'worker',
           'LEADER_MXID': '@leader:local', 'DELIVERABLE_NAME': 'readiness.txt'}
    def accepted(value):
        return subprocess.run(['bash', '-c', function + '\n_task_check_succeeded'],
                              input=json.dumps({'task': value}), env=env,
                              text=True, capture_output=True).returncode == 0
    assert accepted(task)
    single = copy.deepcopy(task)
    single['acceptedResults'] = single['acceptedResults'][:1]
    single['artifacts'] = [artifact]
    assert accepted(single)
    for mutate in (
        lambda t: t.update(artifacts=[report]),
        lambda t: t['artifacts'][1].update(version='stale'),
        lambda t: t['acceptedResults'][0]['handoff'].update(submissionId='other'),
        lambda t: t['acceptedResults'][0].update(resultStatus='BLOCKED'),
        lambda t: t.update(status='in_progress'),
    ):
        invalid = copy.deepcopy(task)
        mutate(invalid)
        assert not accepted(invalid)


def test_case26_waits_for_callable_tools_at_top_level(tmp_path):
    source = (Path(__file__).parent / 'test-26-qwenpaw-teamharness-plugin-mode.sh').read_text()
    marker = source.index('callable_ready=false')
    start = source.rfind('\n', 0, marker) + 1
    block = source[start:source.index('    PACKAGE_MCP=', start)]
    script = '''
date() { n=$(cat "$FIXTURE_CLOCK"); echo $((n + 1)) > "$FIXTURE_CLOCK"; echo "$n"; }
sleep() { :; }
log_pass() { echo PASS; }
log_fail() { echo FAIL; }
_agent_api() {
    if [ "$3" = /api/mcp/teamharness ]; then
        echo '{"enabled":true,"transport":"stdio","command":"python3","args":["server.py"]}'
    else
        n=$(cat "$FIXTURE_CALLS"); n=$((n + 1)); echo "$n" > "$FIXTURE_CALLS"
        if [ "$n" -ge "$FIXTURE_READY_AFTER" ]; then
            echo '[{"name":"health"},{"name":"filesync"},{"name":"task"}]'
        else
            echo '[]'
        fi
    fi
}
container=fixture
''' + block
    for ready_after, expected in ((2, 'PASS'), (999, 'FAIL')):
        clock, calls = tmp_path / 'clock', tmp_path / 'calls'
        clock.write_text('0'); calls.write_text('0')
        env = {**os.environ, 'FIXTURE_CLOCK': str(clock), 'FIXTURE_CALLS': str(calls),
               'FIXTURE_READY_AFTER': str(ready_after)}
        result = subprocess.run(['bash'], input=script, env=env, text=True,
                                capture_output=True, check=True, timeout=10)
        assert result.stdout.strip() == expected, result.stderr
        assert 'can only be used in a function' not in result.stderr
        assert int(calls.read_text()) >= 2
