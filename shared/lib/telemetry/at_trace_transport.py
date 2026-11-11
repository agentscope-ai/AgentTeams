"""W3C carriers in Matrix events and child processes; no per-turn global env."""

import os
import copy
import json
import time
from collections.abc import Mapping
from contextlib import contextmanager
from contextvars import ContextVar

import wrapt
from opentelemetry import baggage, context, propagate, trace

import at_telemetry as telemetry

MATRIX_TRACE_KEY = "com.agentteams.trace"
_delivery = ContextVar("at_reply_delivery", default=None)


def remember_reply(span, fields, metadata=None):
    if metadata is None and fields.get("agentteams.parent.turn.id"):
        return  # A delegated agent must not replace the gateway turn's reply.
    with trace.use_span(span, end_on_exit=False):
        token = telemetry._correlation.set(fields)
        try:
            reply = {"carrier": carrier(), "fields": dict(fields)}
        finally:
            telemetry._correlation.reset(token)
    target = metadata.setdefault("otel_delivery", {}) if isinstance(metadata, dict) else _delivery.get()
    if target is not None:
        target["otel_reply"] = reply


@contextmanager
def reply_context(metadata=None):
    if metadata is None and trace.get_current_span().is_recording() and telemetry._correlation.get().get("agentteams.turn.id"):
        yield
        return
    target_metadata = metadata.get("otel_delivery", metadata) if isinstance(metadata, dict) else _delivery.get() or {}
    reply = target_metadata.get("otel_reply")
    current = trace.get_current_span().get_span_context()
    target = propagate.extract(reply["carrier"]) if reply else None
    expected = trace.get_current_span(target).get_span_context() if target is not None else None
    if not reply or (current.is_valid and current.trace_id == expected.trace_id):
        yield
        return
    token = context.attach(target)
    correlation = telemetry._correlation.set(reply["fields"])
    try:
        yield
    finally:
        telemetry._correlation.reset(correlation)
        context.detach(token)


def carrier():
    ctx = context.get_current()
    ctx = baggage.set_baggage("agentteams.worker.name", os.getenv("AGENTTEAMS_WORKER_NAME", "unknown"), context=ctx)
    for key, value in telemetry._correlation.get().items():
        ctx = baggage.set_baggage(key, str(value), context=ctx)
    result = {}
    propagate.inject(result, context=ctx)
    return result


def incoming(content, *, room_id="", event_id=""):
    result = {"agentteams.room.id": str(room_id), "agentteams.event.id": str(event_id)}
    if hasattr(content, "serialize"):
        content = content.serialize()
    wire = content.get(MATRIX_TRACE_KEY, {}) if isinstance(content, Mapping) else {}
    if isinstance(wire, Mapping):
        remote = trace.get_current_span(propagate.extract(dict(wire))).get_span_context()
        if remote.is_valid:
            result["agentteams.upstream.trace_id"] = f"{remote.trace_id:032x}"
            result["agentteams.upstream.span_id"] = f"{remote.span_id:016x}"
        # Only shared work identity crosses Workers; never overwrite local session/turn.
        remote_context = propagate.extract(dict(wire))
        for label in ("task", "team", "project"):
            key = f"agentteams.{label}.id"
            value = baggage.get_baggage(key, context=remote_context)
            if value:
                result["agentteams.upstream.task_id" if label == "task" else key] = str(value)
    return {key: value for key, value in result.items() if value}


def mcp_client(module):
    async def call(wrapped, instance, args, kwargs):
        if trace.get_current_span().get_span_context().is_valid:
            kwargs = {**kwargs, "meta": {**(kwargs.get("meta") or {}), MATRIX_TRACE_KEY: carrier()}}
        return await wrapped(*args, **kwargs)

    wrapt.wrap_function_wrapper(module, "ClientSession.call_tool", call)


@contextmanager
def mcp_request(metadata, name):
    """Restore one call in a long-lived child, without retaining its identity."""
    wire = metadata.get(MATRIX_TRACE_KEY, {}) if isinstance(metadata, Mapping) else {}
    restored = propagate.extract(dict(wire)) if isinstance(wire, Mapping) else context.Context()
    parent = trace.get_current_span(restored).get_span_context()
    forwarded = baggage.get_all(restored)
    if not parent.is_valid or forwarded.get("agentteams.worker.name") != os.getenv("AGENTTEAMS_WORKER_NAME", "unknown"):
        restored = context.Context()
        fields = {}
    else:
        fields = {key: str(value) for key, value in forwarded.items() if key.startswith("agentteams.")}
    token = context.attach(restored)
    correlation = telemetry._correlation.set(fields)
    try:
        if parent.is_valid and fields:
            with telemetry.operation("mcp " + name, attributes={"langfuse.observation.type": "tool", "gen_ai.operation.name": "execute_tool"}) as span:
                yield span
        else:
            yield None
    finally:
        telemetry._correlation.reset(correlation)
        context.detach(token)


