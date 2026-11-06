# -*- coding: utf-8 -*-
"""Built-in system-prompt guidance for the Qwen Code bridge.

Registered as one host prompt section (``after="workspace"``). It is short on
purpose: it tells the agent what the bridge chats are and how to delegate,
without duplicating the dispatch API docs.
"""

from __future__ import annotations

PROMPT_TEXT = (
    "## Qwen Code bridge\n"
    "- Bridge chats (channel `qwen-code-bridge`) bypass you: "
    "the bridge direct chat (session `qwen-code-bridge:direct`) is answered "
    "by the local Qwen Code CLI, and "
    "`<task> \u00b7 <cli> \u00b7 <state>` chats open the native Qwen Code "
    "console directly.\n"
    "- Delegate coding work with `POST /api/qwen-code-bridge/dispatch` and "
    "pass `origin` = your own session triple (session_id / user_id / channel); "
    "the terminal wake returns to that session. Check status with "
    "`GET /api/qwen-code-bridge/dispatch/<task>` \u2014 never poll a shell.\n"
    "- `/qcode <prompt>` answers from Qwen Code in any chat with per-session "
    "continuity. Approval tiers follow the host tool-execution level "
    "(STRICT\u2192plan, SMART\u2192auto-edit, AUTO\u2192auto, OFF\u2192yolo).\n"
)



def bridge_prompt(_agent=None) -> str:
    """Provider for ``register_prompt_section``."""
    return PROMPT_TEXT


__all__ = ["PROMPT_TEXT", "bridge_prompt"]
