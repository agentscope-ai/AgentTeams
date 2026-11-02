"""Exercise MCP setup initialization against the shared Console session helper."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPTS = ROOT / "manager/agent/skills/mcp-server-management/scripts"
GATEWAY = ROOT / "manager/scripts/lib/gateway-api.sh"


class ConsoleSessionTest(unittest.TestCase):
    def initialize(self, script, overrides=None, cookie=False, login_fails=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "bin").mkdir()
            bootstrap = root / "bootstrap.sh"
            bootstrap.write_text('log() { printf "%s\\n" "$*" >&2; }\n')
            yaml = root / "tool.yaml"
            yaml.write_text('server:\n  accessToken: ""\n')
            calls = root / "calls.jsonl"
            curl = root / "bin/curl"
            curl.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
with open(os.environ['TEST_CALLS'], 'a') as out:
    out.write(json.dumps(args) + '\\n')
if os.environ.get('TEST_LOGIN_FAILS') == '1':
    sys.exit(22)
if '-c' in args:
    pathlib.Path(args[args.index('-c') + 1]).write_text('# test session\\n')
''')
            curl.chmod(0o755)
            env = {
                "PATH": str(root / "bin") + os.pathsep + os.environ["PATH"],
                "AGENTTEAMS_RUNTIME": "k8s",
                "AGENTTEAMS_ADMIN_USER": "test-admin",
                "AGENTTEAMS_ADMIN_PASSWORD": "test-password",
                "TEST_CALLS": str(calls),
                "TEST_LOGIN_FAILS": "1" if login_fails else "0",
            }
            if cookie:
                saved = root / "saved-cookie"
                saved.write_text('# existing test session\n')
                env["HIGRESS_COOKIE_FILE"] = str(saved)
            env.update(overrides or {})
            source = (SCRIPTS / script).read_text()
            source = source[:source.index('# Helper functions')]
            source = source.replace('/opt/agentteams/scripts/lib/agentteams-env.sh', str(bootstrap))
            source = source.replace('/opt/agentteams/scripts/lib/gateway-api.sh', str(root / 'gateway-api.sh'))
            (root / 'gateway-api.sh').write_text(
                GATEWAY.read_text().replace('/tmp/higress-session-cookie-gateway', str(root / 'login-cookie'))
            )
            source += '\nprintf "CONSOLE=%s\\n" "$CONSOLE_URL"\n'
            args = ['tool', 'http://tool.example.local:8080/mcp', 'http'] if 'proxy' in script else [
                'tool', 'test-key', '--yaml-file', str(yaml)
            ]
            result = subprocess.run(['bash', '-c', source, script, *args], env=env,
                                    capture_output=True, text=True)
            requests = [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []
            return result, requests

    def test_custom_console_logs_in_without_existing_cookie(self):
        for script in ('setup-mcp-server.sh', 'setup-mcp-proxy.sh'):
            with self.subTest(script=script):
                result, calls = self.initialize(script, {'HIGRESS_CONSOLE_URL': 'http://console.remote:8001'})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('CONSOLE=http://console.remote:8001', result.stdout)
                self.assertEqual(len(calls), 1)
                self.assertIn('http://console.remote:8001/session/login', calls[0])
                body = json.loads(calls[0][calls[0].index('-d') + 1])
                self.assertEqual(body, {'username': 'test-admin', 'password': 'test-password'})

    def test_alias_and_existing_cookie_are_reused(self):
        for script in ('setup-mcp-server.sh', 'setup-mcp-proxy.sh'):
            with self.subTest(script=script):
                result, calls = self.initialize(script, {'AGENTTEAMS_HIGRESS_CONSOLE_URL': 'http://alias.remote:8001'}, cookie=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('CONSOLE=http://alias.remote:8001', result.stdout)
                self.assertEqual(calls, [])

    def test_primary_console_setting_takes_precedence(self):
        result, _ = self.initialize('setup-mcp-server.sh', {
            'HIGRESS_CONSOLE_URL': 'http://primary.remote:8001',
            'AGENTTEAMS_HIGRESS_CONSOLE_URL': 'http://alias.remote:8001',
        }, cookie=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('CONSOLE=http://primary.remote:8001', result.stdout)

    def test_local_default_is_preserved(self):
        for script in ('setup-mcp-server.sh', 'setup-mcp-proxy.sh'):
            with self.subTest(script=script):
                result, calls = self.initialize(script, cookie=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('CONSOLE=http://127.0.0.1:8001', result.stdout)
                self.assertEqual(calls, [])

    def test_failed_login_stops_setup(self):
        for script in ('setup-mcp-server.sh', 'setup-mcp-proxy.sh'):
            with self.subTest(script=script):
                result, _ = self.initialize(script, login_fails=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('Failed to login', result.stderr)
                self.assertNotIn('CONSOLE=', result.stdout)
                self.assertNotIn('test-password', result.stdout + result.stderr)

    def test_cloud_mode_remains_unsupported(self):
        for script in ('setup-mcp-server.sh', 'setup-mcp-proxy.sh'):
            with self.subTest(script=script):
                result, calls = self.initialize(script, {'AGENTTEAMS_RUNTIME': 'aliyun'})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('not yet supported', result.stderr)
                self.assertEqual(calls, [])


class WorkerAssignmentTest(unittest.TestCase):
    def apply(self, fail=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            agt = root / 'agt'
            manifest = root / 'manifest.json'
            agt.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
if sys.argv[1:5] == ['get', 'workers', 'worker-a', '-o']:
    print(json.dumps({'name':'worker-a', 'env':{'private':'must-not-write'}, 'mcpServers':[
        {'name':'external', 'url':'https://external.example/mcp', 'transport':'sse'},
        {'name':'mcp-tool', 'url':'https://old.example/mcp'}
    ]}))
elif sys.argv[1:3] == ['apply', '-f']:
    if os.environ.get('TEST_APPLY_FAIL') == '1': sys.exit(1)
    pathlib.Path(os.environ['TEST_MANIFEST']).write_text(pathlib.Path(sys.argv[3]).read_text())
else:
    sys.exit(2)
''')
            agt.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                       TEST_MANIFEST=str(manifest), TEST_APPLY_FAIL='1' if fail else '0')
            result = subprocess.run(['bash', '-c', '''set -euo pipefail
source "$1"
gateway_configure_worker_mcp worker-a mcp-tool http://gateway.example:8080/mcp-servers/mcp-tool/mcp
''', 'test', str(GATEWAY)], env=env, capture_output=True, text=True)
            return result, json.loads(manifest.read_text()) if manifest.exists() else None

    def test_assignment_preserves_other_servers_and_only_updates_mcp_spec(self):
        result, manifest = self.apply()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(manifest, {
            'apiVersion': 'agentteams.io/v1beta1', 'kind': 'Worker',
            'metadata': {'name': 'worker-a'},
            'spec': {'mcpServers': [
                {'name': 'external', 'url': 'https://external.example/mcp', 'transport': 'sse'},
                {'name': 'mcp-tool', 'url': 'http://gateway.example:8080/mcp-servers/mcp-tool/mcp', 'transport': 'http'},
            ]},
        })

    def test_failed_assignment_is_not_reported_as_success(self):
        result, manifest = self.apply(fail=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(manifest)


if __name__ == '__main__':
    unittest.main()
