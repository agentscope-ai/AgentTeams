import asyncio
import base64
import json
import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib/telemetry"))

from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
import at_telemetry as telemetry


def test_local_runtime_events_keep_model_tool_failures_without_content(tmp_path, monkeypatch):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'shared/lib'))
    monkeypatch.setenv('AGENTTEAMS_LOG_DIR', str(tmp_path))
    monkeypatch.setenv('MODEL_API_KEY', 'secret-model-key')
    provider = TracerProvider()
    provider.add_span_processor(telemetry.RuntimeLogProcessor())
    tracer = provider.get_tracer('test')
    for kind, attrs, reason in [
        ('model', {'gen_ai.operation.name': 'chat', 'gen_ai.request.model': 'test-model'}, 'HTTP 401 api_key=secret-model-key'),
        ('tool', {'gen_ai.operation.name': 'execute_tool', 'gen_ai.tool.name': 'shell'}, 'command exited 127'),
    ]:
        with tracer.start_as_current_span(kind, attributes={**attrs, 'gen_ai.input.messages': 'PRIVATE PROMPT',
                                                           'gen_ai.tool.call.arguments': 'PRIVATE ARGUMENTS'}) as span:
            span.set_status(trace.Status(trace.StatusCode.ERROR, reason))
    records = [json.loads(line) for line in (tmp_path / 'events.jsonl').read_text().splitlines()]
    ends = [record for record in records if record['event'].endswith('.end')]
    assert [record['status'] for record in ends] == ['failed', 'failed']
    assert 'HTTP 401' in ends[0]['error']
    assert '127' in ends[1]['error']
    raw = json.dumps(records)
    for private in ['secret-model-key', 'PRIVATE PROMPT', 'PRIVATE ARGUMENTS']:
        assert private not in raw
    provider.shutdown()


def test_local_only_bootstrap_does_not_create_a_remote_exporter(tmp_path):
    environment = {**os.environ, 'AGENTTEAMS_OTEL_ENABLED': 'false', 'AGENTTEAMS_LOG_CAPTURED': '1',
                   'AGENTTEAMS_LOG_NATIVE_EVENTS': '1', 'AGENTTEAMS_OTEL_RUNTIME': 'hermes',
                   'AGENTTEAMS_LOG_DIR': str(tmp_path),
                   'PYTHONPATH': os.pathsep.join(str(Path(__file__).resolve().parents[1] / path)
                                               for path in ['shared/lib', 'shared/lib/telemetry'])}
    result = subprocess.run([sys.executable, '-c',
        'import at_telemetry as t; import os; '
        'assert os.environ["AGENTTEAMS_OTEL_CAPTURE_CONTENT"] == "false"; '
        'assert all(not isinstance(p, t.BatchSpanProcessor) for p in t._provider._active_span_processor._span_processors)'],
        env=environment, capture_output=True, text=True, timeout=15)
    assert result.returncode == 0, result.stderr


def test_team_identity_follows_projected_runtime_across_reassignment(tmp_path, monkeypatch):
    import at_runtime_adapters as adapters
    runtime = tmp_path / 'runtime.yaml'
    monkeypatch.setenv('TEAMHARNESS_RUNTIME_CONFIG', str(runtime))
    monkeypatch.setenv('AGENTTEAMS_TEAM_NAME', 'stale-team')
    runtime.write_text('team:\n  name: first-team\nmember:\n  role: worker\n')
    fields = adapters.enrich_task({}, None)
    assert fields['agentteams.team.id'] == 'first-team'
    runtime.write_text('team:\n  name: second-team\nmember:\n  role: worker\n')
    fields = adapters.enrich_task(fields, None)
    assert fields['agentteams.team.id'] == 'second-team'
    runtime.write_text('member:\n  role: standalone\n')
    assert 'agentteams.team.id' not in adapters.enrich_task(fields, None)


def test_adapter_installation_failure_is_reported_without_breaking_import(monkeypatch):
    import at_runtime_adapters as adapters
    monkeypatch.setattr(adapters, '_adapter_errors', [])
    monkeypatch.setattr(adapters.wrapt, 'register_post_import_hook', lambda callback, name: callback(object()))
    def incompatible(module):
        raise AttributeError('changed framework API')
    adapters.register_hook(incompatible, 'framework.module')
    assert adapters._adapter_errors == ['Telemetry adapter framework.module failed: AttributeError']


