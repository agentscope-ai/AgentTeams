#!/usr/bin/env bash
# Exercise the full create-project flow with real jq and isolated API/Matrix/
# storage fixtures. No Manager paths or live services are changed.
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 - "${ROOT_DIR}" <<'PY'
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(sys.argv.pop())
SCRIPTS = ROOT / 'manager/agent/skills/project-management/scripts'

AGT = '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
p = Path(os.environ['CASE'])
with (p / 'agt.log').open('a') as f: f.write(' '.join(sys.argv[1:]) + '\\n')
data = json.loads((p / 'fixtures.json').read_text())
value = data.get(sys.argv[2], {}).get(sys.argv[3])
if value is None: sys.exit(1)
print(value if isinstance(value, str) else json.dumps(value))
'''
CURL = '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
p = Path(os.environ['CASE']); a = sys.argv[1:]
url = next(x for x in a if x.startswith('http'))
body = json.loads(a[a.index('-d') + 1]) if '-d' in a else None
headers = [a[i+1] for i,x in enumerate(a) if x == '-H']
with (p / 'curl.log').open('a') as f: f.write(json.dumps({'url':url,'body':body,'headers':headers}) + '\\n')
if url.endswith('/createRoom'): print('{"room_id":"!project:matrix.test"}')
elif url.endswith('/login'): print(json.dumps({'access_token':'tok-' + body['identifier']['user']}))
elif url.endswith('/join'): print('403' if 'Authorization: Bearer tok-runtime-alice' in headers and os.environ.get('JOIN_FAIL') else '200', end='')
else: print('{}')
'''
MC = '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
p = Path(os.environ['CASE']); a = sys.argv[1:]
with (p / 'mc.log').open('a') as f: f.write(' '.join(a) + '\\n')
if a[0] == 'cat':
    value = json.loads((p / 'fixtures.json').read_text()).get('passwords', {}).get(a[1])
    if value is None: sys.exit(1)
    print(value)