def urllib_requests(module):
    def request(wrapped, instance, args, kwargs):
        original = args[0] if args else kwargs.get("url")
        if not isinstance(original, module.Request) or not trace.get_current_span().get_span_context().is_valid:
            return wrapped(*args, **kwargs)
        if original.type not in ("http", "https"):
            return wrapped(*args, **kwargs)
        from urllib.parse import urlsplit
        url = urlsplit(original.full_url)
        matrix_send = "/_matrix/client/" in url.path and "/send/" in url.path
        attributes = {"http.request.method": original.get_method(), "server.address": url.hostname or "", "url.path": url.path}
        with telemetry.operation("matrix.send" if matrix_send else "http.request", attributes=attributes) as span:
            if matrix_send and original.data:
                content = json.loads(original.data)
                content[MATRIX_TRACE_KEY] = carrier()
                updated = copy.copy(original)
                updated.headers = dict(original.headers)
                updated.unredirected_hdrs = dict(original.unredirected_hdrs)
                updated.data = json.dumps(content).encode("utf-8")
                args, kwargs = ((updated, *args[1:]), kwargs) if args else (args, {**kwargs, "url": updated})
                telemetry.set_io(span, input=content)
            response = wrapped(*args, **kwargs)
            span.set_attribute("http.response.status_code", response.status)
            return response
    wrapt.wrap_function_wrapper(module, "urlopen", request)


def matrix_client(module, path, content_index):
    async def send_current(wrapped, instance, args, kwargs):
        if not trace.get_current_span().get_span_context().is_valid:
            return await wrapped(*args, **kwargs)
        room = args[0] if args else kwargs.get("room_id", "")
        with telemetry.operation("matrix.send", attributes={"messaging.system": "matrix", "messaging.destination.name": str(room)}) as span:
            new_args, new_kwargs = list(args), dict(kwargs)
            content = args[content_index] if len(args) > content_index else kwargs.get("content")
            if hasattr(content, "serialize"):
                content = content.serialize()
            if isinstance(content, Mapping):
                content = {**content, MATRIX_TRACE_KEY: carrier()}
                if isinstance(content.get("m.new_content"), Mapping):
                    content["m.new_content"] = {**content["m.new_content"], MATRIX_TRACE_KEY: content[MATRIX_TRACE_KEY]}
                if len(args) > content_index:
                    new_args[content_index] = content
                else:
                    new_kwargs["content"] = content
                telemetry.set_io(span, input=content)
            result = await wrapped(*new_args, **new_kwargs)
            event_id = getattr(result, "event_id", result if isinstance(result, str) else "")
            span.set_attribute("messaging.message.id", str(event_id))
            telemetry.set_io(span, output={"event_id": str(event_id)})
            return result

    async def send(wrapped, instance, args, kwargs):
        with reply_context():
            return await send_current(wrapped, instance, args, kwargs)

    wrapt.wrap_function_wrapper(module, path, send)


def hermes_gateway(module):
    async def receive(wrapped, instance, args, kwargs):
        event = args[0] if args else kwargs["event"]
        raw = event.raw_message
        content = getattr(raw, "content", raw.get("content", raw) if isinstance(raw, dict) else {})
        fields = {**telemetry._correlation.get(), **incoming(content, room_id=event.source.chat_id, event_id=event.message_id)}
        fields["agentteams.message.received_at_ns"] = time.time_ns()
        token = telemetry._correlation.set(fields)
        delivery_token = _delivery.set({})
        try:
            # Native gateway creates its tasks here, copying ContextVars.
            return await wrapped(*args, **kwargs)
        finally:
            _delivery.reset(delivery_token)
            telemetry._correlation.reset(token)

    wrapt.wrap_function_wrapper(module, "BasePlatformAdapter.handle_message", receive)


def subprocesses(module):
    def launch(wrapped, instance, args, kwargs):
        if trace.get_current_span().get_span_context().is_valid:
            source = args[10] if len(args) > 10 else kwargs.get("env")
            env = dict(os.environ if source is None else source)
            for key, value in carrier().items():
                env[f"AT_OTEL_{key.upper()}"] = value
            if len(args) > 10:
                args = (*args[:10], env, *args[11:])
            else:
                kwargs = {**kwargs, "env": env}
        return wrapped(*args, **kwargs)

    wrapt.wrap_function_wrapper(module, "Popen.__init__", launch)


def install():
    ctx = propagate.extract({key: os.getenv(f"AT_OTEL_{key.upper()}", "") for key in ("traceparent", "tracestate", "baggage")})
    if trace.get_current_span(ctx).get_span_context().is_valid:
        context.attach(ctx)
        telemetry._correlation.set({k: str(v) for k, v in baggage.get_all(ctx).items() if k.startswith("agentteams.")})
    wrapt.register_post_import_hook(lambda m: matrix_client(m, "AsyncClient.room_send", 2), "nio")
    wrapt.register_post_import_hook(lambda m: matrix_client(m, "Client.send_message_event", 2), "mautrix.client")
    wrapt.register_post_import_hook(hermes_gateway, "gateway.platforms.base")
    wrapt.register_post_import_hook(subprocesses, "subprocess")
    wrapt.register_post_import_hook(mcp_client, "mcp.client.session")
    wrapt.register_post_import_hook(urllib_requests, "urllib.request")
