"""Exercise the real MCP SDK and long-lived TeamHarness child in a QwenPaw image.

Run in an isolated container with AGENTTEAMS_OTEL_ENABLED=true and the OTLP
endpoint http://127.0.0.1:4318. This verifies transport, not live model output.
"""
import asyncio
import gzip
import json
import os
from pathlib import Path
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client
from opentelemetry import trace
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
import at_telemetry as telemetry
from at_runtime_adapters import bind

SPANS = []


class Receiver(BaseHTTPRequestHandler):
    def do_POST(self):
        data = self.rfile.read(int(self.headers['Content-Length']))
        if self.headers.get('Content-Encoding') == 'gzip':
            data = gzip.decompress(data)
        batch = ExportTraceServiceRequest.FromString(data)
        for resource in batch.resource_spans:
            for scope in resource.scope_spans:
                for span in scope.spans:
                    SPANS.append({'name': span.name, 'trace_id': span.trace_id.hex(),
                                  'task': next((a.value.string_value for a in span.attributes if a.key == 'agentteams.task.id'), ''),
                                  'status': span.status.code})
        self.send_response(200)
        self.send_header('Content-Type', 'application/x-protobuf')
        self.end_headers()

    def log_message(self, *args):
        pass


async def main():
    root = Path(__file__).resolve().parents[2]
    environment = {key: os.environ[key] for key in ('PATH', 'PYTHONPATH', 'AGENTTEAMS_OTEL_ENABLED',
                   'AGENTTEAMS_OTEL_RUNTIME', 'AGENTTEAMS_WORKER_NAME', 'OTEL_EXPORTER_OTLP_ENDPOINT')}
    environment['AGENTTEAMS_AGENT_ROLE'] = 'leader'
    environment['OTEL_BSP_SCHEDULE_DELAY'] = '100'
    server = StdioServerParameters(command=sys.executable, args=[str(root / 'plugins/teamharness/mcp/server.py')], env=environment)
    expected = {}
    async with stdio_client(server) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            for task, tool in [('first', 'health'), ('second', 'does_not_exist')]:
                with bind({'agentteams.task.id': task, 'agentteams.turn.id': task}):
                    with trace.get_tracer('acceptance').start_as_current_span('parent ' + task) as parent:
                        expected[task] = f'{parent.get_span_context().trace_id:032x}'
                        result = await session.call_tool(tool, {})
                        payload = json.loads(result.content[0].text)
                        assert payload['ok'] == (tool == 'health')
            telemetry.flush()
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline and len([s for s in SPANS if s['name'].startswith('mcp ')]) < 2:
                await asyncio.sleep(0.1)
    children = [s for s in SPANS if s['name'].startswith('mcp ')]
    assert len(children) == 2, SPANS
    assert all(span['trace_id'] == expected[span['task']] for span in children), children
    assert len(set(span['trace_id'] for span in children)) == 2
    assert next(span for span in children if span['task'] == 'second')['status'] == 2
    print(json.dumps({'verified': 'native MCP per-call propagation and error export', 'child_spans': children}))


if __name__ == '__main__':
    receiver = ThreadingHTTPServer(('127.0.0.1', 4318), Receiver)
    threading.Thread(target=receiver.serve_forever, daemon=True).start()
    try:
        asyncio.run(main())
    finally:
        telemetry.flush()
        receiver.shutdown()
