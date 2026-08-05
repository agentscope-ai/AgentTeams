"""QwenPaw public-plugin bridge for a host-local Codex execution runner."""

from __future__ import annotations

import asyncio
import os
import secrets
import threading
import time
import uuid
from collections import deque
from collections.abc import AsyncGenerator, Callable
from dataclasses import dataclass, field
from typing import Any


def _enabled() -> bool:
    return os.getenv("AGENTTEAMS_CODEX_MANAGER_ENABLED", "").strip().lower() in {
        "1",
        "true",
        "yes",
        "on",
    }


def _configured_token() -> str:
    return os.getenv("AGENTTEAMS_CODEX_BROKER_TOKEN", "").strip()


def _text(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, (list, tuple)):
        return "\n".join(text for item in value if (text := _text(item)))
    if isinstance(value, dict):
        if isinstance(value.get("text"), str):
            return str(value["text"])
        return "\n".join(
            _text(item) for item in value.get("content", []) if _text(item)
        )
    content = getattr(value, "content", None)
    if content is not None:
        return _text(content)
    item_text = getattr(value, "text", None)
    return str(item_text) if isinstance(item_text, str) else ""


def _manager_prompt(agent: Any, input_kwargs: dict[str, Any]) -> str:
    # AgentScope 1.x invokes reply middleware with ``inputs``. Keep the
    # singular aliases for compatibility with older QwenPaw releases and
    # third-party middleware wrappers.
    for name in ("inputs", "msg", "message", "input"):
        prompt = _text(input_kwargs.get(name))
        if prompt:
            return prompt
    context = getattr(getattr(agent, "state", None), "context", [])
    for message in reversed(list(context or [])):
        role = str(getattr(message, "role", "") or "")
        if role in {"user", "system"}:
            prompt = _text(message)
            if prompt:
                return prompt
    return "Continue coordinating the current AgentTeams task."


@dataclass
class Execution:
    execution_id: str
    session_key: str
    prompt: str
    created_at: float = field(default_factory=time.time)
    status: str = "queued"
    leased_at: float = 0.0
    output: str = ""
    error: str = ""
    condition: threading.Condition = field(default_factory=threading.Condition)

    def public_request(self) -> dict[str, Any]:
        return {
            "executionId": self.execution_id,
            "role": "manager",
            "sessionKey": self.session_key,
            "prompt": self.prompt,
            "sandbox": "read-only",
            "approvalPolicy": "never",
        }


class ExecutionBroker:
    """Thread-safe in-memory queue owned by the Manager plugin process."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._queue: deque[str] = deque()
        self._executions: dict[str, Execution] = {}

    def submit(self, *, session_key: str, prompt: str) -> Execution:
        execution = Execution(uuid.uuid4().hex, session_key[:512], prompt[:100_000])
        with self._lock:
            self._executions[execution.execution_id] = execution
            self._queue.append(execution.execution_id)
        return execution

    def lease(self) -> dict[str, Any] | None:
        with self._lock:
            lease_timeout = float(
                os.getenv("AGENTTEAMS_CODEX_BROKER_LEASE_TIMEOUT", "60")
            )
            now = time.time()
            for execution in self._executions.values():
                if (
                    execution.status == "leased"
                    and now - execution.leased_at >= lease_timeout
                ):
                    execution.status = "queued"
                    execution.leased_at = 0.0
                    self._queue.append(execution.execution_id)
            while self._queue:
                execution = self._executions.get(self._queue.popleft())
                if execution and execution.status == "queued":
                    execution.status = "leased"
                    execution.leased_at = now
                    return execution.public_request()
        return None

    def complete(self, execution_id: str, *, output: str = "", error: str = "") -> bool:
        output = output[:20_000]
        error = error[:2_000]
        with self._lock:
            execution = self._executions.get(execution_id)
        if execution is None:
            return False
        if execution.status in {"completed", "failed"}:
            return execution.output == output and execution.error == error
        if execution.status not in {"leased", "queued"}:
            return False
        with execution.condition:
            execution.output = output
            execution.error = error
            execution.status = "failed" if error else "completed"
            execution.condition.notify_all()
        return True

    def release(self, execution_id: str) -> None:
        with self._lock:
            self._executions.pop(execution_id, None)

    def cancel(self, execution_id: str) -> None:
        with self._lock:
            execution = self._executions.get(execution_id)
        if execution is None:
            return
        with execution.condition:
            if execution.status not in {"completed", "failed"}:
                execution.status = "cancelled"
                execution.condition.notify_all()

    def wait(self, execution: Execution, timeout: float) -> Execution:
        deadline = time.monotonic() + timeout
        with execution.condition:
            while execution.status in {"queued", "leased"}:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    execution.status = "timed_out"
                    break
                execution.condition.wait(remaining)
        return execution


BROKER = ExecutionBroker()


def authorized(headers: Any) -> bool:
    expected = _configured_token()
    if not expected:
        return False
    authorization = str(getattr(headers, "get", lambda *_: "")("authorization", ""))
    scheme, _, supplied = authorization.partition(" ")
    return scheme.lower() == "bearer" and secrets.compare_digest(supplied, expected)


def manager_middleware_factory(_ctx: Any, _agent_config: Any):
    if not _enabled() or not _configured_token():
        return None
    try:
        from agentscope.event import (
            TextBlockDeltaEvent,
            TextBlockEndEvent,
            TextBlockStartEvent,
        )
        from agentscope.message import Msg, TextBlock
        from agentscope.middleware import MiddlewareBase
    except ImportError:
        return None

    class CodexManagerMiddleware(MiddlewareBase):
        async def on_reply(
            self,
            agent: Any,
            input_kwargs: dict[str, Any],
            next_handler: Callable[..., AsyncGenerator[Any, None]],
        ) -> AsyncGenerator[Any, None]:
            del next_handler
            state = getattr(agent, "state", None)
            session_key = str(getattr(state, "session_id", "") or uuid.uuid4().hex)
            execution = BROKER.submit(
                session_key=session_key,
                prompt=_manager_prompt(agent, input_kwargs),
            )
            timeout = float(os.getenv("AGENTTEAMS_CODEX_MANAGER_TIMEOUT", "1800"))
            try:
                result = await asyncio.to_thread(BROKER.wait, execution, timeout)
            except asyncio.CancelledError:
                BROKER.cancel(execution.execution_id)
                raise
            try:
                if result.status == "completed":
                    output = (
                        result.output.strip()
                        or "Codex Manager completed without a text response."
                    )
                else:
                    detail = result.error.strip() or result.status
                    output = f"BLOCKED: Codex Manager execution {detail}."
            finally:
                BROKER.release(execution.execution_id)
            block_id = uuid.uuid4().hex
            reply_id = str(getattr(state, "reply_id", "") or uuid.uuid4().hex)
            yield TextBlockStartEvent(reply_id=reply_id, block_id=block_id)
            yield TextBlockDeltaEvent(
                reply_id=reply_id, block_id=block_id, delta=output
            )
            yield TextBlockEndEvent(reply_id=reply_id, block_id=block_id)
            yield Msg(
                name=str(getattr(agent, "name", "manager")),
                role="assistant",
                content=[TextBlock(type="text", text=output)],
            )

    return CodexManagerMiddleware()
