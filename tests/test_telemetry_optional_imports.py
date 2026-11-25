"""Transport-only processes should retain tracing without importing a model SDK."""
import os
from pathlib import Path
import subprocess
import sys


def test_receipt_bootstrap_defers_openai_and_still_traces_first_model_call():
    root = Path(__file__).resolve().parents[1]
    env = {**os.environ, 'AGENTTEAMS_OTEL_ENABLED': 'false', 'AGENTTEAMS_OTEL_RUNTIME': 'hermes',
           'PYTHONPATH': str(root / 'shared/lib/telemetry') + os.pathsep + str(root / 'shared/lib')}
    env.pop('AGENTTEAMS_LOG_NATIVE_EVENTS', None)
    code = '''
import sys
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
exporter = InMemorySpanExporter()
provider = TracerProvider()
provider.add_span_processor(SimpleSpanProcessor(exporter))
trace.set_tracer_provider(provider)
import at_telemetry
at_telemetry.configure(local_only=True)
import agentteams_receipts
assert 'openai' not in sys.modules, 'Receipt startup eagerly imported the model SDK'
import openai
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from opentelemetry.instrumentation.openai import OpenAIInstrumentor
assert OpenAIInstrumentor().is_instrumented_by_opentelemetry
body = {'id': 'test', 'object': 'chat.completion', 'model': 'memory-import-test',
        'choices': [{'index': 0, 'finish_reason': 'stop', 'message': {'role': 'assistant', 'content': 'ok'}}],
        'usage': {'prompt_tokens': 2, 'completion_tokens': 1, 'total_tokens': 3}}
class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', '0')))
        payload = json.dumps(body).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)
    def log_message(self, *args):
        pass
server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
thread = threading.Thread(target=server.serve_forever)
thread.start()
try:
    with openai.OpenAI(api_key='synthetic-test-key', base_url=f'http://127.0.0.1:{server.server_port}/v1') as client:
        assert client.chat.completions.create(model='memory-import-test', messages=[{'role':'user','content':'test'}]).choices[0].message.content == 'ok'
finally:
    server.shutdown()
    server.server_close()
    thread.join()
assert any(span.attributes.get('gen_ai.request.model') == 'memory-import-test' for span in exporter.get_finished_spans())
provider.shutdown()
'''
    result = subprocess.run([sys.executable, '-c', code], env=env, capture_output=True, text=True, timeout=90)
    assert result.returncode == 0, result.stdout + result.stderr


def test_python_without_optional_otel_keeps_business_output_clean():
    root = Path(__file__).resolve().parents[1]
    env = {**os.environ, 'AGENTTEAMS_OTEL_ENABLED': 'true',
           'PYTHONPATH': str(root / 'shared/lib/telemetry')}
    result = subprocess.run([sys.executable, '-S', '-c',
                             'import sitecustomize; print("business-result")'],
                            env=env, capture_output=True, text=True, timeout=10)
    assert result.returncode == 0
    assert result.stdout == 'business-result\n'
    assert result.stderr == ''


def test_bootstrap_keeps_unrelated_import_failures_visible(tmp_path):
    root = Path(__file__).resolve().parents[1]
    (tmp_path / 'at_telemetry.py').write_text('import missing_application_dependency\n')
    env = {**os.environ, 'AGENTTEAMS_OTEL_ENABLED': 'true',
           'PYTHONPATH': str(tmp_path) + os.pathsep + str(root / 'shared/lib/telemetry')}
    result = subprocess.run([sys.executable, '-S', '-c',
                             'import sitecustomize; print("business-result")'],
                            env=env, capture_output=True, text=True, timeout=10)
    assert result.returncode == 0
    assert result.stdout == 'business-result\n'
    assert 'OTel bootstrap failed' in result.stderr
    assert 'missing_application_dependency' in result.stderr
