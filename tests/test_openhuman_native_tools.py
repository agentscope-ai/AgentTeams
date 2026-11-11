"""Execute the actual native-workspace bootstrap for each Controller role."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import tomllib
import unittest

ROOT = Path(__file__).resolve().parents[1]
SOURCE = (ROOT / 'openhuman/scripts/openhuman-worker-entrypoint.sh').read_text()
BOOTSTRAP = re.search(r'if \[ .*?\n    # Native channel turns.*?\nfi\n(?=\n# --- LLM)', SOURCE, re.S).group(0)
# Start at the conditional immediately before the native-channel comment.
BOOTSTRAP = BOOTSTRAP[BOOTSTRAP.rfind('\nif [', 0, BOOTSTRAP.index('# Native channel turns')) + 1:]

class NativeToolsTests(unittest.TestCase):
    def test_controller_roles_have_real_tools_and_their_own_identity(self):
        for role in ['manager', 'standalone', 'team_leader']:
            with self.subTest(role=role), tempfile.TemporaryDirectory() as directory:
                home = Path(directory)
                (home / 'skills').mkdir()
                (home / 'skills/task.md').write_text('Use your assigned task tools.')
                (home / 'AGENTS.md').write_text('You have your assigned Controller role.')
                (home / 'SOUL.md').write_text('Be accurate.')
                (home / 'config.toml').write_text('')
                result = subprocess.run(['bash', '-e', '-c', BOOTSTRAP], capture_output=True, text=True,
                    env={**os.environ, 'WORKSPACE': directory, 'WORKER_NAME': 'demo',
                         'AGENTTEAMS_WORKER_NAME': 'demo', 'WORKER_CR_NAME': 'demo',
                         'AGENTTEAMS_AGENT_ROLE': role, 'BRIDGE_ROOM_ID': '!room:test',
                         'AGENTTEAMS_STORAGE_PREFIX': 'agentteams/test-storage',
                         'BRIDGE_USER_ID': '@demo:test', 'AGENTTEAMS_YOLO': '0'})
                self.assertEqual(result.returncode, 0, result.stderr)
                definition = home / 'workspace/agents/orchestrator.toml'
                self.assertTrue(definition.exists(), 'native role definition is missing')
                agent = tomllib.loads(definition.read_text())
                self.assertTrue({'shell', 'file_read', 'file_write'} <= set(agent['tools']['named']))
                self.assertIn('member.role: ' + role, (home / 'workspace/SOUL.md').read_text())
                self.assertTrue((home / 'workspace/skills/task.md').exists())
                policy = tomllib.loads((home / 'config.toml').read_text())['autonomy']
                self.assertIn('mc', policy['allowed_commands'])
                self.assertIn('agentteams/test-storage', (home / 'workspace/SOUL.md').read_text())
                self.assertIn('AgentTeams ' + role, agent['system_prompt']['inline'])

if __name__ == '__main__':
    unittest.main()
