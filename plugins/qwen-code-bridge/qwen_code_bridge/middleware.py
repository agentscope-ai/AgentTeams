# -*- coding: utf-8 -*-
"""Session-injection middleware (design doc §5).

Makes ``QWENPAW_SESSION_ID`` available to every shell command the agent
runs, so headless CLI invocations (``qwen -p ...``) and downstream
scripts can identify the owning QwenPaw session.

Plugin-side replacement for upstream PR 8156 layer 2 (core contextvar
injection in ``qwenpaw.agents.tools.shell``): the plugin never modifies
core — injection happens as an AgentScope middleware.

Contract verified by the T0 spike (``spike-report.md``, 6/6 green):

- H1: ``on_acting`` must mutate ``input_kwargs["tool_call"]`` in place
  before calling ``next_handler()``. ``ToolCallBlock.input`` is a raw
  JSON string, so the mutation is ``json.loads`` → modify →
  ``json.dumps`` write-back on the block object.
- H2: the runtime builds a ``HookContext`` per request and the agent
  builder calls ``factory(ctx, agent_config)``; factories returning
  ``None`` are skipped, so an empty ``ctx.session_id`` means no
  middleware for that request.

Only whitelisted shell tools are touched; any other tool call passes
through untouched.
"""

from __future__ import annotations

import json
import logging
from typing import Any, AsyncGenerator, Callable

from agentscope.middleware import MiddlewareBase

logger = logging.getLogger(__name__)

#: Environment variable injected into shell commands (design §5).
SESSION_ENV_VAR = "QWENPAW_SESSION_ID"

#: Tool names whose ``command`` argument is a shell command line.
#: qwenpaw's shell tool is ``execute_shell_command``
#: (``qwenpaw/agents/tools/shell.py``). Anything else is never touched.
SHELL_TOOL_NAMES = frozenset({"execute_shell_command"})


def _quote_session_id(session_id: str) -> str:
    """Single-quote a shell value, escaping embedded single quotes."""
    return "'" + session_id.replace("'", "'\\''") + "'"


class SessionEnvMiddleware(MiddlewareBase):
    """Prepend ``export QWENPAW_SESSION_ID='<session_id>' &&`` to shell commands.

    Idempotent: a command that already mentions ``QWENPAW_SESSION_ID``
    is passed through unchanged. The middleware holds the request
    ``HookContext`` (built per request by the runtime; the builder
    skips the registration when ``ctx.session_id`` is empty).
    """

    def __init__(self, ctx: Any) -> None:
        self._ctx = ctx

    def _inject(self, tool_call: Any) -> None:
        session_id = getattr(self._ctx, "session_id", "") or ""
        if not session_id:
            return
        try:
            args = json.loads(tool_call.input)
        except (TypeError, ValueError):
            return
        if not isinstance(args, dict):
            return
        command = args.get("command")
        if not isinstance(command, str) or not command:
            return
        if SESSION_ENV_VAR in command:
            return
        args["command"] = (
            f"export {SESSION_ENV_VAR}="
            f"{_quote_session_id(session_id)} && {command}"
        )
        tool_call.input = json.dumps(args, ensure_ascii=False)

    async def on_acting(
        self,
        agent: Any,
        input_kwargs: dict,
        next_handler: Callable[..., AsyncGenerator],
    ) -> AsyncGenerator:
        tool_call = (
            input_kwargs.get("tool_call")
            if isinstance(input_kwargs, dict)
            else None
        )
        if tool_call is not None and getattr(
            tool_call, "name", None
        ) in SHELL_TOOL_NAMES:
            self._inject(tool_call)
        async for event in next_handler():
            yield event


def session_middleware_factory(
    ctx: Any, agent_config: Any
) -> SessionEnvMiddleware | None:
    """``register_middleware`` factory (design §5).

    Called once per request with the runtime ``HookContext``. Returns
    ``None`` when ``ctx.session_id`` is empty — the builder then skips
    the middleware for that request.
    """
    session_id = getattr(ctx, "session_id", "") or ""
    if not session_id:
        return None
    # The chat-level approval switch is client state in QwenPaw (request_context
    # only); record it here so a later wake turn can reuse the freshest level
    # the server can observe instead of a stale dispatch-time capture.
    try:
        request = getattr(ctx, "request", None)
        request_context = getattr(request, "request_context", None) or {}
        level = ""
        if isinstance(request_context, dict):
            level = str(request_context.get("approval_level") or "").strip()
        if level:
            from .session_levels import record as _record_level

            _record_level(session_id, level)
    except Exception:  # never let bookkeeping break the turn
        pass
    return SessionEnvMiddleware(ctx)


__all__ = [
    "SESSION_ENV_VAR",
    "SHELL_TOOL_NAMES",
    "SessionEnvMiddleware",
    "session_middleware_factory",
]
