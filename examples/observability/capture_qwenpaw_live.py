"""Capture real QwenPaw model/tool OTLP batches for backend acceptance replay.

Run inside an isolated QwenPaw image with /artifacts mounted and the existing
full_session_demo model environment. No synthetic model or tool spans are emitted.
"""
import gzip
import json
from pathlib import Path
import runpy
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
from at_telemetry import flush

ARTIFACTS = Path('/artifacts')
ARTIFACTS.mkdir(exist_ok=True)


class Receiver(BaseHTTPRequestHandler):
    def do_POST(self):
        data = self.rfile.read(int(self.headers['Content-Length']))
        if self.headers.get('Content-Encoding') == 'gzip':
            data = gzip.decompress(data)
        (ARTIFACTS / f'batch-{uuid.uuid4().hex}.bin').write_bytes(data)
        self.send_response(200)
        self.send_header('Content-Type', 'application/x-protobuf')
        self.end_headers()

    def log_message(self, *args):
        pass


if __name__ == '__main__':
    receiver = ThreadingHTTPServer(('127.0.0.1', 4318), Receiver)
    threading.Thread(target=receiver.serve_forever, daemon=True).start()
    try:
        runpy.run_path(str(Path(__file__).with_name('full_session_demo.py')), run_name='__main__')
    finally:
        flush()
        receiver.shutdown()
    spans = []
    for file in ARTIFACTS.glob('batch-*.bin'):
        batch = ExportTraceServiceRequest.FromString(file.read_bytes())
        for resource in batch.resource_spans:
            for scope in resource.scope_spans:
                spans.extend(scope.spans)
    roots = [span for span in spans if span.name == 'worker.turn']
    models = [span for span in spans if any(a.key == 'gen_ai.operation.name' and a.value.string_value == 'chat' for a in span.attributes)]
    tools = [span for span in spans if any(a.key == 'gen_ai.operation.name' and a.value.string_value == 'execute_tool' for a in span.attributes)]
    failures = [span for span in tools if span.status.code == 2]
    assert len(roots) >= 2 and len(models) >= 2 and len(tools) >= 4 and failures, (len(roots), len(models), len(tools), len(failures))
    for root in roots:
        attrs = {a.key: a.value.string_value for a in root.attributes}
        assert attrs['agentteams.task.id'] == 'full-session-acceptance'
        assert attrs['agentteams.project.id'] == 'tracing-acceptance'
    summary = {'verified': 'live QwenPaw native model and tools; OTLP captured for backend replay',
               'turns': len(roots), 'models': len(models), 'tools': len(tools),
               'trace_ids': [span.trace_id.hex() for span in roots],
               'failed_tools': [{'trace_id': span.trace_id.hex(), 'span_id': span.span_id.hex(), 'name': span.name} for span in failures]}
    (ARTIFACTS / 'native-summary.json').write_text(json.dumps(summary, indent=2))
    print(json.dumps(summary))
