"""One-off immutable published image acceptance; no model calls or host socket."""
import json
import os
from pathlib import Path
import secrets
import subprocess
import time
import urllib.error
import urllib.request

name = 'published-core-acceptance'
image = os.environ['IMAGE']
arch = os.environ['ARCH']
password = secrets.token_urlsafe(24)
env = {
    'AGENTTEAMS_RUNTIME': 'embedded',
    'AGENTTEAMS_MANAGER_ENABLED': 'false',
    'AGENTTEAMS_CONTROLLER_NAME': name,
    'AGENTTEAMS_RESOURCE_PREFIX': 'published-core-',
    'AGENTTEAMS_CONTROLLER_URL': 'http://127.0.0.1:8090',
    'AGENTTEAMS_MATRIX_URL': 'http://127.0.0.1:6167',
    'AGENTTEAMS_MATRIX_DOMAIN': 'published-core.local:6167',
    'AGENTTEAMS_MATRIX_APPSERVICE_URL': 'http://127.0.0.1:8090',
    'AGENTTEAMS_AI_GATEWAY_URL': 'http://127.0.0.1:8080',
    'AGENTTEAMS_AI_GATEWAY_ADMIN_URL': 'http://127.0.0.1:8001',
    'AGENTTEAMS_FS_ENDPOINT': 'http://127.0.0.1:9000',
    'AGENTTEAMS_MINIO_BUCKET': 'agentteams-storage',
    'AGENTTEAMS_FS_BUCKET': 'agentteams-storage',
    'AGENTTEAMS_ADMIN_USER': 'releasecheck',
    'AGENTTEAMS_ADMIN_PASSWORD': password,
    'AGENTTEAMS_MINIO_USER': 'releasecheck',
    'AGENTTEAMS_MINIO_PASSWORD': password,
    'AGENTTEAMS_REGISTRATION_TOKEN': secrets.token_urlsafe(32),
    'AGENTTEAMS_MATRIX_APPSERVICE_AS_TOKEN': secrets.token_urlsafe(32),
    'AGENTTEAMS_MATRIX_APPSERVICE_HS_TOKEN': secrets.token_urlsafe(32),
}
envfile = Path('/tmp/published-core.env')
envfile.write_text(''.join(k + '=' + v + '\n' for k, v in env.items()))
envfile.chmod(0o600)

def docker(*args, **kwargs):
    return subprocess.run(['docker', *args], check=True, **kwargs)

def request(port, path, body=None, token=None):
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    req = urllib.request.Request(f'http://127.0.0.1:{port}{path}', headers=headers,
        data=json.dumps(body).encode() if body is not None else None)
    with urllib.request.urlopen(req, timeout=10) as response:
        data = response.read()
        return json.loads(data) if data else None

docker('pull', '--platform', 'linux/' + arch, image)
docker('run', '--rm', '--network', 'none', '--entrypoint', 'sh', image, '-ec',
       'agt tracing list --help >/dev/null; agt tracing worker --help >/dev/null; '
       'agt tracing session --help >/dev/null; test -x /usr/local/bin/tuwunel; '
       'test -x /usr/local/bin/agentteams-controller')
try:
    docker('run', '-d', '--name', name, '--env-file', str(envfile),
           '-p', '127.0.0.1:8090:8090', '-p', '127.0.0.1:6167:6167', image,
           stdout=subprocess.DEVNULL)
    end = time.monotonic() + 360
    last = 'not ready'
    while time.monotonic() < end:
        try:
            login = request(6167, '/_matrix/client/v3/login', {
                'type': 'm.login.password',
                'identifier': {'type': 'm.id.user', 'user': 'releasecheck'},
                'password': password,
            })
            token = login['access_token']
            me = request(8090, '/api/v1/me', token=token)
            assert me['permissionLevel'] == 1
            for path in ['/api/v1/workers', '/api/v1/teams', '/api/v1/skills',
                         '/api/v1/gateway/ai/providers']:
                request(8090, path, token=token)
            result = {'arch': arch, 'image': image, 'packagedCLI': 'PASS',
                      'matrixLogin': 'PASS', 'controllerIdentity': 'PASS',
                      'workersTeamsSkillsModels': 'PASS', 'hostSocketMounted': False}
            Path('published-core-' + arch + '.json').write_text(json.dumps(result, indent=2))
            print(json.dumps(result), flush=True)
            break
        except (OSError, AssertionError, KeyError) as error:
            last = type(error).__name__
            time.sleep(3)
    else:
        raise RuntimeError('Published stack did not become ready: ' + last)
finally:
    subprocess.run(['docker', 'rm', '-fv', name], stdout=subprocess.DEVNULL)
    envfile.unlink(missing_ok=True)
