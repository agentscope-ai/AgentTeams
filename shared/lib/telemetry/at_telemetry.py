"""Shared OTel adapter for Python Workers. No Langfuse SDK dependency."""

from contextlib import contextmanager
from contextvars import ContextVar
import json
import os

from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.openai import OpenAIInstrumentor
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import SpanProcessor, TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

_correlation = ContextVar("at_trace_correlation", default={})
_provider = None


class IdentityProcessor(SpanProcessor):
    """Put AT identity on spans, including spans from native instrumentation."""

    def on_start(self, span, parent_context=None):
        fields = {
            "agentteams.worker.name": os.getenv("AGENTTEAMS_WORKER_NAME", "unknown"),
            "agentteams.runtime": os.getenv("AGENTTEAMS_OTEL_RUNTIME", "python"),
            **_correlation.get(),
        }
        for key, value in fields.items():
            span.set_attribute(key, value)
            span.set_attribute(f"langfuse.observation.metadata.{key}", value)
        span.set_attribute("langfuse.user.id", fields["agentteams.worker.name"])
        if not span.parent:
            span.set_attribute("langfuse.trace.name", span.name)
            for key, value in fields.items():
                span.set_attribute(f"langfuse.trace.metadata.{key}", value)
            tags = ["agentteams", fields["agentteams.runtime"]]
            if fields.get("agentteams.task.id"):
                tags.append(f"task:{fields['agentteams.task.id']}")
            fixture = span.attributes.get("agentteams.demo.fixture")
            if fixture is not None:
                tags.append("fixture" if fixture else "live")
            span.set_attribute("langfuse.trace.tags", tags)
        room = fields.get("agentteams.room.id")
        if room:
            span.set_attribute("langfuse.session.id", room)


def configure():
    """Initialize once; keep an already configured SDK provider."""
    global _provider
    if _provider is not None:
        return _provider
    provider = trace.get_tracer_provider()
    if not isinstance(provider, TracerProvider):
        resource = Resource.create({
            "service.name": os.getenv("OTEL_SERVICE_NAME", "agentteams-worker"),
            "agentteams.worker.name": os.getenv("AGENTTEAMS_WORKER_NAME", "unknown"),
            "agentteams.runtime": os.getenv("AGENTTEAMS_OTEL_RUNTIME", "python"),
        })
        provider = TracerProvider(resource=resource)
        provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
        trace.set_tracer_provider(provider)
    provider.add_span_processor(IdentityProcessor())
    os.environ.setdefault("TRACELOOP_TRACE_CONTENT", "false")
    OpenAIInstrumentor().instrument()
    _provider = provider
    return provider


@contextmanager
def operation(name, *, task_id="", room_id="", project_id="", attributes=None):
    """Wrap your native turn/tool hook; async tasks keep separate context."""
    fields = dict(_correlation.get())
    for label, value in (("task", task_id), ("room", room_id), ("project", project_id)):
        if value:
            fields[f"agentteams.{label}.id"] = value
    token = _correlation.set(fields)
    try:
        with trace.get_tracer("agentteams.worker").start_as_current_span(
            name, attributes=attributes or {},
        ) as span:
            yield span
    finally:
        _correlation.reset(token)


def set_io(span, *, input=None, output=None):
    """Explicitly record approved content, never implicit prompt capture."""
    for key, value in (("input", input), ("output", output)):
        if value is not None:
            span.set_attribute(f"langfuse.observation.{key}", json.dumps(value, ensure_ascii=False))


def flush():
    return _provider.force_flush(timeout_millis=10000) if _provider else True
