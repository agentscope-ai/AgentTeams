"""Shared OTel adapter for Python Workers. No Langfuse SDK dependency."""

from contextlib import contextmanager
from contextvars import ContextVar
import asyncio
import base64
import hashlib
import json
import logging
import os
import re
import time
import uuid
from urllib.parse import unquote

from opentelemetry import baggage, context, trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.openai import OpenAIInstrumentor
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import Event, ReadableSpan, SpanLimits, SpanProcessor, TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SpanExporter

_correlation = ContextVar("at_trace_correlation", default={})
_active_turn = ContextVar("at_active_turn", default=None)
_model_request = ContextVar("at_model_request", default=None)
_provider = None
_secret_key = re.compile(r"(?:api[_-]?key|secret|password|authorization|access[_-]?token|gateway[_-]?key|credential|license[_-]?key|security[_-]?token|OTEL_EXPORTER_OTLP(?:_TRACES)?_HEADERS)", re.I)
_limit = 256 * 1024


def sanitize(value):
    """Redact credentials without changing the original runtime object."""
    if isinstance(value, dict):
        return {str(k): "[REDACTED]" if _secret_key.search(str(k)) else sanitize(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [sanitize(item) for item in value]
    if hasattr(value, "model_dump"):
        return sanitize(value.model_dump(mode="python"))
    if isinstance(value, str):
        try:
            decoded = json.loads(value)
        except (ValueError, TypeError):
            decoded = None
        if isinstance(decoded, (dict, list)):
            return json.dumps(sanitize(decoded), ensure_ascii=False, default=str)
        for key, secret in os.environ.items():
            if _secret_key.search(key) and len(secret) >= 8:
                value = value.replace(secret, "[REDACTED]")
                if key in ("OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS"):
                    for header in secret.split(","):
                        name, _, encoded = header.partition("=")
                        if not _secret_key.search(name):
                            continue
                        decoded = unquote(encoded)
                        credentials = [decoded, decoded.partition(" ")[2]]
                        if decoded.lower().startswith("basic "):
                            try:
                                credentials.append(base64.b64decode(decoded[6:]).decode().partition(":")[2])
                            except (ValueError, UnicodeError):
                                pass
                        for credential in credentials:
                            if len(credential) >= 8:
                                value = value.replace(credential, "[REDACTED]")
        value = re.sub(r"(?i)Bearer\s+[^\s\"',]+", "Bearer [REDACTED]", value)
        return re.sub(r"(?i)((?:api[_-]?key|password|secret|access_token)\s*[=:]\s*)[^\s\"',;&]+", r"\1[REDACTED]", value)
    if value is None or isinstance(value, (int, float, bool)):
        return value
    return f"<{type(value).__name__}>"


def capture(value):
    value = sanitize(value)
    encoded = json.dumps(value, ensure_ascii=False, default=str).encode()
    if len(encoded) <= _limit:
        return value
    return {"truncated": True, "original_bytes": len(encoded),
            "sha256": hashlib.sha256(encoded).hexdigest(),
            "preview": encoded[:_limit].decode("utf-8", errors="ignore")}


def genai_messages(value, role):
    """Keep native message parts; normalize text/chat SDK payloads for GenAI backends."""
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except ValueError:
            pass
    if isinstance(value, dict):
        value = value.get("messages", value.get("input", value))
    messages = value if isinstance(value, list) else [value]
    result = []
    for message in messages:
        if isinstance(message, dict) and isinstance(message.get("parts"), list):
            result.append(message)
        else:
            content = message.get("content", message) if isinstance(message, dict) else message
            parts = content if isinstance(content, list) else [{"type": "text", "content": content if isinstance(content, str) else json.dumps(content, ensure_ascii=False)}]
            parts = [{"type": "text", "content": part["text"]} if isinstance(part, dict) and part.get("type") == "text" and "text" in part and "content" not in part else part for part in parts]
            result.append({"role": message.get("role", role) if isinstance(message, dict) else role, "parts": parts})
    return result


def genai_attributes(attrs, name):
    """Add backend-neutral semantics once, retaining the Langfuse compatibility fields."""
    attrs = dict(attrs)
    kind = str(attrs.get("langfuse.observation.type", "")).lower()
    operation_name = attrs.get("gen_ai.operation.name")
    provider = attrs.get("gen_ai.provider.name") or attrs.get("gen_ai.system")
    if provider:
        attrs.setdefault("gen_ai.provider.name", provider)
        attrs.setdefault("gen_ai.system", provider)
    if name == "worker.turn":
        attrs.update({"gen_ai.span.kind": "ENTRY", "gen_ai.operation.name": "enter"})
    elif kind == "generation" or operation_name == "chat":
        attrs.setdefault("gen_ai.span.kind", "LLM")
        attrs.setdefault("gen_ai.operation.name", "chat")
    elif kind == "tool" or operation_name == "execute_tool":
        attrs.setdefault("gen_ai.span.kind", "TOOL")
        attrs.setdefault("gen_ai.operation.name", "execute_tool")
    elif kind == "agent" or operation_name == "invoke_agent":
        attrs.setdefault("gen_ai.span.kind", "AGENT")
        attrs.setdefault("gen_ai.operation.name", "invoke_agent")
    for source, target in (("agentteams.session.id", "gen_ai.session.id"), ("agentteams.turn.id", "gen_ai.turn.id"),
                           ("agentteams.worker.name", "gen_ai.agent.id"), ("agentteams.worker.name", "gen_ai.agent.name")):
        if source in attrs:
            attrs.setdefault(target, attrs[source])
    if attrs.get("gen_ai.span.kind") == "TOOL":
        for label, target in (("input", "gen_ai.tool.call.arguments"), ("output", "gen_ai.tool.call.result")):
            if f"langfuse.observation.{label}" in attrs:
                attrs.setdefault(target, attrs[f"langfuse.observation.{label}"])
    elif attrs.get("gen_ai.span.kind") in ("ENTRY", "AGENT", "LLM"):
        for label, role in (("input", "user"), ("output", "assistant")):
            if f"langfuse.observation.{label}" in attrs:
                attrs.setdefault(f"gen_ai.{label}.messages", json.dumps(genai_messages(attrs[f"langfuse.observation.{label}"], role), ensure_ascii=False))
    return attrs


class SanitizingExporter(SpanExporter):
    """Normalize native semantic conventions and redact before leaving the process."""

    def __init__(self, delegate):
        self.delegate = delegate

    def export(self, spans):
        clean = []
        for span in spans:
            attrs = genai_attributes(span.attributes or {}, span.name)
            status = span.status
            if span.events:
                attrs["langfuse.observation.metadata.events"] = json.dumps([
                    {"name": event.name, "timestamp": event.timestamp, "attributes": dict(event.attributes)}
                    for event in span.events], ensure_ascii=False, default=str)
            operation_name = attrs.get("gen_ai.operation.name")
            if operation_name == "chat":
                attrs["langfuse.observation.type"] = "generation"
            elif operation_name == "execute_tool":
                attrs["langfuse.observation.type"] = "tool"
                error = tool_error(attrs.get("gen_ai.tool.call.result"))
                if error:
                    status = trace.Status(trace.StatusCode.ERROR, str(sanitize(error)))
                    attrs["agentteams.outcome"] = "error"
                    attrs["langfuse.observation.level"] = "ERROR"
                    attrs["langfuse.observation.status_message"] = str(sanitize(error))
            elif operation_name == "invoke_agent":
                attrs["langfuse.observation.type"] = "agent"
            for dest, sources in {
                "langfuse.observation.input": ("gen_ai.input.messages", "gen_ai.tool.call.arguments"),
                "langfuse.observation.output": ("gen_ai.output.messages", "gen_ai.tool.call.result"),
            }.items():
                if dest not in attrs:
                    for source in sources:
                        if source in attrs:
                            attrs[dest] = attrs[source]
                            break
            for key, value in list(attrs.items()):
                if key.startswith("agentteams."):
                    attrs.setdefault(f"langfuse.observation.metadata.{key}", value)
                    if span.name == "worker.turn":
                        attrs.setdefault(f"langfuse.trace.metadata.{key}", value)
            if os.getenv("AGENTTEAMS_OTEL_CAPTURE_CONTENT", "true").lower() not in ("true", "1"):
                attrs = {k: v for k, v in attrs.items() if not k.startswith((
                    "langfuse.observation.input", "langfuse.observation.output", "gen_ai.input", "gen_ai.output",
                    "gen_ai.prompt", "gen_ai.completion", "gen_ai.tool.call.arguments", "gen_ai.tool.call.result"))}
            sanitized = {key: "[REDACTED]" if _secret_key.search(key) else capture(value) for key, value in attrs.items()}
            attrs = {key: value if isinstance(value, (str, int, float, bool)) or
                     (isinstance(value, list) and all(isinstance(v, (str, int, float, bool)) for v in value)) else
                     json.dumps(value, ensure_ascii=False) for key, value in sanitized.items()}
            clean.append(ReadableSpan(
                name=sanitize(span.name), context=span.context, parent=span.parent, resource=span.resource,
                attributes=attrs, events=[Event(e.name, {k: sanitize(v) for k, v in e.attributes.items()}, e.timestamp) for e in span.events],
                links=span.links, kind=span.kind, status=trace.Status(status.status_code, sanitize(status.description)),
                start_time=span.start_time, end_time=span.end_time, instrumentation_scope=span.instrumentation_scope,
            ))
        return self.delegate.export(clean)

    def shutdown(self):
        return self.delegate.shutdown()

    def force_flush(self, timeout_millis=30000):
        return self.delegate.force_flush(timeout_millis)


def session_key(session_id, *, user_id=""):
    worker = os.getenv("AGENTTEAMS_WORKER_NAME", "unknown")
    runtime = os.getenv("AGENTTEAMS_OTEL_RUNTIME", "python")
    digest = hashlib.sha256(json.dumps([worker, runtime, session_id, user_id]).encode()).hexdigest()[:24]
    return f"{worker}:{digest}"


def turn_fields(session_id, *, user_id="", metadata=None):
    fields = dict(_correlation.get())
    parent_span = trace.get_current_span()
    if fields.get("agentteams.turn.id") and parent_span.is_recording():
        parent = parent_span.get_span_context()
        fields.update({"agentteams.parent.turn.id": fields["agentteams.turn.id"],
                       "agentteams.parent.session.id": fields.get("agentteams.session.id", ""),
                       "agentteams.upstream.trace_id": f"{parent.trace_id:032x}",
                       "agentteams.upstream.span_id": f"{parent.span_id:016x}"})
    fields.update({"agentteams.session.id": session_key(session_id, user_id=user_id),
                   "agentteams.runtime.session_id": str(session_id), "agentteams.turn.id": uuid.uuid4().hex})
    if (metadata or {}).get("send_gate_retrigger"):
        fields["agentteams.turn.trigger"] = "reply_revalidation"
    for key, value in (metadata or {}).items():
        if key == "otel_received_at_ns" and isinstance(value, int):
            fields["agentteams.message.received_at_ns"] = value
        if key in ("room_id", "task_id", "team_id", "project_id", "event_id") and value:
            fields[f"agentteams.{key[:-3] if key.endswith('_id') else key}.id"] = str(value)
    received = fields.get("agentteams.message.received_at_ns")
    if received:
        fields["agentteams.message.dispatch_delay_ms"] = max(0, (time.time_ns() - int(received)) / 1_000_000)
    return fields


def tool_error(result):
    if isinstance(result, str):
        try:
            result = json.loads(result)
        except ValueError:
            return ""  # Free-form text containing 'error' is not proof of failure.
    if hasattr(result, "model_dump"):
        result = result.model_dump(mode="json")
    if isinstance(result, dict):
        if result.get("error") or result.get("failed") or result.get("isError"):
            return str(result.get("error") or result.get("failed") or "tool error")[:2000]
        if result.get("exit_code", 0) not in (0, None):
            return f"exit_code={result['exit_code']}"
        if str(result.get("state", "")).lower() in ("error", "failed"):
            return "tool returned error state"
    return ""


def outcome(span, result=None, error=None):
    reason = str(error) if error else tool_error(result)
    cancelled = isinstance(error, (asyncio.CancelledError, GeneratorExit))
    span.set_attribute("agentteams.outcome", "cancelled" if cancelled else "error" if reason else "ok")
    if reason or cancelled:
        span.set_status(trace.Status(trace.StatusCode.ERROR, str(sanitize(reason or "cancelled"))))
        span.set_attribute("langfuse.observation.level", "ERROR")
        span.set_attribute("langfuse.observation.status_message", str(sanitize(reason or "cancelled")))


async def trace_stream(generator, span, fields):
    """Attach only while advancing/closing: consumers may close on another task."""
    last = None
    error = None
    span_context = trace.set_span_in_context(span)
    try:
        while True:
            token = context.attach(span_context)
            correlation_token = _correlation.set(fields)
            turn_token = _active_turn.set(span) if span.name == "worker.turn" else None
            try:
                last = await anext(generator)
            except StopAsyncIteration:
                break
            finally:
                if turn_token is not None:
                    _active_turn.reset(turn_token)
                _correlation.reset(correlation_token)
                context.detach(token)
            yield last
    except BaseException as exc:
        error = exc
        if not isinstance(exc, (asyncio.CancelledError, GeneratorExit)):
            span.record_exception(exc)
        raise
    finally:
        token = context.attach(span_context)
        correlation_token = _correlation.set(fields)
        try:
            await generator.aclose()
        finally:
            _correlation.reset(correlation_token)
            context.detach(token)
            set_io(span, output=last)
            outcome(span, last, error)
            span.end()


class IdentityProcessor(SpanProcessor):
    """Put AT identity on spans, including spans from native instrumentation."""

    def on_start(self, span, parent_context=None):
        request = _model_request.get()
        if request and (span.attributes.get("gen_ai.operation.name") == "chat" or span.name == "openai.chat"):
            set_io(span, input=request)
        forwarded = baggage.get_all(parent_context)
        worker = os.getenv("AGENTTEAMS_WORKER_NAME", "unknown")
        forwarded = {key: str(value) for key, value in forwarded.items() if key.startswith("agentteams.")} if forwarded.get("agentteams.worker.name") == worker else {}
        fields = {
            **forwarded,
            **_correlation.get(),
            "agentteams.worker.name": worker,
            "agentteams.runtime": os.getenv("AGENTTEAMS_OTEL_RUNTIME", "python"),
            "agentteams.telemetry.schema": "1",
        }
        for key, value in fields.items():
            span.set_attribute(key, value)
            span.set_attribute(f"langfuse.observation.metadata.{key}", value)
        span.set_attribute("langfuse.user.id", fields["agentteams.worker.name"])
        tags = ["agentteams", fields["agentteams.runtime"]]
        if fields.get("agentteams.turn.id"):
            tags.append("worker-turn")
        for label in ("task", "team", "room", "project"):
            if fields.get(f"agentteams.{label}.id"):
                tags.append(f"{label}:{fields[f'agentteams.{label}.id']}")
        span.set_attribute("langfuse.trace.tags", tags)
        for key, value in fields.items():
            span.set_attribute(f"langfuse.trace.metadata.{key}", value)
        if not span.parent and fields.get("agentteams.turn.id"):
            if "langfuse.trace.name" not in span.attributes:
                span.set_attribute("langfuse.trace.name", span.name)
            for key, value in fields.items():
                span.set_attribute(f"langfuse.trace.metadata.{key}", value)
            fixture = span.attributes.get("agentteams.demo.fixture")
            if fixture is not None:
                tags.append("fixture" if fixture else "live")
            span.set_attribute("langfuse.trace.tags", tags)
        session = fields.get("agentteams.session.id")
        if session:
            span.set_attribute("langfuse.session.id", session)


class RuntimeLogProcessor(SpanProcessor):
    """Record operation metadata locally, without prompts or tool arguments."""

    @staticmethod
    def phase(span):
        attrs = span.attributes or {}
        if span.name == "worker.turn":
            return "turn"
        if attrs.get("gen_ai.operation.name") == "execute_tool" or attrs.get("langfuse.observation.type") == "tool":
            return "tool"
        if attrs.get("gen_ai.operation.name") == "chat" or attrs.get("langfuse.observation.type") == "generation" or span.name == "openai.chat":
            return "model"
        if span.name == "http.request":
            return "http"

    def record(self, span, suffix):
        from runtime_observability import event
        phase = self.phase(span)
        if not phase:
            return
        attrs = span.attributes or {}
        fields = {key: attrs[source] for key, source in {
            "room_id": "agentteams.room.id", "event_id": "agentteams.event.id",
            "turn_id": "agentteams.turn.id", "tool_name": "gen_ai.tool.name",
            "item_id": "gen_ai.tool.call.id", "model": "gen_ai.request.model",
            "http_status": "http.response.status_code", "server": "server.address",
        }.items() if source in attrs}
        fields["span_id"] = f"{span.context.span_id:016x}"
        if suffix == "end":
            fields["duration_ms"] = round((span.end_time - span.start_time) / 1_000_000)
            fields["status"] = "failed" if span.status.status_code == trace.StatusCode.ERROR else "completed"
            if span.status.description:
                fields["error"] = span.status.description[:4096]
        event(f"{phase}.{suffix}", **fields)

    def on_start(self, span, parent_context=None):
        self.record(span, "begin")

    def on_end(self, span):
        self.record(span, "end")


def configure(*, local_only=False):
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
            "agentteams.telemetry.config_revision": os.getenv("AGENTTEAMS_OTEL_CONFIG_REVISION", ""),
        })
        provider = TracerProvider(resource=resource, span_limits=SpanLimits(max_attributes=4096))
        if not local_only:
            provider.add_span_processor(BatchSpanProcessor(SanitizingExporter(OTLPSpanExporter())))
        trace.set_tracer_provider(provider)
    provider.add_span_processor(IdentityProcessor())
    if os.getenv("AGENTTEAMS_LOG_CAPTURED") == "1":
        provider.add_span_processor(RuntimeLogProcessor())
    os.environ["TRACELOOP_TRACE_CONTENT"] = os.getenv("AGENTTEAMS_OTEL_CAPTURE_CONTENT", "true")
    if os.getenv("AGENTTEAMS_OTEL_RUNTIME") != "qwenpaw":
        OpenAIInstrumentor().instrument()
    from at_runtime_adapters import install
    install()
    _provider = provider
    return provider


