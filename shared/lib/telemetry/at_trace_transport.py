"""W3C carriers in Matrix events and child processes; no per-turn global env."""

import os
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
