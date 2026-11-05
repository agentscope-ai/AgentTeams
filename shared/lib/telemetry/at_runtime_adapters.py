"""Small version-tested adapters; runtime execution stays in native code."""

import inspect
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
    if team:
        fields.setdefault("agentteams.team.id", team)
    if workspace and not fields.get("agentteams.task.id"):
        resolver = task_resolver()
        found = resolver(Path(workspace), room_id=fields.get("agentteams.room.id", "")) if resolver else {}
        fields["agentteams.task.correlation"] = found.get("reason") or ("unique active task" if found else "task resolver unavailable")
        if found.get("project_id"):
            fields["agentteams.project.id"] = found["project_id"]
        task = found.get("task") or {}
        if task.get("task_id") or task.get("taskId"):
            fields["agentteams.task.id"] = task.get("task_id") or task["taskId"]
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
    links = []
    upstream = fields.get("agentteams.upstream.trace_id")
    if upstream:
        links.append(trace.Link(trace.SpanContext(
            trace_id=int(upstream, 16), span_id=int(fields["agentteams.upstream.span_id"], 16),
            is_remote=True, trace_flags=trace.TraceFlags(1),
        )))
    with bind(fields):
        versions = {}
        for package in ("agentscope", "qwenpaw", "hermes-agent", "openai"):
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


def qwenpaw_runtime(module):
    async def run(wrapped, instance, args, kwargs):
        request = args[0] if args else kwargs["request"]
        fields = telemetry.turn_fields(getattr(request, "session_id", "") or "unassigned",
                                      user_id=getattr(request, "user_id", ""),
                                      metadata=getattr(request, "channel_meta", {}) or {})
        from at_trace_transport import incoming
        fields.update(incoming({"com.agentteams.trace": (getattr(request, "channel_meta", {}) or {}).get("otel_carrier", {})}))
        fields = enrich_task(fields, getattr(instance.workspace, "workspace_dir", None))
        span = start_turn(fields, request)
        from at_trace_transport import remember_reply
        remember_reply(span, fields, getattr(request, "channel_meta", None))
        async for item in telemetry.trace_stream(wrapped(*args, **kwargs), span, fields):
            yield item

    wrapt.wrap_function_wrapper(module, "Runtime.run", run)


def agentscope(module):
    from agentscope.middleware import TracingMiddleware

    class WorkerTracingMiddleware(TracingMiddleware):
        async def on_acting(self, agent, input_kwargs, next_handler):
            call = input_kwargs.get("tool_call")
            fields = dict(telemetry._correlation.get())
            span = trace.get_tracer("agentteams.worker").start_span(f"tool {getattr(call, 'name', 'unknown')}", attributes={
                "gen_ai.operation.name": "execute_tool", "langfuse.observation.type": "tool",
                "gen_ai.tool.name": getattr(call, "name", "unknown"),
                "gen_ai.tool.call.id": getattr(call, "id", ""),
            })
            telemetry.set_io(span, input=getattr(call, "input", None))
            async for item in telemetry.trace_stream(next_handler(**input_kwargs), span, fields):
                yield item

        async def on_check_permission(self, agent, input_kwargs, next_handler):
            with telemetry.operation("tool.permission") as span:
                telemetry.set_io(span, input=input_kwargs.get("tool_call"))
                result = await next_handler(**input_kwargs)
                telemetry.set_io(span, output=result)
                if str(getattr(result, "behavior", "")).lower() == "deny":
                    telemetry.outcome(span, error=RuntimeError("permission denied"))
                return result

        async def on_compress_context(self, agent, input_kwargs, next_handler):
            with telemetry.operation("context.compress") as span:
                telemetry.set_io(span, input=input_kwargs)
                result = await next_handler(**input_kwargs)
                telemetry.set_io(span, output=result)
                return result

    def initialize(wrapped, instance, args, kwargs):
        # AgentScope's public middleware API captures model context, tools,
        # tool-call IDs, usage, agent state and errors without SDK double counting.
        middlewares = list((args[4] if len(args) > 4 else kwargs.get("middlewares")) or [])
        if not any(isinstance(m, TracingMiddleware) for m in middlewares):
            middlewares.insert(0, WorkerTracingMiddleware())
        if len(args) > 4:
            args = (*args[:4], middlewares, *args[5:])
        else:
            kwargs["middlewares"] = middlewares
        return wrapped(*args, **kwargs)

    async def reply(wrapped, instance, args, kwargs):
        if telemetry._correlation.get().get("agentteams.turn.id"):
            return await wrapped(*args, **kwargs)
        rc = getattr(instance, "_request_context", {}) or {}
        session_id = rc.get("session_id") or getattr(instance.state, "session_id", "")
        fields = telemetry.turn_fields(session_id, user_id=rc.get("user_id", ""), metadata=rc.get("channel_meta", {}))
        fields = enrich_task(fields, rc.get("workspace_dir") or getattr(instance, "workspace_dir", None))
        with bind(fields):
            with trace.use_span(start_turn(fields, args or kwargs), end_on_exit=True) as span:
                try:
                    result = await wrapped(*args, **kwargs)
                    telemetry.set_io(span, output=result)
                    telemetry.outcome(span, result)
                    return result
                except BaseException as exc:
                    telemetry.outcome(span, error=exc)
                    raise

    wrapt.wrap_function_wrapper(module, "Agent.__init__", initialize)
    wrapt.wrap_function_wrapper(module, "Agent.reply", reply)


