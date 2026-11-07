"""Temporary protocol diagnostic. This is NOT a Langfuse substitute."""

import gzip
from http.server import BaseHTTPRequestHandler, HTTPServer
import json

from google.protobuf.json_format import MessageToDict
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest


class Probe(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        payload = self.rfile.read(int(self.headers["Content-Length"]))
        if self.headers.get("Content-Encoding") == "gzip":
            payload = gzip.decompress(payload)
        request = ExportTraceServiceRequest.FromString(payload)
        for resource in request.resource_spans:
            for scope in resource.scope_spans:
                for span in scope.spans:
                    record = {"name": span.name, "traceId": span.trace_id.hex(),
                              "spanId": span.span_id.hex(), "parentSpanId": span.parent_span_id.hex(),
                              "status": span.status.code,
                              "attributes": {a.key: MessageToDict(a.value) for a in span.attributes}}
                    with open("/evidence/probe.jsonl", "a") as output:
                        output.write(json.dumps(record) + "\n")
        self.send_response(200)
        self.send_header("Content-Type", "application/x-protobuf")
        self.end_headers()


print("OTLP protocol probe ready", flush=True)
HTTPServer(("0.0.0.0", 4318), Probe).serve_forever()
