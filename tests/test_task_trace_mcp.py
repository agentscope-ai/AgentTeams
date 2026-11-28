"""Per-call MCP propagation, including long-lived children and Matrix handoff."""
import asyncio
import importlib.util
import json
import sys
from pathlib import Path
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "shared/lib/telemetry"))

from opentelemetry import context, trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
import at_telemetry as telemetry
import at_trace_transport as transport


def test_mcp_client_injects_each_call_without_mutating_meta(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "leader")
    provider = TracerProvider()
    tracer = provider.get_tracer("test")
    calls = []

    class ClientSession:
        async def call_tool(self, name, arguments=None, **kwargs):
            calls.append(kwargs["meta"])
            return {"ok": True}

    transport.mcp_client(SimpleNamespace(ClientSession=ClientSession))
    original = {"custom": "keep"}

    async def run():
        for task in ("first", "second"):
            with tracer.start_as_current_span(task):
                token = telemetry._correlation.set({"agentteams.task.id": task})
                try:
                    await ClientSession().call_tool("taskflow", {}, meta=original)
                finally:
                    telemetry._correlation.reset(token)

    asyncio.run(run())
    assert original == {"custom": "keep"}
    assert calls[0][transport.MATRIX_TRACE_KEY]["traceparent"] != calls[1][transport.MATRIX_TRACE_KEY]["traceparent"]
    assert "first" in calls[0][transport.MATRIX_TRACE_KEY]["baggage"]
    assert "second" in calls[1][transport.MATRIX_TRACE_KEY]["baggage"]


def test_mcp_server_restores_and_clears_identity_for_every_call(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "leader")
    monkeypatch.setenv("AGENTTEAMS_OTEL_ENABLED", "true")
    provider = TracerProvider()
    tracer = provider.get_tracer("test")
    exporter = InMemorySpanExporter()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    monkeypatch.setattr(trace, "get_tracer", lambda *args, **kwargs: tracer)
    with tracer.start_as_current_span("leader.turn") as root:
        token = telemetry._correlation.set({"agentteams.task.id": "one"})
        carrier = transport.carrier()
        telemetry._correlation.reset(token)
        expected = root.get_span_context().trace_id
    with transport.mcp_request({transport.MATRIX_TRACE_KEY: carrier}, "taskflow"):
        assert trace.get_current_span().get_span_context().trace_id == expected
        assert telemetry._correlation.get()["agentteams.task.id"] == "one"
        spec = importlib.util.spec_from_file_location("matrix_mentions", ROOT / "plugins/teamharness/mcp/room/mention.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        content = module._matrix_content("delegate", ["@worker:matrix"])
        assert content[transport.MATRIX_TRACE_KEY]["traceparent"]
    assert telemetry._correlation.get() == {}
    assert not trace.get_current_span().get_span_context().is_valid
    with transport.mcp_request({}, "health"):
        assert telemetry._correlation.get() == {}
        assert not trace.get_current_span().get_span_context().is_valid






def test_urllib_matrix_handoff_injects_send_span_without_mutating_request(monkeypatch):
    import urllib.request
    provider = TracerProvider()
    exporter = InMemorySpanExporter()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    tracer = provider.get_tracer("test")
    monkeypatch.setattr(trace, "get_tracer", lambda *args, **kwargs: tracer)
    captured = []
    def send(request, **kwargs):
        captured.append(json.loads(request.data))
        return SimpleNamespace(status=200)
    module = SimpleNamespace(Request=urllib.request.Request, urlopen=send)
    transport.urllib_requests(module)
    original = urllib.request.Request('http://matrix/_matrix/client/v3/rooms/room/send/m.room.message/txn', data=b'{"msgtype":"m.text","body":"assignment"}', method='PUT', headers={'Content-Length': '49'})
    before = (original.data, dict(original.headers))
    with tracer.start_as_current_span('delegate'):
        module.urlopen(original, timeout=10)
    assert (original.data, original.headers) == before
    span = next(span for span in exporter.get_finished_spans() if span.name == 'matrix.send')
    assert f'{span.context.span_id:016x}' in captured[0][transport.MATRIX_TRACE_KEY]['traceparent']
