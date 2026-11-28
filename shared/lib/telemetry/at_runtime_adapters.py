"""Small version-tested adapters; runtime execution stays in native code."""

import importlib
import importlib.metadata
import logging
import os
from contextlib import contextmanager
from functools import lru_cache
from pathlib import Path

import wrapt
from opentelemetry import context, trace

import at_telemetry as telemetry
_adapter_errors = []
_runtime_packages = ("openai",)


@lru_cache(maxsize=1)
def task_resolver():
    # Reuse TeamHarness's canonical matching rules, including ambiguity handling.
    try:
        from at_task_trace import find_task_trace_context
        return find_task_trace_context
    except ImportError:
        return None


def enrich_task(fields, workspace):
    fields = dict(fields)
    team = os.getenv("AGENTTEAMS_TEAM_NAME")
    runtime_path = os.getenv("TEAMHARNESS_RUNTIME_CONFIG")
    if runtime_path:
        # Team membership is hot-updated in runtime.yaml, not container env.
        import yaml
        try:
            runtime = yaml.safe_load(Path(runtime_path).read_text()) or {}
            team = (runtime.get("team") or {}).get("name")
            fields.pop("agentteams.team.id", None)
        except (OSError, yaml.YAMLError):
            pass
    if team:
        fields["agentteams.team.id"] = team
    if workspace and not fields.get("agentteams.task.id"):
        resolver = task_resolver()
        found = resolver(Path(workspace), room_id=fields.get("agentteams.room.id", "")) if resolver else {}
        fields["agentteams.task.correlation"] = found.get("reason") or ("unique active task" if found else "task resolver unavailable")
        if found.get("project_id"):
            fields["agentteams.project.id"] = found["project_id"]
        task = found.get("task") or {}
        if task.get("task_id") or task.get("taskId"):
            fields["agentteams.task.id"] = task.get("task_id") or task["taskId"]
            for source, label in (("notifyTxn", "dispatch_id"), ("submission_id", "submission_id")):
                if task.get(source):
                    fields[f"agentteams.task.{label}"] = str(task[source])
    elif fields.get("agentteams.task.id"):
        fields["agentteams.task.correlation"] = "explicit request context"
    return fields


@contextmanager
def bind(fields):
    token = telemetry._correlation.set(fields)
    try:
        yield
    finally:
        telemetry._correlation.reset(token)


def start_turn(fields, request):
    telemetry.capture_logs()
    links = []
    upstream = fields.get("agentteams.upstream.trace_id")
    if upstream:
        links.append(trace.Link(trace.SpanContext(
            trace_id=int(upstream, 16), span_id=int(fields["agentteams.upstream.span_id"], 16),
            is_remote=True, trace_flags=trace.TraceFlags(1),
        )))
    with bind(fields):
        versions = {}
        for package in _runtime_packages:
            try:
                versions[package] = importlib.metadata.version(package)
            except importlib.metadata.PackageNotFoundError:
                pass
        span = trace.get_tracer("agentteams.worker").start_span("worker.turn", attributes={
            "langfuse.observation.type": "agent",
            "langfuse.trace.name": f"{os.getenv('AGENTTEAMS_WORKER_NAME', 'worker')} / turn",
            "agentteams.telemetry.adapter": os.getenv("AGENTTEAMS_OTEL_RUNTIME", "python"),
            "agentteams.telemetry.versions": str(versions),
            "agentteams.telemetry.capture_content": os.getenv("AGENTTEAMS_OTEL_CAPTURE_CONTENT", "true"),
            "agentteams.telemetry.field_limit_bytes": 256 * 1024,
            "agentteams.telemetry.adapter_errors": str(_adapter_errors),
            "agentteams.telemetry.coverage": "turn,model,tool,http_attempt,matrix_send,warning,error,cancel",
        }, context=context.Context(), links=links)
    telemetry.set_io(span, input=request)
    return span


def httpx(module):
    def attrs(request):
        return {"http.request.method": request.method, "server.address": request.url.host,
                "url.path": request.url.path, "agentteams.phase": "http.attempt"}

    def inject(request):
        from at_trace_transport import carrier
        # Keep business baggage on the Worker's own API, not model providers.
        local = request.url.host in ("localhost", "127.0.0.1", "::1")
        request.headers.update({key: value for key, value in carrier().items() if local or key != "baggage"})

    def send(wrapped, instance, args, kwargs):
        if not trace.get_current_span().get_span_context().is_valid:
            return wrapped(*args, **kwargs)
        request = args[0] if args else kwargs["request"]
        with telemetry.operation("http.request", attributes=attrs(request)) as span:
            inject(request)
            response = wrapped(*args, **kwargs)
            span.set_attribute("http.response.status_code", response.status_code)
            span.set_attribute("http.response.request_id", response.headers.get("x-request-id", ""))
            if response.status_code >= 400:
                telemetry.outcome(span, error=RuntimeError(f"HTTP {response.status_code}"))
            return response

    async def async_send(wrapped, instance, args, kwargs):
        if not trace.get_current_span().get_span_context().is_valid:
            return await wrapped(*args, **kwargs)
        request = args[0] if args else kwargs["request"]
        with telemetry.operation("http.request", attributes=attrs(request)) as span:
            inject(request)
            response = await wrapped(*args, **kwargs)
            span.set_attribute("http.response.status_code", response.status_code)
            span.set_attribute("http.response.request_id", response.headers.get("x-request-id", ""))
            if response.status_code >= 400:
                telemetry.outcome(span, error=RuntimeError(f"HTTP {response.status_code}"))
            return response

    wrapt.wrap_function_wrapper(module, "Client.send", send)
    wrapt.wrap_function_wrapper(module, "AsyncClient.send", async_send)


def register_hook(callback, name):
    def ready(module):
        try:
            callback(module)
        except Exception as error:
            message = f"Telemetry adapter {name} failed: {type(error).__name__}"
            _adapter_errors.append(message)
            logging.getLogger("agentteams.telemetry").error(message)
    wrapt.register_post_import_hook(ready, name)


def install():
    global _runtime_packages
    try:
        native = importlib.import_module("at_native_telemetry")
    except ModuleNotFoundError as exc:
        if exc.name != "at_native_telemetry":
            raise
        native = None
    if native is not None:
        _runtime_packages = native.PACKAGES
        native.install()
    if native is None or native.INSTRUMENT_OPENAI:
        def instrument_openai(_module):
            from opentelemetry.instrumentation.openai import OpenAIInstrumentor
            OpenAIInstrumentor().instrument()
        register_hook(instrument_openai, "openai")
    register_hook(httpx, "httpx")
    from at_trace_transport import install as install_transport
    install_transport()
