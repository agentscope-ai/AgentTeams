"""Local protocol fixture: SDK -> Collector -> OTLP receiver -> signed SLS -> Dashboard.

All model/tool results are synthetic and carry the fixture tag. This is not a
cloud AgentLoop ingestion, evaluation, or organization-login acceptance claim.
"""
import argparse
import base64
import hashlib
import gzip
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import re
import subprocess
import sys
import threading
import time
from urllib.parse import parse_qs, urlparse
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'shared/lib/telemetry'))
import at_telemetry as telemetry
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor

APP = 'at-agentloop-demo-dashboard'
COLLECTOR = 'at-agentloop-demo-collector'
BASE = 'http://127.0.0.1:13005'
ROWS = {}
READS = []


def value(item):
    kind = item.WhichOneof('value')
    if kind == 'array_value':
        return [value(v) for v in item.array_value.values]
    if kind == 'kvlist_value':
        return {v.key: value(v.value) for v in item.kvlist_value.values}
    return getattr(item, kind) if kind else None


class Fixture(BaseHTTPRequestHandler):
    def reply(self, body, status=200):
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('x-log-progress', 'Complete')
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    def do_POST(self):
        if self.path != '/v1/traces' or self.headers.get('x-arms-license-key') != 'fixture-ingestion' or self.headers.get('x-cms-workspace') != 'fixture-workspace':
            return self.reply({'error': 'fixture ingestion auth failed'}, 403)
        body = self.rfile.read(int(self.headers['Content-Length']))
        if self.headers.get('Content-Encoding') == 'gzip':
            body = gzip.decompress(body)
        message = ExportTraceServiceRequest.FromString(body)
        for resource in message.resource_spans:
            for scope in resource.scope_spans:
                for span in scope.spans:
                    ROWS[(span.trace_id.hex(), span.span_id.hex())] = {
                        'traceId': span.trace_id.hex(), 'spanId': span.span_id.hex(), 'parentSpanId': span.parent_span_id.hex(),
                        'operationName': span.name, 'startTime': str(span.start_time_unix_nano),
                        'duration': str(span.end_time_unix_nano - span.start_time_unix_nano), 'statusCode': str(span.status.code),
                        'attributes': json.dumps({a.key: value(a.value) for a in span.attributes}),
                    }
        self.send_response(200)
        self.send_header('Content-Type', 'application/x-protobuf')
        self.end_headers()

    def do_GET(self):
        parsed = urlparse(self.path)
        params = {k: v[0] for k, v in parse_qs(parsed.query).items()}
        headers = {k.lower(): v for k, v in self.headers.items()}
        signed = ''.join(f'{k}:{headers[k]}\n' for k in sorted(headers) if k.startswith(('x-log-', 'x-acs-')))
        resource = parsed.path + '?' + '&'.join(f'{k}={params[k]}' for k in sorted(params))
        source = f"GET\n\napplication/json\n{headers.get('date')}\n{signed}{resource}"
        signature = base64.b64encode(hmac.new(b'fixture-secret', source.encode(), hashlib.sha1).digest()).decode()
        if parsed.path != '/logstores/trace-store' or not hmac.compare_digest(headers.get('authorization', ''), 'LOG fixture-id:' + signature) or headers.get('x-acs-security-token') != 'fixture-sts':
            return self.reply({'error': 'fixture SLS signature failed'}, 403)
        READS.append({'offset': params.get('offset'), 'from': params.get('from')})
        query = params.get('query', '')
        rows = list(ROWS.values())
        if '__connection_check__' in query:
            rows = []
        if 'attributes.gen_ai.span.kind:"ENTRY"' in query:
            rows = [r for r in rows if json.loads(r['attributes']).get('gen_ai.span.kind') == 'ENTRY']
        match = re.search(r'traceId:"([a-f0-9]{32})"', query)
        if match:
            rows = [r for r in rows if r['traceId'] == match[1]]
        rows = [r for r in rows if int(params['from']) <= int(r['startTime']) // 1000000000 < int(params['to'])]
        rows.sort(key=lambda r: (r['startTime'], r['spanId']), reverse=True)
        offset, line = int(params.get('offset', 0)), int(params.get('line', 100))
        self.reply(rows[offset:offset + line])

    def log_message(self, *_):
        pass


def docker(*args):
    return subprocess.check_output(['docker', *args], text=True).strip()


def api(method, path='', body=None):
    request = Request(BASE + '/api/agentteams/' + path, method=method, headers={'Content-Type': 'application/json'},
                      data=json.dumps(body).encode() if body is not None else None)
    with urlopen(request, timeout=30) as response:
        return json.load(response)


def emit(runtime):
    import os
    worker = 'trace-' + runtime
    os.environ['AGENTTEAMS_WORKER_NAME'] = worker
    os.environ['AGENTTEAMS_OTEL_RUNTIME'] = runtime
    provider = TracerProvider(resource=Resource.create({'service.name': 'agentloop-protocol-fixture'}))
    provider.add_span_processor(telemetry.IdentityProcessor())
    provider.add_span_processor(SimpleSpanProcessor(telemetry.SanitizingExporter(OTLPSpanExporter(endpoint='http://127.0.0.1:14328/v1/traces'))))
    token = telemetry._correlation.set({'agentteams.session.id': worker + ':fixture-session', 'agentteams.turn.id': 'fixture-turn', 'agentteams.team.id': 'tracing-team'})
    tracer = provider.get_tracer('agentteams.protocol.fixture')
    with tracer.start_as_current_span('worker.turn', attributes={'langfuse.observation.type': 'agent', 'agentteams.demo.fixture': True}) as root:
        telemetry.set_io(root, input='Synthetic AgentLoop protocol acceptance', output='fixture complete')
        for n in (1, 2):
            with tracer.start_as_current_span('chat fixture-model', attributes={'gen_ai.operation.name': 'chat', 'gen_ai.request.model': 'fixture-model', 'gen_ai.usage.input_tokens': 80 * n, 'gen_ai.usage.output_tokens': 20 * n}) as model:
                telemetry.set_io(model, input={'messages': [{'role': 'user', 'content': 'synthetic prompt'}]}, output='synthetic answer')
        with tracer.start_as_current_span('tool read_file', attributes={'gen_ai.operation.name': 'execute_tool'}) as tool:
            telemetry.set_io(tool, input={'path': '/fixture/missing'}, output={'error': 'fixture missing file'})
        # A trace exceeding one SLS page must retain all observations.
        for _ in range(100):
            with tracer.start_as_current_span('http fixture'):
                pass
    telemetry._correlation.reset(token)
    provider.shutdown()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--keep', action='store_true')
    args = parser.parse_args()
    server = ThreadingHTTPServer(('127.0.0.1', 19098), Fixture)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    source = ROOT.parent / 'dashboard/.next/standalone'
    for name in (APP, COLLECTOR):
        subprocess.run(['docker', 'rm', '-f', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    docker('run', '-d', '--name', COLLECTOR, '--network', 'at-tracing-demo_default', '--tmpfs', '/var/lib/otelcol:rw,mode=777', '-p', '127.0.0.1:14328:4318',
           '-e', 'AGENTLOOP_OTLP_ENDPOINT=http://host.docker.internal:19098', '-e', 'AGENTLOOP_LICENSE_KEY=fixture-ingestion', '-e', 'AGENTLOOP_ARMS_PROJECT=fixture-project', '-e', 'AGENTLOOP_WORKSPACE=fixture-workspace',
           '-v', str(ROOT / 'examples/observability/collector.agentloop.yaml') + ':/etc/otelcol/config.yaml:ro',
           'ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-contrib:0.120.0', '--config=/etc/otelcol/config.yaml')
    scope = {'teams': [{'name': 'tracing-team', 'label': 'Tracing Team'}], 'workers': [{'name': 'trace-' + r, 'team': 'tracing-team', 'runtime': r} for r in ('qwenpaw', 'hermes')]}
    env = {'HOSTNAME': '0.0.0.0', 'PORT': '3000', 'DASHBOARD_CONFIG_FILE': '/data/config.json', 'AGENTTEAMS_AUTH_DISABLED': 'true',
           'AGENTTEAMS_TRACING_DEMO': 'true', 'AGENTTEAMS_TRACING_DEMO_SCOPE': json.dumps(scope), 'AGENTLOOP_SLS_ENDPOINT': 'http://host.docker.internal:19098',
           'ALIBABA_CLOUD_ACCESS_KEY_ID': 'fixture-id', 'ALIBABA_CLOUD_ACCESS_KEY_SECRET': 'fixture-secret', 'ALIBABA_CLOUD_SECURITY_TOKEN': 'fixture-sts',
           'DASHBOARD_BUILD_ID': (source / '.next/BUILD_ID').read_text().strip()}
    command = ['run', '-d', '--name', APP, '--network', 'at-tracing-demo_default', '--tmpfs', '/data:rw,mode=1777', '-p', '127.0.0.1:13005:3000', '-v', str(source) + ':/app:ro']
    for key, val in env.items():
        command += ['-e', key + '=' + val]
    docker(*command, 'agentteams/agentteams-dashboard:local-20261104120000', 'node', 'server.js')
    for _ in range(60):
        try:
            settings = api('GET', 'observability/')
            break
        except OSError:
            time.sleep(0.5)
    settings.update({'backend': 'agentloop', 'enabled': True, 'agentloop': {'region': 'cn-hangzhou', 'workspace': 'fixture-workspace', 'project': 'trace-project', 'logstore': 'trace-store'},
                     'collectorEndpoint': 'http://' + COLLECTOR + ':4318', 'secretKey': '', 'collectorHeaders': ''})
    saved = api('PUT', 'observability/', settings)
    assert 'secretKey' not in saved and 'fixture-secret' not in json.dumps(saved)
    tested = api('POST', 'observability/', {'action': 'test', 'settings': settings})
    assert tested['projectId'] == 'trace-project'
    for runtime in ('qwenpaw', 'hermes'):
        emit(runtime)
    for _ in range(30):
        if len(ROWS) == 208:
            break
        time.sleep(0.5)
    listing = api('GET', 'traces/')
    assert listing['meta']['totalItems'] == 2, listing
    assert 'synthetic prompt' not in json.dumps(listing)
    for item in listing['data']:
        detail = api('GET', 'traces/' + item['id'] + '/')
        assert len(detail['observations']) == 104
        models = [o for o in detail['observations'] if o['type'] == 'GENERATION']
        assert len(models) == 2 and sum(sum(o['usageDetails'].values()) for o in models) == 300
        assert any(o['type'] == 'TOOL' and o['level'] == 'ERROR' and o['input'] and o['output'] for o in detail['observations'])
        assert 'fixture' in detail['tags']
    assert any(int(r['offset']) >= 100 for r in READS)
    output = {'fixture': True, 'cloudVerified': False, 'traceCount': len(listing['data']), 'spansPerTrace': 104, 'tokenCountPerTrace': 300,
              'signedQueries': len(READS), 'url': BASE + '/tracing-demo/'}
    (ROOT / 'output/observability-demo/agentloop-acceptance.json').write_text(json.dumps(output, indent=2))
    print(json.dumps(output), flush=True)
    if args.keep:
        threading.Event().wait()
    server.shutdown()
    docker('rm', '-f', APP, COLLECTOR)


if __name__ == '__main__':
    main()
