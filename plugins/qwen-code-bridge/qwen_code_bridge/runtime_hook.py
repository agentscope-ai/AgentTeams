# -*- coding: utf-8 -*-
"""Runtime PRE_DISPATCH hook: make the bridge's own chats behave natively.

Two session shapes are owned by the bridge (channel ``qwen-code-bridge``):

* ``qwen-code-bridge:direct`` — the "直接对话" chat. Plain text (no slash
  prefix) goes to Qwen Code; the answer is returned as the assistant message
  and persisted into the chat history.
* ``qwen-code-bridge:<task_id>`` — a dispatch run's chat. Any message renders
  a read-only status card (state / exit / wall + transcript tail), so the run
  is visible inside QwenPaw without opening Qwen Code.

Everything else returns ``CONTINUE`` — normal chats are untouched.
"""

from __future__ import annotations

import json
from typing import Any

from qwenpaw.runtime.hooks import HookAction, HookBase, HookResult
from qwenpaw.runtime.phases import Phase

from .chats import DIRECT_SESSION_ID
from .dispatch import load_task
from .slash import _persist_turn, run_cli

RUN_PREFIX = "qwen-code-bridge:"
RUN_ID_LEN = 32
TRANSCRIPT_LINES = 40


def _last_user_text(ctx: Any) -> str:
    try:
        for msg in reversed(list(getattr(ctx, "input_msgs", []) or [])):
            if str(getattr(msg, "role", "")) != "user":
                continue
            content = getattr(msg, "content", None)
            if isinstance(content, str):
                return content.strip()
            for block in content or []:
                text = getattr(block, "text", None)
                if text:
                    return str(text).strip()
    except Exception:
        return ""
    return ""


def _msg(text: str) -> Any:
    from agentscope.message import Msg, TextBlock

    return Msg(
        name="qwen-code",
        role="assistant",
        content=[TextBlock(type="text", text=text)],
    )


def _run_card(task_id: str) -> str:
    task = load_task(task_id)
    if not task:
        return (
            f"⚠️ 找不到派单记录 `{task_id}`（可能已清理）。\n"
            "本会话由 Qwen Code 控制台承载；如未显示请刷新。"
        )
    state = task.get("state") or "?"
    exit_code = task.get("exit_code")
    wall = task.get("wall_s")
    cli = task.get("cli") or "?"
    mode = task.get("approval_mode") or "?"
    tail = str(task.get("log_tail") or "").strip().splitlines()[-TRANSCRIPT_LINES:]
    header = (
        f"**{task.get('task')}** · {cli} · {state}"
        f" · exit={exit_code} · wall={wall}s · approval={mode}\n"
        "[打开 Qwen Code 控制台](/qwen-code) "
        f"· session `{task.get('session_id')}`\n"
    )
    if tail:
        header += "```\n" + "\n".join(tail) + "\n```"
    else:
        header += "_（暂无输出）_"
    return header


class BridgeChatHook(HookBase):
    """PRE_DISPATCH hook for ``qwen-code-bridge:*`` sessions."""

    phase = Phase.PRE_DISPATCH
    name = "qwen_code_bridge_chat"
    priority = 10

    async def run(self, ctx: Any) -> HookResult:
        session_id = str(getattr(ctx, "session_id", "") or "")
        if not session_id.startswith(RUN_PREFIX):
            return HookResult()
        text = _last_user_text(ctx)
        if not text:
            return HookResult()

        if session_id == DIRECT_SESSION_ID or session_id.startswith(
            DIRECT_SESSION_ID + ":",
        ):
            answer = await run_cli(
                text,
                cwd=str(getattr(ctx, "workspace_dir", "") or "") or None,
                qwenpaw_session_id=session_id,
                agent_id=str(getattr(ctx, "agent_id", "") or ""),
            )
            await _persist_turn(ctx, text, answer)
            return HookResult(
                action=HookAction.SHORT_CIRCUIT,
                payload=_msg(answer),
            )

        task_id = session_id[len(RUN_PREFIX):]
        if len(task_id) == RUN_ID_LEN:
            card = _run_card(task_id)
            await _persist_turn(ctx, text, card)
            return HookResult(
                action=HookAction.SHORT_CIRCUIT,
                payload=_msg(card),
            )
        return HookResult()


__all__ = ["BridgeChatHook"]


def _dump(task: Any) -> str:  # pragma: no cover - debug helper
    return json.dumps(task, ensure_ascii=False, default=str)
