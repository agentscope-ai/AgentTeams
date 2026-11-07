"""Late-configuration acceptance: real Dashboard/Langfuse/Workers, fixture Controller API.

The fixture may only recreate the two pre-existing isolated demo Workers. It
does not replace the Controller's native env/reconcile regression tests.
"""
import argparse
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, urlopen

from prepare_matrix_workers import STATE, matrix
from verify import read as langfuse

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = ROOT / 'output/observability-demo'
APP = 'at-tracing-config-acceptance'
BASE = 'http://127.0.0.1:13004'
RUNTIMES = ('qwenpaw', 'hermes')
workers = {}


def docker(*args):
    return subprocess.check_output(['docker', *args], text=True).strip()


def recreate_worker(runtime, env):
    info = workers[runtime]['snapshot']
    name = 'at-tracing-worker-' + runtime
    assert info['Name'] == '/' + name
    assert set(info['NetworkSettings']['Networks']) == {'at-tracing-demo_default'}
    docker('rm', '-f', name)
    command = ['run', '-d', '--name', name, '--restart', 'unless-stopped', '--network', 'at-tracing-demo_default']
    for binding in info['HostConfig'].get('Binds') or []:
        command += ['-v', binding]
    for port, bindings in (info['HostConfig'].get('PortBindings') or {}).items():
        for binding in bindings:
            command += ['-p', f"{binding['HostIp'] or '127.0.0.1'}:{binding['HostPort']}:{port}"]
    merged = dict(item.split('=', 1) for item in info['Config']['Env'])
    for key in workers[runtime]['env']:
        merged.pop(key, None)
    merged.update(env)
    for key, value in merged.items():
        command += ['-e', f'{key}={value}']
    if info['Config'].get('Entrypoint'):
        command += ['--entrypoint', info['Config']['Entrypoint'][0]]
    command += [workers[runtime]['image']]
    command += info['Config'].get('Cmd') or []
    docker(*command)
    workers[runtime]['env'] = env


class ControllerFixture(BaseHTTPRequestHandler):
    def reply(self, body, status=200):
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_GET(self):
        if self.path == '/api/v1/teams':
            return self.reply({'teams': [{'name': 'tracing-team', 'teamName': 'Tracing Team'}]})
        if self.path == '/api/v1/workers':
            return self.reply({'workers': [self.worker(r) for r in RUNTIMES]})
        runtime = self.path.removeprefix('/api/v1/workers/').removeprefix('trace-')
        if runtime in RUNTIMES:
            return self.reply(self.worker(runtime))
        self.reply({'error': 'not found'}, 404)

    def worker(self, runtime):
        return {'name': 'trace-' + runtime, 'team': 'tracing-team', 'runtime': runtime,
                'containerManaged': True, 'envEditable': True, 'env': workers[runtime]['env']}

    def do_PUT(self):
        runtime = self.path.removeprefix('/api/v1/workers/').removeprefix('trace-')
        if runtime not in RUNTIMES:
            return self.reply({'error': 'not found'}, 404)
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        recreate_worker(runtime, body['env'])
        self.reply({'name': 'trace-' + runtime})

    def log_message(self, *args):
        pass


def api(method, path='', body=None):
    request = Request(BASE + '/api/agentteams/' + path, method=method,
                      headers={'Content-Type': 'application/json'},
                      data=json.dumps(body).encode() if body is not None else None)
    try:
        with urlopen(request, timeout=45) as response:
            return json.load(response)
    except HTTPError as error:
        raise RuntimeError(f'API {path}: HTTP {error.code}: {error.read().decode()}') from None