def test_agentscope_middleware_import_order_does_not_break_worker_startup(tmp_path):
    package = tmp_path / 'agentscope'
    (package / 'middleware').mkdir(parents=True)
    (package / 'agent').mkdir()
    (package / '__init__.py').write_text('from . import middleware\n')
    (package / 'middleware/__init__.py').write_text('from ._tracing import TracingMiddleware\n')
    (package / 'middleware/_tracing.py').write_text('from ..agent._agent import Agent\nclass TracingMiddleware: pass\n')
    (package / 'agent/__init__.py').write_text('from ._agent import Agent\n')
    (package / 'agent/_agent.py').write_text('class Agent:\n    def __init__(self, middlewares=None): self.middlewares = middlewares\n    async def reply(self): pass\n')
    environment = {**os.environ, 'AGENTTEAMS_OTEL_ENABLED': 'true', 'AGENTTEAMS_OTEL_RUNTIME': 'qwenpaw',
                   'PYTHONPATH': str(Path(__file__).resolve().parents[1] / 'shared/lib/telemetry') + os.pathsep + str(tmp_path)}
    result = subprocess.run([sys.executable, '-c', 'from agentscope.agent import Agent; from agentscope.middleware import TracingMiddleware; assert isinstance(Agent().middlewares[0], TracingMiddleware)'],
                            env=environment, capture_output=True, text=True, timeout=15)
    assert result.returncode == 0, result.stderr


def test_startup_allows_runtime_to_configure_logging():
    environment = {**os.environ, 'AGENTTEAMS_OTEL_ENABLED': 'true', 'AGENTTEAMS_OTEL_RUNTIME': 'qwenpaw',
                   'PYTHONPATH': str(Path(__file__).resolve().parents[1] / 'shared/lib/telemetry')}
    result = subprocess.run([sys.executable, '-c', 'import logging; logging.basicConfig(level=logging.INFO); assert logging.getLogger().level == logging.INFO; import at_telemetry; at_telemetry.capture_logs(); at_telemetry.capture_logs(); assert sum(isinstance(h, at_telemetry.TraceLogHandler) for h in logging.getLogger().handlers) == 1'],
                            env=environment, capture_output=True, text=True, timeout=15)
    assert result.returncode == 0, result.stderr


def test_otlp_auth_credentials_are_redacted_even_when_decoded_from_headers(monkeypatch):
    from urllib.parse import quote
    secret = "sk-private-tracing-project"
    credential = base64.b64encode(("pk-project:" + secret).encode()).decode()
    headers = "Authorization=" + quote("Basic " + credential) + ",x-langfuse-ingestion-version=4"
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_HEADERS", headers)
    source = {"tool_output": f"headers={headers}; decoded=Basic {credential}; secret={secret}",
              "OTEL_EXPORTER_OTLP_TRACES_HEADERS": headers}
    cleaned = json.dumps(telemetry.sanitize(source))
    assert secret not in cleaned
    assert credential not in cleaned
    assert headers not in cleaned
    assert source["OTEL_EXPORTER_OTLP_TRACES_HEADERS"] == headers


def test_sanitize_structured_content_and_boundaries(monkeypatch):
    monkeypatch.setenv("DEMO_LLM_API_KEY", "live-key-for-test-only")
    source = {"messages": [{"content": "result live-key-for-test-only"}],
              "api_key": "credential", "Authorization": "Bearer credential"}
    cleaned = telemetry.sanitize(source)
    assert cleaned["api_key"] == "[REDACTED]"
    assert "live-key-for-test-only" not in json.dumps(cleaned)
    assert source["api_key"] == "credential"
    bounded = telemetry.capture("x" * 300000)
    assert bounded["truncated"] and bounded["original_bytes"] == 300002
    assert len(bounded["sha256"]) == 64


