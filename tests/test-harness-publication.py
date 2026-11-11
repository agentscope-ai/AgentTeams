#!/usr/bin/env python3
"""Exercise publishing orchestration without building or contacting a registry."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'scripts/release/build-harness-platform.sh'

class HarnessPublicationTest(unittest.TestCase):
    def exercise(self, runtime, fail_smoke=False):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            log = root / 'calls.jsonl'
            tool = '#!' + sys.executable + '''
import json, os, sys
with open(os.environ['CALL_LOG'], 'a') as out:
    out.write(json.dumps([os.path.basename(sys.argv[0]), *sys.argv[1:]]) + '\\n')
if os.path.basename(sys.argv[0]) == 'git': print('candidate-sha')
if os.environ.get('FAIL_SMOKE') == '1' and sys.argv[1:2] == ['run']: sys.exit(9)
'''
            for name in ['docker', 'make', 'git']:
                (root / name).write_text(tool)
                (root / name).chmod(0o755)
            env = {**os.environ, 'PATH': str(root) + ':' + os.environ['PATH'],
                   'CALL_LOG': str(log), 'REGISTRY': 'registry.test',
                   'RELEASE_VERSION': 'v1.2.7', 'FAIL_SMOKE': str(int(fail_smoke))}
            result = subprocess.run(['bash', str(SCRIPT), runtime, 'candidate-arm64',
                                     'linux/arm64', 'registry.test/controller@sha256:123'],
                                    env=env, capture_output=True, text=True)
            calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
            return result.returncode, calls

    def test_all_six_names_and_smoke_before_push(self):
        for runtime in ['openclaw', 'qwenpaw', 'hermes', 'deepseek', 'openhuman', 'codex']:
            with self.subTest(runtime=runtime):
                code, calls = self.exercise(runtime)
                self.assertEqual(code, 0)
                self.assertEqual(calls[-1], ['docker', 'push',
                    f'registry.test/agentteams/agentteams-harness-{runtime}:candidate-arm64'])
                self.assertTrue(any(c[:2] == ['docker', 'run'] for c in calls[:-1]))
                self.assertNotIn('agentteams-manager', json.dumps(calls))

    def test_failed_smoke_never_pushes(self):
        code, calls = self.exercise('qwenpaw', fail_smoke=True)
        self.assertNotEqual(code, 0)
        self.assertFalse(any(c[:2] == ['docker', 'push'] for c in calls))

    def test_unknown_runtime_never_builds_or_pushes(self):
        code, calls = self.exercise('unknown')
        self.assertNotEqual(code, 0)
        self.assertFalse(any(c[0] in ['make', 'docker'] for c in calls))

if __name__ == '__main__':
    unittest.main()
