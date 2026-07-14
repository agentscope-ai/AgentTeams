"""Runtime hooks for adapting upstream CoPaw behavior to AgentTeams."""

from __future__ import annotations

import logging
from copy import deepcopy
from typing import Any

logger = logging.getLogger(__name__)

_TOOL_HOOK_INSTALLED = False
_MESSAGE_FILTER_HOOK_INSTALLED = False


def _builtin_tool_config(agent: Any, name: str) -> Any | None:
    try:
        builtin_tools = agent._agent_config.tools.builtin_tools
        return builtin_tools.get(name)
    except Exception:
        return None


def _builtin_tool_enabled(agent: Any, name: str) -> bool:
    config = _builtin_tool_config(agent, name)
    return bool(getattr(config, "enabled", True))


def _builtin_tool_async_execution(agent: Any, name: str) -> bool:
    config = _builtin_tool_config(agent, name)
    return bool(getattr(config, "async_execution", False))


def _register_tool_function(toolkit: Any, func: Any, **kwargs: Any) -> None:
    try:
        toolkit.register_tool_function(func, **kwargs)
    except TypeError:
        fallback = dict(kwargs)
        fallback.pop("async_execution", None)
        toolkit.register_tool_function(func, **fallback)


def _existing_tool_schema(toolkit: Any, name: str) -> dict[str, Any] | None:
    tool = getattr(toolkit, "tools", {}).get(name)
    schema = getattr(tool, "json_schema", None)
    return deepcopy(schema) if isinstance(schema, dict) else None


def install_message_filter_hooks() -> None:
    """Leave Matrix reply filtering to final send/tool boundaries."""
    global _MESSAGE_FILTER_HOOK_INSTALLED
    if _MESSAGE_FILTER_HOOK_INSTALLED:
        return

    _MESSAGE_FILTER_HOOK_INSTALLED = True
    logger.info("AgentTeams CoPaw query-handler message filter hook is disabled")


def install_tool_hooks() -> None:
    """Install AgentTeams-owned CoPaw tool hooks.

    CoPaw creates a temporary CoPawAgent for every query, and each agent
    builds a fresh toolkit. Hooking _create_toolkit lets AgentTeams inject tools
    without modifying upstream CoPaw files.
    """
    global _TOOL_HOOK_INSTALLED
    install_message_filter_hooks()

    if _TOOL_HOOK_INSTALLED:
        return

    from copaw.agents.react_agent import CoPawAgent
    from copaw_worker.hooks.credential_guard import install_credential_guard_hook
    from copaw_worker.hooks.output_sanitizer import create_sanitizer_middleware
    from copaw_worker.hooks.tools.filesync import filesync
    from copaw_worker.hooks.tools.message import message
    from copaw_worker.hooks.tools.projectflow import projectflow
    from copaw_worker.hooks.tools.taskflow import taskflow

    original_create_toolkit = CoPawAgent._create_toolkit
    if getattr(original_create_toolkit, "_hiclaw_message_hook", False):
        _TOOL_HOOK_INSTALLED = True
        return

    def create_toolkit_with_hiclaw_tools(self: Any, *args: Any, **kwargs: Any):
        toolkit = original_create_toolkit(self, *args, **kwargs)
        try:
            _register_tool_function(
                toolkit,
                message,
                namesake_strategy="override",
            )
            logger.debug("Registered AgentTeams CoPaw message tool")
            _register_tool_function(
                toolkit,
                filesync,
                namesake_strategy="override",
            )
            logger.debug("Registered AgentTeams CoPaw filesync tool")
            _register_tool_function(
                toolkit,
                projectflow,
                namesake_strategy="override",
            )
            logger.debug("Registered AgentTeams CoPaw projectflow tool")
            _register_tool_function(
                toolkit,
                taskflow,
                namesake_strategy="override",
            )
            logger.debug("Registered AgentTeams CoPaw taskflow tool")
        except Exception:
            logger.exception("Failed to register AgentTeams CoPaw tool hooks")
        try:
            toolkit.register_middleware(create_sanitizer_middleware())
        except Exception:
            logger.exception("Failed to register output sanitizer middleware")
        return toolkit

    create_toolkit_with_hiclaw_tools._hiclaw_message_hook = True  # type: ignore[attr-defined]
    CoPawAgent._create_toolkit = create_toolkit_with_hiclaw_tools
    _TOOL_HOOK_INSTALLED = True
    logger.info("Installed AgentTeams CoPaw tool hooks")

    install_credential_guard_hook()