def test_session_identity_and_causal_link_are_worker_scoped(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "one")
    one = telemetry.session_key("matrix:room", user_id="u")
    assert one == telemetry.session_key("matrix:room", user_id="u")
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "two")
    assert one != telemetry.session_key("matrix:room", user_id="u")
    assert one != telemetry.session_key("matrix:room", user_id="other")


def test_async_generator_context_is_detached_before_yield_and_close():
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(telemetry.IdentityProcessor())
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    tracer = provider.get_tracer("test")

    async def source():
        with tracer.start_as_current_span("child"):
            await asyncio.sleep(0)
        yield "first"
        yield "second"

    async def run():
        root = tracer.start_span("turn")
        stream = telemetry.trace_stream(source(), root, {"agentteams.task.id": "task"})
        assert await anext(stream) == "first"
        assert telemetry._correlation.get() == {}
        assert not trace.get_current_span().get_span_context().is_valid
        await asyncio.create_task(stream.aclose())

    asyncio.run(run())
    spans = exporter.get_finished_spans()
    assert {s.name for s in spans} == {"turn", "child"}
    assert next(s for s in spans if s.name == "turn").attributes["agentteams.outcome"] == "cancelled"


def test_returned_tool_errors_are_detected():
    assert telemetry.tool_error('{"error":"file missing"}')
    assert telemetry.tool_error({"exit_code": 2, "stdout": ""})
    assert not telemetry.tool_error({"exit_code": 0, "stdout": "error count: 0"})


def test_runtime_handles_in_request_are_not_serialized_as_json():
    class Request:
        def model_dump(self, mode):
            assert mode == "python"
            return {"input": "hello", "channel_instance": object()}
    assert telemetry.capture(Request())["channel_instance"] == "<object>"


def test_matrix_carrier_links_workers_without_reusing_session(monkeypatch):
    from at_trace_transport import carrier, incoming, remember_reply, reply_context
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "sender")
    provider = TracerProvider()
    span = provider.get_tracer("test").start_span("turn")
    fields = {"agentteams.session.id": "sender-session", "agentteams.task.id": "sender-task",
              "agentteams.team.id": "team"}
    token = telemetry._correlation.set(fields)
    metadata = {"otel_delivery": {}}
    shallow_copy = dict(metadata)
    try:
        remember_reply(span, fields, metadata)
    finally:
        telemetry._correlation.reset(token)
    span.end()
    with reply_context(shallow_copy):
        received = incoming({"com.agentteams.trace": carrier()}, room_id="room", event_id="event")
        assert trace.get_current_span().get_span_context().trace_id == span.get_span_context().trace_id
    with provider.get_tracer("test").start_as_current_span("unrelated transport") as ambient:
        with reply_context(metadata):
            assert trace.get_current_span().get_span_context().trace_id == span.get_span_context().trace_id
        assert trace.get_current_span().get_span_context().trace_id == ambient.get_span_context().trace_id
    assert received["agentteams.upstream.trace_id"] == f"{span.get_span_context().trace_id:032x}"
    assert received["agentteams.upstream.task_id"] == "sender-task"
    assert "agentteams.session.id" not in received
    assert "agentteams.task.id" not in received
    assert telemetry._correlation.get() == {}


def test_export_normalizes_returned_tool_error_and_redacts(monkeypatch):
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(telemetry.SanitizingExporter(exporter)))
    with provider.get_tracer("test").start_as_current_span("tool read", attributes={
        "gen_ai.operation.name": "execute_tool", "gen_ai.tool.call.result": '{"error":"missing file"}',
        "http.request.header.authorization": "credential",
        "langfuse.trace.tags": ["agentteams", "worker-turn"],
    }):
        pass
    recorded = exporter.get_finished_spans()[0]
    assert recorded.status.status_code.name == "ERROR"
    assert recorded.attributes["http.request.header.authorization"] == "[REDACTED]"
    assert list(recorded.attributes["langfuse.trace.tags"]) == ["agentteams", "worker-turn"]
    monkeypatch.setenv("AGENTTEAMS_OTEL_CAPTURE_CONTENT", "false")
    with provider.get_tracer("test").start_as_current_span("chat", attributes={"gen_ai.input.messages": "private"}):
        pass
    assert "gen_ai.input.messages" not in exporter.get_finished_spans()[-1].attributes


