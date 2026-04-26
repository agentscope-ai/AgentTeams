"""Runtime hooks for adapting upstream CoPaw behavior to HiClaw."""

from __future__ import annotations

import logging
from typing import Any

logger = logging.getLogger(__name__)

_TOOL_HOOK_INSTALLED = False
_MESSAGE_FILTER_HOOK_INSTALLED = False


def _message_text(msg: Any) -> str:
    if hasattr(msg, "get_text_content"):
        try:
            return msg.get_text_content() or ""
        except Exception:
            logger.debug("Failed to read message text", exc_info=True)

    content = getattr(msg, "content", None)
    if isinstance(content, list):
        for block in content:
            if isinstance(block, dict) and block.get("type") == "text":
                return str(block.get("text") or "")
            if getattr(block, "type", None) == "text":
                return str(getattr(block, "text", "") or "")
    if isinstance(content, str):
        return content
    return ""


def _replace_message_text(msg: Any, text: str) -> None:
    from agentscope.message import TextBlock

    if hasattr(msg, "content"):
        msg.content = [TextBlock(type="text", text=text)]


def install_message_filter_hooks() -> None:
    """Install final-response filtering for outgoing Matrix replies."""
    global _MESSAGE_FILTER_HOOK_INSTALLED
    if _MESSAGE_FILTER_HOOK_INSTALLED:
        return

    from copaw.app.runner.runner import AgentRunner
    from copaw_worker.hooks.message_filter import (
        NO_REPLY_TOKEN,
        filter_outgoing_matrix_message,
    )

    original_query_handler = AgentRunner.query_handler
    if getattr(original_query_handler, "_hiclaw_message_filter_hook", False):
        _MESSAGE_FILTER_HOOK_INSTALLED = True
        return

    async def query_handler_with_message_filter(
        self: Any,
        msgs: Any,
        request: Any = None,
        **kwargs: Any,
    ):
        async for item in original_query_handler(self, msgs, request, **kwargs):
            if not isinstance(item, tuple) or not item:
                yield item
                continue

            msg = item[0]
            text = _message_text(msg)
            result = filter_outgoing_matrix_message(text)
            if result.suppressed:
                session_id = (
                    getattr(request, "session_id", "") if request is not None else ""
                )
                logger.info(
                    "Message filter suppressed final reply for session=%s: %s",
                    session_id,
                    result.suppress_reason,
                )
                _replace_message_text(msg, NO_REPLY_TOKEN)
            elif result.rewritten:
                logger.info(
                    "Message filter rewrote final reply: %s",
                    result.rewrite_reason,
                )
                _replace_message_text(msg, result.text)
            yield item

    query_handler_with_message_filter._hiclaw_message_filter_hook = True  # type: ignore[attr-defined]
    AgentRunner.query_handler = query_handler_with_message_filter
    _MESSAGE_FILTER_HOOK_INSTALLED = True
    logger.info("Installed HiClaw CoPaw message filter hooks")


def install_tool_hooks() -> None:
    """Install HiClaw-owned CoPaw tool hooks.

    CoPaw creates a temporary CoPawAgent for every query, and each agent
    builds a fresh toolkit. Hooking _create_toolkit lets HiClaw inject tools
    without modifying upstream CoPaw files.
    """
    global _TOOL_HOOK_INSTALLED
    install_message_filter_hooks()

    if _TOOL_HOOK_INSTALLED:
        return

    from copaw.agents.react_agent import CoPawAgent
    from copaw_worker.hooks.tools.filesync import filesync
    from copaw_worker.hooks.tools.message import message
    from copaw_worker.hooks.tools.taskflow import taskflow

    original_create_toolkit = CoPawAgent._create_toolkit
    if getattr(original_create_toolkit, "_hiclaw_message_hook", False):
        _TOOL_HOOK_INSTALLED = True
        return

    def create_toolkit_with_hiclaw_tools(self: Any, *args: Any, **kwargs: Any):
        toolkit = original_create_toolkit(self, *args, **kwargs)
        try:
            toolkit.register_tool_function(
                message,
                namesake_strategy="override",
            )
            logger.debug("Registered HiClaw CoPaw message tool")
            toolkit.register_tool_function(
                filesync,
                namesake_strategy="override",
            )
            logger.debug("Registered HiClaw CoPaw filesync tool")
            toolkit.register_tool_function(
                taskflow,
                namesake_strategy="override",
            )
            logger.debug("Registered HiClaw CoPaw taskflow tool")
        except Exception:
            logger.exception("Failed to register HiClaw CoPaw message tool")
        return toolkit

    create_toolkit_with_hiclaw_tools._hiclaw_message_hook = True  # type: ignore[attr-defined]
    CoPawAgent._create_toolkit = create_toolkit_with_hiclaw_tools
    _TOOL_HOOK_INSTALLED = True
    logger.info("Installed HiClaw CoPaw tool hooks")
