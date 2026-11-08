"""Exercise the real pull script with isolated filesystem and object-store transport."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / 'manager/agent/worker-agent/skills/file-sync/scripts/agentteams-sync.sh'


class ConfigPullTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.lib = self.root / 'lib'
        self.lib.mkdir()
        # Only image paths are redirected; the shipped scripts run unchanged otherwise.
        for name in ('merge-openclaw-config.sh', 'migrate-openclaw-config.sh', 'worker-file-sync.sh'):
            source = (ROOT / 'shared/lib' / name).read_text()
            (self.lib / name).write_text(source.replace('/opt/agentteams/scripts/lib', str(self.lib)))
        (self.lib / 'agentteams-env.sh').write_text('set -e\nensure_mc_credentials() { :; }\n')
        self.workspace = self.root / 'fs/agents/test-worker'
        self.workspace.mkdir(parents=True)
        self.config = self.workspace / 'openclaw.json'
        self.local = {'channels': {'matrix': {'accessToken': 'current-token'}}, 'tools': {'local': True}}
        self.config.write_text(json.dumps(self.local))
        self.remote = self.root / 'remote'
        self.remote.mkdir()
        (self.remote / 'openclaw.json').write_text(json.dumps({
            'channels': {'matrix': {'accessToken': 'expired-token'}}, 'models': {'updated': True}}))
        (self.remote / 'SOUL.md').write_text('updated prompt')
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        mc = self.bin / 'mc'
        mc.write_text('''#!/usr/bin/env python3
import fnmatch, json, os, pathlib, shutil, sys
args = sys.argv[1:]
root = pathlib.Path(os.environ['TEST_ROOT'])
config = root / 'fs/agents/test-worker/openclaw.json'
if os.environ.get('FAIL_STAGE') == args[0]: sys.exit(7)
if args[0] == 'cp':
    shutil.copyfile(root / 'remote/openclaw.json', args[2])
elif args[0] == 'mirror':
    if args[1].endswith('/shared/'):
        if os.environ.get('FAIL_STAGE') == 'shared': sys.exit(9)
    else:
        excludes = [args[i+1] for i, arg in enumerate(args) if arg == '--exclude']
        for src in (root / 'remote').iterdir():
            if not any(fnmatch.fnmatch(src.name, pattern) for pattern in excludes):
                shutil.copyfile(src, pathlib.Path(args[2]) / src.name)
        if config.exists():
            assert json.loads(config.read_text())['channels']['matrix']['accessToken'] == 'current-token'
''')
        mc.chmod(0o755)
        script = SCRIPT.read_text().replace('/opt/agentteams/scripts/lib', str(self.lib))
        script = script.replace('/root/agentteams-fs', str(self.root / 'fs'))
        script = script.replace('/tmp/openclaw-local-sync.json', str(self.root / 'saved-local.json'))
        script = script.replace('/tmp/agentteams-worker-sync', str(self.root / 'sync-state'))
        self.script = self.root / 'sync.sh'
        self.script.write_text(script)
        self.env = dict(os.environ, TEST_ROOT=str(self.root), AGENTTEAMS_WORKER_NAME='test-worker',
                        AGENTTEAMS_STORAGE_PREFIX='fixture/bucket', PATH=f'{self.bin}:{os.environ["PATH"]}')

    def run_pull(self, failure=None):
        if failure:
            self.env['FAIL_STAGE'] = failure
        return subprocess.run(['sh', str(self.script)], env=self.env, text=True, capture_output=True)

    def assert_failed_without_publication(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(json.loads(self.config.read_text()), self.local)
        self.assertFalse((self.workspace / '.last-pull').exists())
        self.assertFalse((self.root / 'sync-state/last-successful-push').exists())
        self.assertEqual(list(self.workspace.glob('openclaw.json.sync.*')), [])

    def test_remote_token_is_never_published_and_manager_fields_still_update(self):
        result = self.run_pull()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        merged = json.loads(self.config.read_text())
        self.assertEqual(merged['channels']['matrix']['accessToken'], 'current-token')
        self.assertTrue(merged['models']['updated'])
        self.assertTrue(merged['tools']['local'])
        self.assertEqual((self.workspace / 'SOUL.md').read_text(), 'updated prompt')
        self.assertTrue((self.workspace / '.last-pull').exists())
        self.assertEqual(list(self.workspace.glob('openclaw.json.sync.*')), [])

    def test_download_failure_preserves_live_config_and_markers(self):
        self.assert_failed_without_publication(self.run_pull('cp'))

    def test_workspace_mirror_failure_preserves_live_config_and_markers(self):
        self.assert_failed_without_publication(self.run_pull('mirror'))

    def test_shared_mirror_failure_preserves_live_config_and_markers(self):
        self.assert_failed_without_publication(self.run_pull('shared'))

    def test_invalid_remote_preserves_live_config_and_markers(self):
        (self.remote / 'openclaw.json').write_text('invalid json')
        self.assert_failed_without_publication(self.run_pull())

    def test_no_local_config_uses_remote_config(self):
        self.config.unlink()
        result = self.run_pull()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(json.loads(self.config.read_text())['channels']['matrix']['accessToken'], 'expired-token')


if __name__ == '__main__':
    unittest.main()
