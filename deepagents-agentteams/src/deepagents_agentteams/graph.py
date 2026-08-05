"""Construction of the DeepAgents graph used by an AgentTeams Worker."""

from __future__ import annotations

from collections.abc import Callable
from typing import Any

from deepagents_agentteams.approvals import ToolApprovalPolicy
from deepagents_agentteams.config import RuntimeConfig
from deepagents_agentteams.gateway import build_higress_model, build_mcp_connections

_SYSTEM_PROMPT = """You are an AgentTeams Worker running on DeepAgents.

Use /workspace as the root for all task files. The execution sandbox is ephemeral;
only files synchronized by AgentTeams are durable. Never attempt to read container
credentials, service-account tokens, or paths outside /workspace. Human approval
interrupts are authorization boundaries, not suggestions: wait for an explicit
decision before continuing an interrupted action.
"""


def system_prompt(config: RuntimeConfig) -> str:
    """Compose fixed security boundaries with optional explicit prompt sections."""
    sections = [_SYSTEM_PROMPT.rstrip()]
    inline = config.inline_config
    for title, content in (
        ("AgentTeams Identity", inline.identity),
        ("AgentTeams Soul", inline.soul),
        ("AgentTeams Instructions", inline.agents),
    ):
        if content:
            sections.append(f"## {title}\n{content}")
    return "\n\n".join(sections) + "\n"


async def build_deepagents_graph(
    config: RuntimeConfig,
    *,
    backend: Any,
    checkpointer: Any,
    model_factory: Callable[..., Any] | None = None,
    mcp_client_factory: Callable[..., Any] | None = None,
    agent_factory: Callable[..., Any] | None = None,
) -> Any:
    """Build a configured graph using public DeepAgents extension points."""
    if mcp_client_factory is None:
        from langchain_mcp_adapters.client import MultiServerMCPClient

        mcp_client_factory = MultiServerMCPClient
    if agent_factory is None:
        from deepagents import create_deep_agent

        agent_factory = create_deep_agent

    model = build_higress_model(config.model, model_factory=model_factory)
    connections = build_mcp_connections(config.mcp_servers, gateway_key=config.model.gateway_key)
    mcp_client = mcp_client_factory(connections, tool_name_prefix=True)
    policy = ToolApprovalPolicy(
        file_writes=config.approvals.file_writes,
        mcp_default=config.approvals.mcp_default,
        mcp_rules=config.approvals.mcp_rules,
    )

    tools: list[Any] = []
    interrupt_on: dict[str, bool] = {"execute": True}
    if policy.file_writes == "required":
        interrupt_on.update({"write_file": True, "edit_file": True, "delete": True})

    for server in config.mcp_servers:
        server_name = server["name"]
        server_tools = await mcp_client.get_tools(server_name=server_name)
        prefix = f"{server_name}_"
        for tool in server_tools:
            tool_name = getattr(tool, "name", "")
            if not isinstance(tool_name, str) or not tool_name:
                raise ValueError(f"MCP server {server_name} returned a tool without a name")
            original_name = tool_name[len(prefix) :] if tool_name.startswith(prefix) else tool_name
            if policy.requires_approval(
                tool_name="mcp",
                mcp_server=server_name,
                mcp_tool=original_name,
            ):
                interrupt_on[tool_name] = True
        tools.extend(server_tools)

    return agent_factory(
        model=model,
        tools=tools,
        system_prompt=system_prompt(config),
        backend=backend,
        interrupt_on=interrupt_on,
        checkpointer=checkpointer,
        name=f"agentteams-{config.runtime_name}",
    )