def test_agentloop_semantics_preserve_langfuse_io_and_content_controls(monkeypatch):
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(telemetry.SanitizingExporter(exporter)))
    tracer = provider.get_tracer("test")
    monkeypatch.setenv("AGENTLOOP_LICENSE_KEY", "private-license-key")
    attrs = {"agentteams.session.id": "worker:session", "agentteams.turn.id": "turn",
             "agentteams.worker.name": "worker", "langfuse.observation.type": "agent"}
    with tracer.start_as_current_span("worker.turn", attributes=attrs) as span:
        telemetry.set_io(span, input={"messages": [{"role": "user", "content": "hello private-license-key"}]}, output="done")
    root = exporter.get_finished_spans()[-1].attributes
    assert root["gen_ai.span.kind"] == "ENTRY"
    assert root["gen_ai.operation.name"] == "enter"
    assert root["gen_ai.session.id"] == "worker:session" and root["gen_ai.turn.id"] == "turn"
    assert json.loads(root["gen_ai.input.messages"])[0]["parts"][0]["content"] == "hello [REDACTED]"
    assert "langfuse.observation.input" in root
    assert telemetry.genai_messages([{"role": "user", "content": [{"type": "text", "text": "native text"}]}], "user")[0]["parts"] == [{"type": "text", "content": "native text"}]
    assert telemetry.genai_attributes({"gen_ai.system": "openai"}, "chat")["gen_ai.provider.name"] == "openai"
    with tracer.start_as_current_span("tool read", attributes={"gen_ai.operation.name": "execute_tool"}) as span:
        telemetry.set_io(span, input={"path": "demo"}, output={"error": "missing file"})
    tool = exporter.get_finished_spans()[-1]
    assert tool.attributes["gen_ai.span.kind"] == "TOOL"
    assert json.loads(tool.attributes["gen_ai.tool.call.arguments"]) == {"path": "demo"}
    assert tool.status.status_code.name == "ERROR"
    monkeypatch.setenv("AGENTTEAMS_OTEL_CAPTURE_CONTENT", "false")
    with tracer.start_as_current_span("worker.turn", attributes={**attrs, "langfuse.observation.input": "private", "gen_ai.input.messages": "private"}):
        pass
    assert not any(key.endswith((".input", ".messages")) for key in exporter.get_finished_spans()[-1].attributes)


def test_turns_do_not_inherit_long_lived_matrix_consumer_trace(monkeypatch):
    from at_runtime_adapters import start_turn
    provider = TracerProvider()
    tracer = provider.get_tracer("test")
    monkeypatch.setattr(trace, "get_tracer", lambda *args, **kwargs: tracer)
    with tracer.start_as_current_span("matrix.sync") as transport:
        first = start_turn({"agentteams.turn.id": "one"}, "first")
        second = start_turn({"agentteams.turn.id": "two"}, "second")
        assert first.parent is None and second.parent is None
        assert len({s.get_span_context().trace_id for s in (transport, first, second)}) == 3
        first.end(); second.end()


def test_matrix_edit_carrier_survives_normalizing_new_content(monkeypatch):
    from types import SimpleNamespace
    from at_trace_transport import matrix_client
    provider = TracerProvider()
    tracer = provider.get_tracer("test")
    monkeypatch.setattr(trace, "get_tracer", lambda *args, **kwargs: tracer)
    sent = []

    class Client:
        async def room_send(self, room_id, event_type, content):
            sent.append(content)
            return SimpleNamespace(event_id="event")

    module = SimpleNamespace(Client=Client)
    matrix_client(module, "Client.room_send", 2)
    original = {"body": "* update", "m.new_content": {"body": "update"}}
    async def send():
        with tracer.start_as_current_span("turn"):
            await Client().room_send("room", "m.room.message", original)
    asyncio.run(send())
    assert sent[0]["com.agentteams.trace"]["traceparent"]
    assert sent[0]["m.new_content"]["com.agentteams.trace"] == sent[0]["com.agentteams.trace"]
    assert "com.agentteams.trace" not in original