'''

class ProjectTeamTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.p = Path(self.temp.name)
        for name in ('bin', 'data/worker-creds', 'tmp', 'home/.copaw/workspaces/default', 'fs/agents/manager'):
            (self.p / name).mkdir(parents=True)
        for name, text in [('agt', AGT), ('curl', CURL), ('mc', MC)]:
            f = self.p / 'bin' / name; f.write_text(text); f.chmod(0o755)
        for name in ('create-project.sh', 'project-members.sh'):
            text = (SCRIPTS / name).read_text()
            # Rewrite existing /tmp paths first, before inserting temporary paths.
            for old, new in [('/tmp/', str(self.p / 'tmp') + '/'),
                             ('/opt/agentteams/scripts/lib/agentteams-env.sh', str(self.p / 'env.sh')),
                             ('/root/agentteams-fs', str(self.p / 'fs')),
                             ('/data/', str(self.p / 'data') + '/')]:
                text = text.replace(old, new)
            (self.p / name).write_text(text)
        (self.p / 'env.sh').write_text('log() { echo "$*" >&2; }\n')
        for name in ('fs/agents/manager/openclaw.json', 'home/openclaw.json', 'home/.copaw/workspaces/default/agent.json'):
            (self.p / name).write_text('{}')
        self.fixtures = {
            'teams': {'alpha': {'leaderName': 'leader-cr', 'workerNames': ['worker-cr', 'worker-cr']}},
            'workers': {'leader-cr': {'name': 'leader-cr', 'workerName': 'runtime-lead'},
                        'worker-cr': {'name': 'worker-cr', 'workerName': 'runtime-alice'}},
            'passwords': {'storage/agents/runtime-alice/credentials/matrix/password': 'alice-pass',
                          'storage/agents/extra/credentials/matrix/password': 'extra-pass'}}
        (self.p / 'data/worker-creds/leader-cr.env').write_text('WORKER_PASSWORD="leader-pass"\n')
        self.env = dict(os.environ, CASE=str(self.p), HOME=str(self.p / 'home'),
                        PATH=str(self.p / 'bin') + ':' + os.environ['PATH'],
                        AGENTTEAMS_MATRIX_URL='http://matrix.test', AGENTTEAMS_MATRIX_DOMAIN='matrix.test',
                        AGENTTEAMS_STORAGE_PREFIX='storage', MANAGER_MATRIX_TOKEN='manager-token',
                        AGENTTEAMS_ADMIN_PASSWORD='', AGENTTEAMS_ADMIN_USER='admin',
                        AGENTTEAMS_JOIN_STATUS_FILE=str(self.p / 'tmp/status.json'))

    def run_project(self, *args, ok=True, error=None):
        (self.p / 'fixtures.json').write_text(json.dumps(self.fixtures))
        r = subprocess.run(['bash', str(self.p / 'create-project.sh'), '--id', 'project', '--title', 'Team project', *args],
                           env=self.env, text=True, capture_output=True)
        if not ok:
            self.assertNotEqual(r.returncode, 0, r.stdout + r.stderr)
            self.assertIsNotNone(error)
            self.assertIn(error, r.stdout + r.stderr)
            self.assertIn(error, json.loads(r.stdout)["error"])
            self.assertFalse((self.p / 'fs/shared/projects').exists())
            self.assertFalse((self.p / 'curl.log').exists())
            self.assertFalse((self.p / 'mc.log').exists())
            return
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.logs = r.stderr
        self.result = json.loads(r.stdout.split('---RESULT---\n')[1])
        self.requests = [json.loads(x) for x in (self.p / 'curl.log').read_text().splitlines()]
        self.room = next(x['body'] for x in self.requests if x['url'].endswith('/createRoom'))
        self.status = json.loads((self.p / 'tmp/status.json').read_text())

    def test_team_resolves_runtime_and_credential_names_and_deduplicates(self):
        self.run_project('--team', 'alpha', '--workers', 'runtime-alice, extra,extra', '--grant-admin', 'bob')
        names = ['runtime-lead', 'runtime-alice', 'extra']
        self.assertEqual(self.result['workers'], names)
        self.assertEqual(self.result['joined'], names)
        self.assertEqual(self.result['join_pending'], [])
        self.assertEqual(self.result['join_failed'], [])
        self.assertEqual(self.room['invite'], ['@admin:matrix.test'] + ['@'+n+':matrix.test' for n in names] + ['@bob:matrix.test'])
        levels = self.room['power_level_content_override']['users']
        self.assertEqual(levels['@bob:matrix.test'], 100)
        self.assertTrue(all(levels['@'+n+':matrix.test'] == 0 for n in names))
        logins = [x['body'] for x in self.requests if x['url'].endswith('/login')]
        self.assertEqual([(x['identifier']['user'], x['password']) for x in logins],
                         [('runtime-lead', 'leader-pass'), ('runtime-alice', 'alice-pass'), ('extra', 'extra-pass')])
        joins = [x for x in self.requests if x['url'].endswith('/join')]
        self.assertEqual(len(joins), 3)
        self.assertTrue(all('Authorization: Bearer tok-'+n in joins[i]['headers'] for i,n in enumerate(names)))
        self.assertEqual([x['status'] for x in self.status['workers']], ['joined']*3)
        mc = (self.p / 'mc.log').read_text()
        self.assertNotIn('/agents/worker-cr/', mc)
        self.assertNotIn('/agents/leader-cr/', mc)
        for file, key in [('home/openclaw.json', 'groupAllowFrom'), ('home/.copaw/workspaces/default/agent.json', 'group_allow_from')]:
            cfg = json.loads((self.p / file).read_text())['channels']['matrix']
            self.assertEqual(set(cfg[key]), {'@'+n+':matrix.test' for n in names} | {'@bob:matrix.test'})
        meta = json.loads((self.p / 'fs/shared/projects/project/meta.json').read_text())
        self.assertEqual(meta['workers'], names)

    def test_team_with_three_workers_invites_and_joins_all_four(self):
        self.fixtures['teams']['alpha']['workerNames'] = ['worker-cr', 'bob-cr', 'carol-cr']
        for name in ('bob', 'carol'):
            self.fixtures['workers'][name+'-cr'] = {'name': name+'-cr', 'workerName': 'runtime-'+name}
            self.fixtures['passwords']['storage/agents/runtime-'+name+'/credentials/matrix/password'] = name+'-pass'
        self.run_project('--team', 'alpha')
        names = ['runtime-lead', 'runtime-alice', 'runtime-bob', 'runtime-carol']
        self.assertEqual(self.result['workers'], names)
        self.assertEqual(self.result['joined'], names)
        self.assertEqual(self.room['invite'], ['@admin:matrix.test'] + ['@'+n+':matrix.test' for n in names])
        self.assertEqual(self.status['summary'], 'joined=4 pending=0 failed=0')
        self.assertLess(self.logs.index('Team alpha roster:'), self.logs.index('Step 1:'))

    def test_empty_worker_name_falls_back_to_cr_name(self):
        self.fixtures['teams']['alpha'] = {'leaderName': 'leader-cr'}
        self.fixtures['workers']['leader-cr']['workerName'] = ''
        self.run_project('--team', 'alpha')
        self.assertEqual(self.result['workers'], ['leader-cr'])
        self.assertEqual(self.result['joined'], ['leader-cr'])

    def test_workers_alone_still_means_explicit_runtime_names(self):
        self.run_project('--workers', 'extra,extra')
        self.assertEqual(self.result['workers'], ['extra'])
        self.assertFalse((self.p / 'agt.log').exists())

    def test_pending_and_failed_joins_remain_distinct(self):
        self.env['JOIN_FAIL'] = '1'
        self.run_project('--team', 'alpha', '--workers', 'no-creds')
        self.assertEqual(self.result['joined'], ['runtime-lead'])
        self.assertEqual(self.result['join_pending'], ['no-creds'])
        self.assertEqual(self.result['join_failed'], ['runtime-alice'])
        self.assertEqual(self.status['summary'], 'joined=1 pending=1 failed=1')
        self.assertEqual([(x['status'], x['detail']) for x in self.status['workers']],
                         [('joined', 'http-200'), ('failed', 'http-403'), ('pending', 'no-credentials')])

    def test_unknown_team_has_no_side_effects(self):
        self.run_project('--team', 'missing', ok=False, error='Team missing not found or not readable')

    def test_empty_team_has_no_side_effects(self):
        self.fixtures['teams']['alpha'] = {'workerNames': []}
        self.run_project('--team', 'alpha', ok=False, error='Team alpha has no leader or workers')

    def test_unreadable_worker_after_leader_has_no_side_effects(self):
        del self.fixtures['workers']['worker-cr']
        self.run_project('--team', 'alpha', ok=False, error='Worker worker-cr in Team alpha not found or not readable')

    def test_malformed_worker_has_no_side_effects(self):
        self.fixtures['workers']['worker-cr'] = 'invalid JSON'
        self.run_project('--team', 'alpha', ok=False, error='Worker worker-cr in Team alpha returned no readable runtime identity')

    def test_wrong_worker_response_has_no_side_effects(self):
        self.fixtures['workers']['worker-cr']['name'] = 'some-other-worker'
        self.run_project('--team', 'alpha', ok=False, error='Worker worker-cr in Team alpha returned no readable runtime identity')

unittest.main(verbosity=2)
PY
