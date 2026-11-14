import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'shared/lib'))
from runtime_observability import event, redact, write_record


class RuntimeLogTests(unittest.TestCase):
    def test_redacts_environment_and_json_credentials(self):
        with patch.dict(os.environ, {'MODEL_API_KEY': 'secret-12345'}):
            result = redact('secret-12345 Bearer abcdef {"access_token":"abc123", "password":"pass"}')
        for secret in ['secret-12345', 'abcdef', 'abc123', '"pass"']:
            self.assertNotIn(secret, result)

    def test_rotation_and_status(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {
            'AGENTTEAMS_LOG_DIR': tmp, 'AGENTTEAMS_LOG_MAX_BYTES': '300',
            'AGENTTEAMS_LOG_BACKUPS': '2', 'AGENTTEAMS_BOOT_ID': 'boot-1'}):
            for _ in range(20):
                write_record('runtime.log', 'x' * 100)
            self.assertTrue((Path(tmp) / 'runtime.log.2').exists())
            self.assertFalse((Path(tmp) / 'runtime.log.3').exists())
            event('runtime.ready', evidence='matrix_sync')
            state = json.loads((Path(tmp) / 'status.json').read_text())
            self.assertEqual(state['state'], 'ready')
            self.assertEqual(state['boot_id'], 'boot-1')
            self.assertEqual((Path(tmp) / 'runtime.log').stat().st_mode & 0o777, 0o600)

    def test_supervisor_preserves_exit_and_captures_stderr(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = subprocess.run([sys.executable, str(ROOT / 'shared/lib/runtime_supervisor.py'), '--runtime', 'hermes', '--',
                sys.executable, '-c', 'import sys; print("hello"); print("secret-12345", file=sys.stderr); sys.exit(7)'],
                env={**os.environ, 'AGENTTEAMS_LOG_DIR': tmp, 'MODEL_API_KEY': 'secret-12345',
                     'AGENTTEAMS_MANAGER_RUNTIME': 'hermes'}, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 7, result.stderr)
            output = (Path(tmp) / 'runtime.log').read_text()
            self.assertIn('hello', output)
            self.assertIn('[REDACTED]', output)
            self.assertNotIn('secret-12345', result.stdout)
            state = json.loads((Path(tmp) / 'status.json').read_text())
            self.assertEqual(state['state'], 'failed')
            self.assertEqual(state['exit_code'], 7)

    def test_json_errors_remain_parseable_and_keep_reasons(self):
        with patch.dict(os.environ, {'MODEL_API_KEY': 'secret-12345'}):
            raw = redact(json.dumps({'error': {'message': 'HTTP 401 api_key=secret-12345'},
                                     'gateway_key': 'other-secret', 'exit_code': 127}))
        record = json.loads(raw)
        self.assertIn('HTTP 401', record['error']['message'])
        self.assertEqual(record['exit_code'], 127)
        self.assertEqual(record['gateway_key'], '[REDACTED]')
        self.assertNotIn('secret-12345', raw)

    def test_ready_is_not_downgraded_by_late_or_stale_events(self):
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {'AGENTTEAMS_LOG_DIR': tmp, 'AGENTTEAMS_BOOT_ID': 'new'}):
            event('startup.begin')
            event('runtime.ready', evidence='matrix_sync')
            event('startup.stage', stage='late')
            with patch.dict(os.environ, {'AGENTTEAMS_BOOT_ID': 'old'}):
                event('runtime.exit', exit_code=7)
            state = json.loads((Path(tmp) / 'status.json').read_text())
            self.assertEqual(state['state'], 'ready')
            self.assertEqual(state['boot_id'], 'new')

    def test_spawn_failure_keeps_actual_reason(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = subprocess.run([sys.executable, str(ROOT / 'shared/lib/runtime_supervisor.py'),
                '--runtime', 'codex', '--', '/nonexistent/agentteams-runtime'],
                env={**os.environ, 'AGENTTEAMS_LOG_DIR': tmp}, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 127)
            state = json.loads((Path(tmp) / 'status.json').read_text())
            self.assertEqual(state['stage'], 'process.spawn')
            self.assertIn('No such file', state['error'])

    def test_supervisor_keeps_identity_for_every_role(self):
        for role in ['manager', 'leader', 'worker']:
            with self.subTest(role=role), tempfile.TemporaryDirectory() as tmp:
                result = subprocess.run([sys.executable, str(ROOT / 'shared/lib/runtime_supervisor.py'),
                    '--runtime', 'codex', '--', sys.executable, '-c', 'print("boot failed"); raise SystemExit(9)'],
                    env={**os.environ, 'AGENTTEAMS_AGENT_ROLE': role, 'AGENTTEAMS_WORKER_NAME': 'alice',
                         'AGENTTEAMS_MANAGER_NAME': 'default', 'AGENTTEAMS_LOG_DIR': tmp},
                    capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 9)
                status = json.loads((Path(tmp) / 'status.json').read_text())
                self.assertEqual(status['role'], role)
                self.assertEqual(status['agent_name'], 'default' if role == 'manager' else 'alice')
