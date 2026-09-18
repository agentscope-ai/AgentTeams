"""Check the real CI matrix and runner scheduling without Docker or an LLM."""
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = (ROOT / '.github/workflows/test-integration.yml').read_text()
RUNNER = (ROOT / 'tests/run-all-tests.sh').read_text()


class IntegrationSchedulingTest(unittest.TestCase):
    def matrix(self, untrusted=False):
        step = WORKFLOW.split('      - name: Select integration test matrix\n', 1)[1]
        script = textwrap.dedent(step.split('        run: |\n', 1)[1].split('\n  deepseek-harness-tests:', 1)[0])
        with tempfile.NamedTemporaryFile() as output:
            subprocess.run(['bash', '-eu', '-c', script], check=True, env={
                **os.environ, 'GITHUB_OUTPUT': output.name,
                'UNTRUSTED_PR': str(untrusted).lower(),
            })
            return json.loads(Path(output.name).read_text().removeprefix('matrix='))['include']

    def test_runtime_coverage_and_shared_scenarios(self):
        filters = dict(re.findall(r'^  (SHARD_\w+): "([\d ]+)"$', WORKFLOW, re.M))
        matrix = self.matrix()
        controller = [entry for entry in matrix if entry['shard'] == 'controller-cr']
        selected = {entry['worker_runtime']: filters[entry['filter_env']].split() for entry in controller}
        self.assertEqual(set(selected), {'openclaw', 'qwenpaw', 'hermes'})
        for tests in selected.values():
            self.assertTrue({'15', '17', '18', '19', '20', '22', '24', '100'} <= set(tests))
        for number in ('23', '25'):
            self.assertEqual([rt for rt, tests in selected.items() if number in tests], ['qwenpaw'])
        for number in ('27', '28'):
            self.assertEqual([rt for rt, tests in selected.items() if number in tests], ['qwenpaw'])
        self.assertEqual(len(matrix), 7)
        self.assertEqual(sum(len(tests) for tests in selected.values()), 28)
        for tests in filters.values():
            for number in tests.split():
                self.assertEqual(len(list((ROOT / 'tests').glob(f'test-{number}-*.sh'))), 1)

    def test_copaw_only_remains_as_migration_source(self):
        matrix = self.matrix()
        self.assertFalse(any('copaw' in (entry['manager_runtime'], entry['worker_runtime']) for entry in matrix))
        self.assertFalse(any(entry['shard'] == 'controller-cr-2' for entry in matrix))
        self.assertTrue(any(entry['shard'] == 'qwenpaw-teamharness' for entry in matrix))
        # Real state migration still needs a built CoPaw worker image.
        targets = re.search(r'target: \[(.*?)\]', WORKFLOW).group(1).split(', ')
        self.assertIn('copaw-worker', targets)

    def test_fork_keeps_shared_coverage_without_secrets(self):
        matrix = self.matrix(untrusted=True)
        self.assertEqual(len(matrix), 3)
        self.assertTrue(all(not entry['requires_secret'] for entry in matrix))
        self.assertTrue(any(entry['worker_runtime'] == 'qwenpaw' for entry in matrix))

    def test_mcp_probe_uses_configured_environment(self):
        script = (ROOT / 'tests/test-26-qwenpaw-teamharness-plugin-mode.sh').read_text()
        start = script.index('env = dict(os.environ)')
        end = script.index('request = {', start)
        with patch.dict(os.environ, {'AGENTTEAMS_AGENT_ROLE': 'standalone', 'TOKEN': 'secret'}, clear=True):
            scope = {'os': os, 'client': {'env': {
                'AGENTTEAMS_AGENT_ROLE': 'te******er', 'TOKEN': '******',
                'TEAMHARNESS_RUNTIME_CONFIG': '******',
            }}, 'derived_env': {'TEAMHARNESS_RUNTIME_CONFIG': '/worker/runtime/runtime.yaml', 'AGENTTEAMS_AGENT_ROLE': 'team_leader'}}
            exec(script[start:end], scope)
        self.assertEqual(scope['env']['AGENTTEAMS_AGENT_ROLE'], 'team_leader')
        self.assertEqual(scope['env']['TOKEN'], 'secret')
        self.assertEqual(scope['env']['TEAMHARNESS_RUNTIME_CONFIG'], '/worker/runtime/runtime.yaml')

    def test_cleanup_waits_for_resource_deletion(self):
        cleanup = (ROOT / 'tests/test-100-cleanup.sh').read_text()
        start = cleanup.index('RECONCILE_TIMEOUT=120')
        end = cleanup.index('# Section 6:', start)
        for mode, expected in (('ready', 0), ('delayed', 10), ('stuck', 120), ('api-error', 120)):
            with self.subTest(mode=mode):
                result = subprocess.run(['bash', '-c', r'''
ELAPSED=0
FAILED=0
sleep() { ELAPSED=$((ELAPSED + $1)); }
log_info() { :; }
log_pass() { :; }
log_fail() { FAILED=1; }
list_test_worker_containers() { :; }
exec_in_agent() {
    [ "$MODE" != api-error ] || return 1
    if [ "$MODE" = stuck ] || { [ "$MODE" = delayed ] && [ "$ELAPSED" -lt 10 ]; }; then
        printf '{"%s":[{"name":"test-pending"}]}\n' "$3"
    else
        printf '{"%s":[]}\n' "$3"
    fi
}
''' + cleanup[start:end] + '\nprintf "RESULT %s %s\\n" "$ELAPSED" "$FAILED"'],
                    env={**os.environ, 'MODE': mode}, capture_output=True, text=True, check=True)
                self.assertIn(f'RESULT {expected} {int(expected == 120)}', result.stdout)

    def run_schedule(self, no_llm, test_filter=''):
        # Execute the runner's actual selection/dispatch/report block. Stub only
        # external scenarios and session polling, preserving exit-code handling.
        script = RUNNER[RUNNER.index('log "Running integration tests..."'):]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for number in ('02', '15', '17', '21', '23', '27', '100'):
                (root / f'test-{number}-stub.sh').write_text(f'echo run-{number} >> "$TRACE"\n')
            trace = root / 'trace'
            subprocess.run(['bash', '-e', '-c', '''
log() { :; }
error() { echo "$*" >&2; }
wait_for_session_stable() { echo wait >> "$TRACE"; }
''' + script], check=True, capture_output=True, text=True, env={
                **os.environ, 'SCRIPT_DIR': str(root), 'TRACE': str(trace),
                'AGENTTEAMS_CI_NO_LLM': str(no_llm), 'TEST_FILTER': test_filter,
            })
            return trace.read_text().splitlines()

    def test_cleanup_last_and_only_conversations_wait(self):
        self.assertEqual(self.run_schedule(0), [
            'wait', 'run-02', 'wait', 'run-15', 'run-17', 'wait',
            'run-21', 'run-23', 'run-27', 'run-100',
        ])

    def test_no_llm_has_no_session_waits(self):
        self.assertEqual(self.run_schedule(1), [
            'run-02', 'run-15', 'run-17', 'run-21', 'run-23', 'run-27', 'run-100',
        ])

    def test_explicit_filter_is_preserved(self):
        self.assertEqual(self.run_schedule(0, '100 23'), ['run-23', 'run-100'])
        self.assertEqual(self.run_schedule(0, '17'), ['run-17'])


if __name__ == '__main__':
    unittest.main()