def test_child_propagation_preserves_an_explicit_empty_environment(monkeypatch):
    from types import SimpleNamespace
    from at_trace_transport import subprocesses
    monkeypatch.setenv("PRIVATE_CONTEXT", "must-not-inherit")
    captured = []

    class Popen:
        def __init__(self, command, env=None):
            captured.append(env)

    subprocesses(SimpleNamespace(Popen=Popen))
    with TracerProvider().get_tracer("test").start_as_current_span("tool"):
        Popen(["command"], env={})
    assert "PRIVATE_CONTEXT" not in captured[0]
    assert "AT_OTEL_TRACEPARENT" in captured[0]
    assert "AT_OTEL_TRACEPARENT" not in __import__("os").environ


def test_stream_lifecycle_failure_keeps_exception_stack():
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(telemetry.SanitizingExporter(exporter)))
    span = provider.get_tracer("test").start_span("worker.turn")
    async def source():
        raise ValueError("isolated lifecycle failure")
        yield
    async def run():
        try:
            async for _ in telemetry.trace_stream(source(), span, {}):
                pass
        except ValueError:
            pass
    asyncio.run(run())
    recorded = exporter.get_finished_spans()[0]
    assert recorded.status.status_code.name == "ERROR"
    assert "isolated lifecycle failure" in recorded.events[0].attributes["exception.stacktrace"]
    assert "exception.stacktrace" in recorded.attributes["langfuse.observation.metadata.events"]


def test_subagent_does_not_overwrite_gateway_reply_context():
    from at_trace_transport import _delivery, remember_reply, reply_context
    tracer = TracerProvider().get_tracer("test")
    parent = tracer.start_span("gateway turn")
    token = _delivery.set({})
    try:
        remember_reply(parent, {"agentteams.turn.id": "parent"})
        with tracer.start_as_current_span("child turn") as child:
            fields = {"agentteams.turn.id": "child", "agentteams.parent.turn.id": "parent"}
            remember_reply(child, fields)
            correlation = telemetry._correlation.set(fields)
            try:
                with reply_context():
                    assert trace.get_current_span() is child
            finally:
                telemetry._correlation.reset(correlation)
        with reply_context():
            assert trace.get_current_span().get_span_context().trace_id == parent.get_span_context().trace_id
    finally:
        _delivery.reset(token)
        parent.end()


def test_forwarded_baggage_is_accepted_only_for_this_worker(monkeypatch):
    from opentelemetry import baggage, context
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "local-worker")
    provider = TracerProvider()
    provider.add_span_processor(telemetry.IdentityProcessor())
    for worker in ("local-worker", "another-worker"):
        ctx = baggage.set_baggage("agentteams.worker.name", worker, context=context.Context())
        ctx = baggage.set_baggage("agentteams.task.id", "forwarded-task", context=ctx)
        span = provider.get_tracer("test").start_span("server", context=ctx)
        assert span.attributes["agentteams.worker.name"] == "local-worker"
        assert (span.attributes.get("agentteams.task.id") == "forwarded-task") == (worker == "local-worker")
        span.end()


def test_http_propagates_business_baggage_only_to_local_api(monkeypatch):
    from types import SimpleNamespace
    from at_runtime_adapters import httpx
    tracer = TracerProvider().get_tracer("test")
    monkeypatch.setattr(trace, "get_tracer", lambda *args, **kwargs: tracer)

    class Client:
        def send(self, request):
            return SimpleNamespace(status_code=200, headers={})
    class AsyncClient:
        async def send(self, request):
            return SimpleNamespace(status_code=200, headers={})
    httpx(SimpleNamespace(Client=Client, AsyncClient=AsyncClient))
    correlation = telemetry._correlation.set({"agentteams.turn.id": "turn"})
    try:
        remote = trace.NonRecordingSpan(trace.SpanContext(1, 2, is_remote=True, trace_flags=trace.TraceFlags(1)))
        with trace.use_span(remote):
            for host in ("localhost", "model.example"):
                request = SimpleNamespace(method="POST", url=SimpleNamespace(host=host, path="/api"), headers={})
                Client().send(request)
                assert "traceparent" in request.headers
                assert ("baggage" in request.headers) == (host == "localhost")
    finally:
        telemetry._correlation.reset(correlation)
