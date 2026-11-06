# -*- coding: utf-8 -*-
"""`/qcode` — talk to Qwen Code directly from any QwenPaw chat.

The runtime dispatches slash commands *before* the model turn; returning a
``Msg`` short-circuits the agent (``runtime/runtime.py`` — fixed 1). So this
command is a real CLI bridge: the message goes to Qwen Code, and the CLI's
answer becomes the chat reply — no model turn, no tool round-trip.

Continuity: the first message of a QwenPaw session creates a Qwen Code
session (``--session-id <uuid>`` + ``--chat-recording``); later messages
resume it (``-r <uuid>``). The mapping lives in the plugin state dir, so it
survives restarts and hot reloads.

Env:
  QWEN_CODE_BRIDGE_CLI            executable (default ``qwen``)
  QWEN_CODE_BRIDGE_SLASH_TIMEOUT  wall-clock seconds (default 180)
  QWEN_CODE_BRIDGE_SLASH_CWD      working directory (default: workspace dir)
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import time
import uuid
from pathlib import Path
from typing import Any

from .dispatch import _effective_approval_level, resolve_approval_mode
from .state import state_dir

CLI_ENV = "QWEN_CODE_BRIDGE_CLI"
TIMEOUT_ENV = "QWEN_CODE_BRIDGE_SLASH_TIMEOUT"
CWD_ENV = "QWEN_CODE_BRIDGE_SLASH_CWD"
_DEFAULT_TIMEOUT = 180.0
USAGE = "用法：`/qcode <问题或任务>`（同一会话自动续聊；`/qcode` 空参显示本条）"


def _sessions_dir() -> Path:
    path = state_dir() / "slash-sessions"
    path.mkdir(parents=True, exist_ok=True)
    return path


def _session_file(qwenpaw_session_id: str) -> Path:
    digest = hashlib.sha1(
        str(qwenpaw_session_id or "default").encode("utf-8"),
    ).hexdigest()[:16]
    return _sessions_dir() / f"{digest}.json"


def load_cli_session(qwenpaw_session_id: str) -> str:
    try:
        data = json.loads(_session_file(qwenpaw_session_id).read_text("utf-8"))
        return str(data.get("cli_session_id") or "")
    except Exception:
        return ""


def save_cli_session(
    qwenpaw_session_id: str,
    cli_session_id: str,
    *,
    turns: int = 0,
) -> None:
    try:
        path = _session_file(qwenpaw_session_id)
        path.write_text(
            json.dumps(
                {
                    "cli_session_id": cli_session_id,
                    "turns": turns,
                    "updated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ"),
                },
                ensure_ascii=False,
            ),
            encoding="utf-8",
        )
    except Exception:
        pass


def _parse_result(stdout: bytes) -> str:
    """Extract the final ``result`` from qwen stream-json output."""
    text = stdout.decode("utf-8", "replace")
    result = ""
    last_text = ""
    for line in text.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            if line:
                last_text = line
            continue
        try:
            event = json.loads(line)
        except ValueError:
            continue
        kind = str(event.get("type") or "")
        if kind == "result":
            result = str(event.get("result") or "")
        elif kind == "assistant":
            message = event.get("message") or {}
            for block in message.get("content") or []:
                if isinstance(block, dict) and block.get("type") == "text":
                    last_text = str(block.get("text") or "")
    return (result or last_text).strip()


async def run_cli(
    prompt: str,
    *,
    cwd: str | None = None,
    qwenpaw_session_id: str = "default",
    agent_id: str = "",
) -> str:
    """Run one CLI turn; returns the answer text (errors as text)."""
    binary = os.environ.get(CLI_ENV, "").strip() or "qwen"
    try:
        timeout = float(
            os.environ.get(TIMEOUT_ENV, "").strip() or _DEFAULT_TIMEOUT,
        )
    except ValueError:
        timeout = _DEFAULT_TIMEOUT
    workdir = (
        (cwd or "").strip()
        or os.environ.get(CWD_ENV, "").strip()
        or os.getcwd()
    )
    if not os.path.isdir(workdir):
        workdir = os.getcwd()

    cli_session_id = load_cli_session(qwenpaw_session_id)
    mode = resolve_approval_mode(None, _effective_approval_level(agent_id))
    base = [binary, "--approval-mode", mode, "--channel", "CI", "-o", "stream-json"]
    if cli_session_id:
        args = base + ["-r", cli_session_id, "-p", prompt]
    else:
        cli_session_id = str(uuid.uuid4())
        args = base + [
            "--chat-recording",
            "--session-id",
            cli_session_id,
            "-p",
            prompt,
        ]

    try:
        proc = await asyncio.create_subprocess_exec(
            *args,
            cwd=workdir,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.STDOUT,
        )
        try:
            stdout, _ = await asyncio.wait_for(
                proc.communicate(),
                timeout=timeout,
            )
        except asyncio.TimeoutError:
            proc.kill()
            await proc.wait()
            return f"Qwen Code 超时（>{int(timeout)}s）：`{prompt[:80]}`"
    except OSError as exc:
        return f"Qwen Code 启动失败：{exc}"

    answer = _parse_result(stdout)
    if not cli_session_id.startswith("-") and not load_cli_session(
        qwenpaw_session_id,
    ):
        save_cli_session(qwenpaw_session_id, cli_session_id, turns=0)
    if answer:
        if cli_session_id:
            save_cli_session(
                qwenpaw_session_id,
                cli_session_id,
                turns=1,
            )
        return answer
    tail = stdout.decode("utf-8", "replace").strip().splitlines()[-3:]
    return "Qwen Code 无输出" + (f"（rc={proc.returncode}）" if proc.returncode else "") + (
        ("\n" + "\n".join(tail)) if tail else ""
    )




async def append_to_session(
    session: Any,
    session_id: str,
    user_id: str,
    channel: str,
    *,
    role: str,
    name: str,
    text: str,
) -> bool:
    """Append one message to a session's agent context (chat history)."""
    try:
        from agentscope.message import Msg, TextBlock
        from agentscope.state import AgentState

        state_dict = await session.get_session_state_dict(
            session_id,
            user_id=user_id,
            channel=channel,
        )
        raw = ((state_dict or {}).get("agent") or {}).get("state") or {}
        try:
            state = AgentState.model_validate(raw) if raw else AgentState()
        except Exception:
            state = AgentState()
        state.context.append(
            Msg(
                name=name,
                role=role,
                content=[TextBlock(type="text", text=text)],
            ),
        )
        await session.update_session_state(
            session_id,
            "agent.state",
            state.model_dump(mode="json"),
            user_id=user_id,
            channel=channel,
        )
        return True
    except Exception:
        return False


