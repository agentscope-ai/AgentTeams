"""Case26's Human bootstrap must use the registered Team administrator."""
import json
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