@contextmanager
def operation(name, *, task_id="", room_id="", project_id="", attributes=None):
    """Wrap your native turn/tool hook; async tasks keep separate context."""
    capture_logs()
    fields = dict(_correlation.get())
    for label, value in (("task", task_id), ("room", room_id), ("project", project_id)):
        if value:
            fields[f"agentteams.{label}.id"] = value
    if room_id and not fields.get("agentteams.session.id"):
        fields["agentteams.session.id"] = session_key(room_id)
    token = _correlation.set(fields)
    try:
        with trace.get_tracer("agentteams.worker").start_as_current_span(
            name, attributes=attributes or {},
        ) as span:
            yield span
    finally:
        _correlation.reset(token)


def set_io(span, *, input=None, output=None):
    """Record approved content; telemetry serialization must never fail the turn."""
    for key, value in (("input", input), ("output", output)):
        if value is not None:
            if os.getenv("AGENTTEAMS_OTEL_CAPTURE_CONTENT", "true").lower() in ("true", "1"):
                try:
                    span.set_attribute(f"langfuse.observation.{key}", json.dumps(capture(value), ensure_ascii=False, default=str))
                except Exception as exc:
                    span.set_attribute(f"agentteams.telemetry.{key}.capture_error", type(exc).__name__)


def flush():
    return _provider.force_flush(timeout_millis=10000) if _provider else True


def capture_logs():
    # Register after runtime startup so logging.basicConfig still configures
    # the application's own output and level.
    root = logging.getLogger()
    if not any(isinstance(handler, TraceLogHandler) for handler in root.handlers):
        root.addHandler(TraceLogHandler())


class TraceLogHandler(logging.Handler):
    """Keep warnings/errors with their active operation; never recurse on exporter logs."""

    def __init__(self):
        super().__init__(logging.WARNING)

    def emit(self, record):
        if record.name.startswith(("opentelemetry", "agentteams.telemetry")):
            return
        span = trace.get_current_span()
        if span.is_recording():
            span.add_event("log", {"log.severity": record.levelname, "log.logger": record.name,
                                   "log.message": json.dumps(capture(record.getMessage()), ensure_ascii=False)})