async def _persist_turn(ctx: Any, user_text: str, answer: str) -> None:
    """Append the slash turn to the session store so the chat shows it.

    Slash dispatch short-circuits the agent, so nothing else writes the
    conversation. We mirror what an agent turn would persist: append the
    user message and the assistant reply to ``agent.state.context``.
    """
    try:
        session = getattr(getattr(ctx, "workspace", None), "session", None)
        if session is None:
            return
        request = getattr(ctx, "request", None)
        user_id = str(getattr(request, "user_id", "") or "default")
        channel = str(getattr(request, "channel", "") or "console")
        session_id = str(getattr(ctx, "session_id", "") or "")
        if not session_id:
            return
        await append_to_session(
            session,
            session_id,
            user_id,
            channel,
            role="user",
            name=user_id,
            text=user_text,
        )
        await append_to_session(
            session,
            session_id,
            user_id,
            channel,
            role="assistant",
            name="qwen-code",
            text=answer,
        )
    except Exception:
        return


def make_handler():
    """Build the async slash-command handler ``(ctx, args) -> Msg``."""

    async def handler(ctx: Any, args: str) -> Any:
        from agentscope.message import Msg, TextBlock

        prompt = str(args or "").strip()
        if not prompt:
            return Msg(
                name="qwen-code",
                role="assistant",
                content=[TextBlock(type="text", text=USAGE)],
            )
        session_id = str(getattr(ctx, "session_id", "") or "default")
        workspace = str(getattr(ctx, "workspace_dir", "") or "").strip()
        answer = await run_cli(
            prompt,
            cwd=workspace or None,
            qwenpaw_session_id=session_id,
            agent_id=str(getattr(ctx, "agent_id", "") or ""),
        )
        await _persist_turn(ctx, prompt, answer)
        return Msg(
            name="qwen-code",
            role="assistant",
            content=[TextBlock(type="text", text=answer)],
        )

    return handler