def hermes_turn(module):
    def run(wrapped, instance, args, kwargs):
        fields = telemetry.turn_fields(str(getattr(instance, "session_id", "")), metadata={
            "room_id": getattr(instance, "_chat_id", ""),
            "event_id": kwargs.get("persist_user_platform_id", ""),
        })
        if kwargs.get("task_id"):
            fields["agentteams.runtime.task_id"] = str(kwargs["task_id"])
        if getattr(instance, "_parent_session_id", None):
            fields["agentteams.runtime.parent_session_id"] = str(instance._parent_session_id)
        fields = enrich_task(fields, os.getenv("AGENTTEAMS_WORKER_HOME") or os.getenv("HOME"))
        with bind(fields):
            with trace.use_span(start_turn(fields, {"message": args[0] if args else kwargs.get("user_message"),
                                                    "history": kwargs.get("conversation_history")}), end_on_exit=True) as span:
                from at_trace_transport import remember_reply
                remember_reply(span, fields)
                try:
                    result = wrapped(*args, **kwargs)
                    telemetry.set_io(span, output=result)
                    telemetry.outcome(span, result)
                    return result
                except BaseException as exc:
                    telemetry.outcome(span, error=exc)
                    raise

    wrapt.wrap_function_wrapper(module, "TurnFacadeMixin.run_conversation", run)


def hermes_tools(module):
    # Both sequential and parallel execution use this boundary, including policy
    # blocks and inline tools. Keep an explicit compatibility check for upgrades.
    signature = inspect.signature(module._run_agent_tool_execution_middleware)
    required = {"function_name", "function_args", "tool_call_id", "execute"}
    if not required.issubset(signature.parameters):
        message = "Unsupported Hermes tool execution signature; update telemetry adapter"
        _adapter_errors.append(message)
        logging.getLogger("agentteams.telemetry").error(message)
        return

    def execute(wrapped, instance, args, kwargs):
        name = kwargs["function_name"]
        with telemetry.operation(f"tool {name}", attributes={
            "langfuse.observation.type": "tool", "gen_ai.operation.name": "execute_tool",
            "gen_ai.tool.name": name, "gen_ai.tool.call.id": kwargs.get("tool_call_id", ""),
        }) as span:
            telemetry.set_io(span, input=kwargs.get("function_args"))
            try:
                result = wrapped(*args, **kwargs)
                telemetry.set_io(span, output=result.result)
                span.set_attribute("agentteams.tool.blocked", result.blocked)
                span.set_attribute("agentteams.tool.dispatched", result.dispatched)
                span.set_attribute("agentteams.tool.middleware", str(telemetry.capture(result.middleware_trace)))
                telemetry.outcome(span, result.result)
                if result.blocked:
                    telemetry.outcome(span, error=RuntimeError("tool blocked by policy"))
                return result
            except BaseException as exc:
                telemetry.outcome(span, error=exc)
                raise

    wrapt.wrap_function_wrapper(module, "_run_agent_tool_execution_middleware", execute)


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


def openai_content(module):
    # OpenAI instrumentation's prompt capture varies across SDK/provider paths.
    # Supply the actual SDK request to its generation span at on_start instead
    # of emitting a second generation (which would double-count usage).
    def create(wrapped, instance, args, kwargs):
        token = telemetry._model_request.set(kwargs)
        try:
            return wrapped(*args, **kwargs)
        finally:
            telemetry._model_request.reset(token)

    async def async_create(wrapped, instance, args, kwargs):
        token = telemetry._model_request.set(kwargs)
        try:
            return await wrapped(*args, **kwargs)
        finally:
            telemetry._model_request.reset(token)

    wrapt.wrap_function_wrapper(module, "Completions.create", create)
    wrapt.wrap_function_wrapper(module, "AsyncCompletions.create", async_create)


def install():
    runtime = os.getenv("AGENTTEAMS_OTEL_RUNTIME")
    if runtime == "qwenpaw":
        wrapt.register_post_import_hook(agentscope, "agentscope.agent._agent")
        wrapt.register_post_import_hook(qwenpaw_runtime, "qwenpaw.runtime.runtime")
    elif runtime == "hermes":
        wrapt.register_post_import_hook(hermes_turn, "agent.turn_facade")
        wrapt.register_post_import_hook(hermes_tools, "agent.tool_executor")
        wrapt.register_post_import_hook(openai_content, "openai.resources.chat.completions")
    wrapt.register_post_import_hook(httpx, "httpx")
    from at_trace_transport import install as install_transport
    install_transport()
