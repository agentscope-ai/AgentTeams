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
import openai, httpx
from opentelemetry.instrumentation.openai import OpenAIInstrumentor
assert OpenAIInstrumentor().is_instrumented_by_opentelemetry
body = {'id': 'test', 'object': 'chat.completion', 'model': 'memory-import-test',
        'choices': [{'index': 0, 'finish_reason': 'stop', 'message': {'role': 'assistant', 'content': 'ok'}}],
        'usage': {'prompt_tokens': 2, 'completion_tokens': 1, 'total_tokens': 3}}
client = openai.OpenAI(api_key='synthetic-test-key', base_url='http://test.invalid/v1',
    http_client=httpx.Client(transport=httpx.MockTransport(lambda request: httpx.Response(200, json=body))))
assert client.chat.completions.create(model='memory-import-test', messages=[{'role':'user','content':'test'}]).choices[0].message.content == 'ok'
client.close()
assert any(span.attributes.get('gen_ai.request.model') == 'memory-import-test' for span in exporter.get_finished_spans())
provider.shutdown()
'''
    result = subprocess.run([sys.executable, '-c', code], env=env, capture_output=True, text=True, timeout=90)
    assert result.returncode == 0, result.stdout + result.stderr
