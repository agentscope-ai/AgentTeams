from types import SimpleNamespace
from unittest.mock import Mock

from deepagents_agentteams.approvals import MCPApprovalRule
from deepagents_agentteams.config import ApprovalConfig, InlineConfig, ModelConfig
from deepagents_agentteams.graph import build_deepagents_graph


class FakeMCPClient:
    def __init__(self, connections: dict[str, object], *, tool_name_prefix: bool) -> None:
        self.connections = connections
        self.tool_name_prefix = tool_name_prefix

    async def get_tools(self, *, server_name: str | None = None) -> list[SimpleNamespace]:
        assert server_name is not None
        return [
            SimpleNamespace(name=f"{server_name}_get_issue"),
            SimpleNamespace(name=f"{server_name}_create_issue"),
        ]


async def test_builds_vendored_deepagents_graph_with_higress_mcp_and_hitl() -> None:
    config = SimpleNamespace(
        runtime_name="researcher",
        model=ModelConfig(
            name="qwen-max",
            gateway_url="https://higress.example.org/v1",
            gateway_key="gateway-key",
        ),
        mcp_servers=(
            {
                "name": "github",
                "url": "https://higress.example.org/mcp/github",
                "transport": "http",
            },
        ),
        approvals=ApprovalConfig(
            file_writes="required",
            mcp_default="required",
            mcp_rules=(
                MCPApprovalRule(server="github", tool="get_issue", mode="notRequired"),
            ),
            coordinators=(),
        ),
        inline_config=InlineConfig(
            identity="You are a security reviewer.",
            soul="Be skeptical and precise.",
            agents="Run tests before reporting results.",
        ),
    )
    backend = Mock()
    checkpointer = Mock()
    recorded: dict[str, object] = {}

    def model_factory(**kwargs: object) -> SimpleNamespace:
        return SimpleNamespace(kwargs=kwargs)

    def agent_factory(**kwargs: object) -> str:
        recorded.update(kwargs)
        return "compiled-graph"

    graph = await build_deepagents_graph(
        config,
        backend=backend,
        checkpointer=checkpointer,
        model_factory=model_factory,
        mcp_client_factory=FakeMCPClient,
        agent_factory=agent_factory,
    )

    assert graph == "compiled-graph"
    assert recorded["backend"] is backend
    assert recorded["checkpointer"] is checkpointer
    assert [tool.name for tool in recorded["tools"]] == ["github_get_issue", "github_create_issue"]
    assert recorded["interrupt_on"] == {
        "execute": True,
        "write_file": True,
        "edit_file": True,
        "delete": True,
        "github_create_issue": True,
    }
    assert "/workspace" in recorded["system_prompt"]
    assert "## AgentTeams Identity\nYou are a security reviewer." in recorded["system_prompt"]
    assert "## AgentTeams Soul\nBe skeptical and precise." in recorded["system_prompt"]
    assert "## AgentTeams Instructions\nRun tests before reporting results." in recorded["system_prompt"]
    assert "Never attempt to read container\ncredentials" in recorded["system_prompt"]
    model = recorded["model"]
    assert model.kwargs["base_url"] == "https://higress.example.org/v1"
    assert model.kwargs["use_responses_api"] is False


async def test_not_required_file_writes_omit_all_file_interrupts_and_prompt_sections() -> None:
    config = SimpleNamespace(
        runtime_name="researcher",
        model=ModelConfig(
            name="qwen-max",
            gateway_url="https://higress.example.org/v1",
            gateway_key="gateway-key",
        ),
        mcp_servers=(),
        approvals=ApprovalConfig(
            file_writes="notRequired",
            mcp_default="notRequired",
            mcp_rules=(),
            coordinators=(),
        ),
        inline_config=InlineConfig(identity="", soul="", agents=""),
    )
    recorded: dict[str, object] = {}

    def agent_factory(**kwargs: object) -> str:
        recorded.update(kwargs)
        return "compiled-graph"

    graph = await build_deepagents_graph(
        config,
        backend=Mock(),
        checkpointer=Mock(),
        model_factory=lambda **kwargs: SimpleNamespace(kwargs=kwargs),
        mcp_client_factory=FakeMCPClient,
        agent_factory=agent_factory,
    )

    assert graph == "compiled-graph"
    assert recorded["interrupt_on"] == {"execute": True}
    assert "## AgentTeams" not in recorded["system_prompt"]