def start_dashboard(source, controller_port):
    subprocess.run(['docker', 'rm', '-f', APP], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    image = 'agentteams/agentteams-dashboard:local-20261104120000'
    volume = APP + '-config-' + str(time.time_ns())
    docker('run', '--rm', '--network', 'none', '--user', '0:0', '--entrypoint', 'sh', '-v', volume + ':/data', image, '-c', 'chown 1001:1001 /data')
    command = ['run', '-d', '--name', APP, '--network', 'at-tracing-demo_default', '-p', '127.0.0.1:13004:3000',
               '-v', str(source) + ':/app:ro', '-v', volume + ':/data']
    scope = {'teams': [{'name': 'tracing-team', 'label': 'Tracing Team'}],
             'workers': [{'name': 'trace-' + r, 'team': 'tracing-team', 'runtime': r} for r in RUNTIMES]}
    env = {'HOSTNAME': '0.0.0.0', 'PORT': '3000', 'DASHBOARD_CONFIG_FILE': '/data/config.json',
           'AGENTTEAMS_AUTH_DISABLED': 'true', 'AGENTTEAMS_TRACING_DEMO': 'true',
           'AGENTTEAMS_TRACING_DEMO_SCOPE': json.dumps(scope),
           'AGENTTEAMS_API_URL': f'http://host.docker.internal:{controller_port}',
           'DASHBOARD_BUILD_ID': (source / '.next/BUILD_ID').read_text().strip()}
    for key, value in env.items():
        command += ['-e', f'{key}={value}']
    docker(*command, image, 'node', 'server.js')
    for _ in range(60):
        try:
            return api('GET', 'observability/')
        except (OSError, RuntimeError):
            time.sleep(0.5)
    raise RuntimeError('Acceptance Dashboard did not start')


def send_turn(runtime, phase, run):
    state = json.loads(STATE.read_text())
    worker = state['accounts']['trace-' + runtime]['user_id']
    marker = f'CONFIG_{phase}_{runtime}_{run}'
    if phase == 'on':
        prompt = f'Use your native file tools to write "{marker}" to /tmp/at-config-{runtime}-{run}.txt and read it back. Do not delegate. Final reply: {marker}'
    else:
        prompt = f'Reply only with {marker}. Do not use tools or delegate.'
    before = int(time.time() * 1000)
    event = matrix('PUT', f"/rooms/{quote(state['room_id'], safe='')}/send/m.room.message/{time.time_ns()}",
                   {'msgtype': 'm.text', 'body': worker + ' ' + prompt, 'm.mentions': {'user_ids': [worker]}},
                   state['accounts']['trace-human']['access_token'])['event_id']
    for _ in range(90):
        messages = matrix('GET', f"/rooms/{quote(state['room_id'], safe='')}/messages?dir=b&limit=50", token=state['accounts']['trace-human']['access_token'])['chunk']
        if any(m.get('sender') == worker and m.get('origin_server_ts', 0) >= before and marker in m.get('content', {}).get('m.new_content', m.get('content', {})).get('body', '') for m in messages):
            return event
        time.sleep(1)
    raise RuntimeError(f'{runtime}/{phase} did not reply')


def wait_for_worker(runtime):
    marker = 'qwenpaw worker startup complete' if runtime == 'qwenpaw' else 'Gateway housekeeping started'
    deadline = time.monotonic() + 150
    while time.monotonic() < deadline:
        logs = subprocess.check_output(['docker', 'logs', 'at-tracing-worker-' + runtime], stderr=subprocess.STDOUT, text=True)
        if marker in logs:
            return
        time.sleep(2)
    raise RuntimeError(f'{runtime} did not become ready')


def traces_for(event, runtime, since):
    result = langfuse('/traces?limit=100&userId=trace-' + runtime + '&fromTimestamp=' + quote(since))['data']
    return [t for t in result if t.get('metadata', {}).get('agentteams.event.id') == event or t.get('metadata', {}).get('attributes', {}).get('agentteams.event.id') == event]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--keep', action='store_true', help='Keep the localhost acceptance Dashboard and fixture for browser inspection')
    parser.add_argument('--worker-tag', required=True, help='Tag of prepared agentteams/qwenpaw-worker and hermes-worker images')
    parser.add_argument('--skip-turns', action='store_true', help='Only verify save/test/persistence; native runtime proof still requires a full run')
    args = parser.parse_args()
    for runtime in RUNTIMES:
        snapshot = json.loads(docker('inspect', 'at-tracing-worker-' + runtime))[0]
        environment = dict(item.split('=', 1) for item in snapshot['Config']['Env'])
        workers[runtime] = {'snapshot': snapshot, 'image': f'agentteams/{runtime}-worker:{args.worker_tag}',
                            'env': {k: v for k, v in environment.items() if k.startswith(('AGENTTEAMS_OTEL_', 'OTEL_EXPORTER_OTLP_'))}}
        workers[runtime]['env']['AT_CONFIG_ACCEPTANCE_SENTINEL'] = 'preserved'
    fixture = ThreadingHTTPServer(('127.0.0.1', 19095), ControllerFixture)
    threading.Thread(target=fixture.serve_forever, daemon=True).start()
    source = ROOT.parent / 'dashboard/.next/standalone'
    initial = start_dashboard(source, fixture.server_port)
    assert initial['enabled'] is False and not initial['secretKeyConfigured']
    settings = {'enabled': False, 'captureContent': True, 'mode': 'collector', 'langfuseUrl': 'http://langfuse:3000',
                'publicUrl': 'http://localhost:13100', 'projectId': 'at-workers', 'publicKey': 'pk-lf-at-local-demo',
                'secretKey': 'sk-lf-at-local-demo', 'collectorEndpoint': 'http://collector:4318', 'collectorHeaders': ''}
    checks = ['start without observability', 'default disabled']
    tested = api('POST', 'observability/', {'action': 'test', 'settings': settings})
    assert tested['projectId'] == 'at-workers'
    checks.append('real Langfuse project authentication and empty OTLP batch')
    saved = api('PUT', 'observability/', settings)
    assert 'secretKey' not in saved and saved['secretKeyConfigured']
    revision = saved['revision']
    docker('restart', APP)
    for _ in range(60):
        try:
            persisted = api('GET', 'observability/')
            break
        except (OSError, RuntimeError):
            time.sleep(0.5)
    assert persisted['revision'] == revision
    checks.append('server restart retains private configuration')
    since = datetime.now(timezone.utc).isoformat()
    run = str(time.time_ns())[-10:]
    proof = []
    if not args.skip_turns:
        for phase in ('off', 'on', 'off-again'):
            settings['enabled'] = phase == 'on'
            saved = api('PUT', 'observability/', {**settings, 'secretKey': ''})
            selected = ['trace-' + r for r in RUNTIMES]
            applied = api('POST', 'observability/', {'action': 'apply', 'workers': selected, 'revision': saved['revision']})
            assert all(r['success'] for r in applied['results']), applied
            for name in selected:
                runtime = name.removeprefix('trace-')
                wait_for_worker(runtime)
                assert workers[runtime]['env']['AT_CONFIG_ACCEPTANCE_SENTINEL'] == 'preserved'
                assert 'sk-lf-at-local-demo' not in json.dumps(workers[runtime]['env'])
                event = send_turn(runtime, phase, run)
                time.sleep(6)
                matches = traces_for(event, runtime, since)
                if settings['enabled']:
                    for _ in range(30):
                        if matches:
                            break
                        time.sleep(1); matches = traces_for(event, runtime, since)
                    assert matches, (phase, runtime, 'missing trace')
                    details = [langfuse('/traces/' + trace['id']) for trace in matches]
                    observations = [o for trace in details for o in trace['observations']]
                    assert any(o['type'] == 'TOOL' and o.get('input') is not None and o.get('output') is not None for o in observations)
                    assert any(o['type'] == 'GENERATION' and o.get('usageDetails') for o in observations)
                    assert 'sk-lf-at-local-demo' not in json.dumps(details)
                else:
                    assert not matches, (phase, runtime, 'disabled Worker still exported')
                proof.append({'phase': phase, 'runtime': runtime, 'eventId': event, 'traceIds': [t['id'] for t in matches], 'revision': saved['revision']})
                print(json.dumps(proof[-1]), flush=True)
        checks += ['Worker env merge preserves unrelated values', 'Langfuse query key stays outside Worker configuration', 'real native off/on/off tasks through Collector']
    report = {'controller': 'fixture API; native Controller env/recreate tests run separately', 'checks': checks, 'proof': proof}
    (OUTPUT / 'configuration-acceptance.json').write_text(json.dumps(report, indent=2))
    print(json.dumps({'url': BASE + '/tracing-demo/', 'checks': checks}), flush=True)
    if args.keep:
        threading.Event().wait()
    fixture.shutdown()
    docker('rm', '-f', APP)


if __name__ == '__main__':
    main()
