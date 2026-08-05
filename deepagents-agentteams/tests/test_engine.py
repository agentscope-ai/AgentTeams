from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import httpx
import pytest
from langgraph.types import Command, Interrupt

from deepagents_agentteams.engine import AgentEngine, ManagedAgentIdentityClient, PendingApprovalStore
from deepagents_agentteams.matrix import MatrixMessage


class FakeGraph:
    def __init__(self) -> None:
        self.awaiting_approval = True
        self.invocations = []

    async def ainvoke(self, value, *, config):  # noqa: ANN001, ANN201
        self.invocations.append((value, config))
        if isinstance(value, Command):
            self.awaiting_approval = False
            return {"messages": [SimpleNamespace(type="ai", content="approved result")]}
        return {"messages": []}

    async def aget_state(self, config):  # noqa: ANN001, ANN201
        if not self.awaiting_approval:
            return SimpleNamespace(interrupts=())
        return SimpleNamespace(
            interrupts=(
                Interrupt(
                    value={
                        "action_requests": [
                            {"name": "execute", "args": {"command": "make test"}},
                            {"name": "github_create_issue", "args": {"title": "Failure"}},
                        ],
                        "review_configs": [],
                    },
                    id="interrupt-1",
                ),
            )
        )


def runtime_config() -> SimpleNamespace:
    return SimpleNamespace(
        worker_uid="worker-uid-1",
        human_approver_ids=frozenset({"@operator:example.org", "@manager:example.org"}),
        agent_matrix_ids=frozenset(
            {
                "@manager:example.org",
                "@worker:example.org",
                "@leader:example.org",
            }
        ),
        approvals=SimpleNamespace(coordinators=()),
    )


def message(sender: str, body: str) -> MatrixMessage:
    return MatrixMessage(
        room_id="!room:example.org",
        event_id="$event",
        thread_root_event_id="$root",
        sender=sender,
        body=body,
    )


async def test_matrix_human_approval_resumes_the_same_checkpoint_thread(tmp_path: Path) -> None:
    graph = FakeGraph()
    replies: list[str] = []

    async def graph_factory(_thread_id: str) -> FakeGraph:
        return graph

    async def send_reply(_message: MatrixMessage, body: str) -> None:
        replies.append(body)

    engine = AgentEngine(
        config=runtime_config(),
        graph_factory=graph_factory,
        send_reply=send_reply,
        pending_store=PendingApprovalStore(tmp_path / "pending.json"),
        is_managed_agent=AsyncMock(return_value=False),
    )

    await engine.handle_message(message("@manager:example.org", "investigate"))
    await engine.handle_message(message("@manager:example.org", "approve all"))
    await engine.handle_message(message("@worker:example.org", "approve all"))
    await engine.handle_message(message("@leader:example.org", "approve all"))
    await engine.handle_message(message("@operator:example.org", "approve all"))

    assert "Approval required" in replies[0]
    assert "not authorized" in replies[1]
    assert "not authorized" in replies[2]
    assert "not authorized" in replies[3]
    assert replies[4] == "approved result"
    command = graph.invocations[-1][0]
    assert isinstance(command, Command)
    assert command.resume == {
        "decisions": [
            {"type": "approve"},
            {"type": "approve"},
        ]
    }
    first_thread = graph.invocations[0][1]["configurable"]["thread_id"]
    resumed_thread = graph.invocations[-1][1]["configurable"]["thread_id"]
    assert first_thread == resumed_thread


async def test_live_worker_requester_and_manager_coordinator_are_denied_without_restart(
    tmp_path: Path,
) -> None:
    graph = FakeGraph()
    replies: list[str] = []
    managed_ids: set[str] = set()

    async def graph_factory(_thread_id: str) -> FakeGraph:
        return graph

    async def send_reply(_message: MatrixMessage, body: str) -> None:
        replies.append(body)

    async def is_managed_agent(matrix_user_id: str) -> bool:
        return matrix_user_id in managed_ids

    config = runtime_config()
    coordinator = "@late-manager:example.org"
    config.human_approver_ids = config.human_approver_ids | {coordinator}
    config.approvals = SimpleNamespace(coordinators=(coordinator,))
    engine = AgentEngine(
        config=config,
        graph_factory=graph_factory,
        send_reply=send_reply,
        pending_store=PendingApprovalStore(tmp_path / "pending.json"),
        is_managed_agent=is_managed_agent,
    )
    requester = "@new-agent:example.org"

    await engine.handle_message(message(requester, "investigate"))
    managed_ids.add(requester)  # The controller now reports a Worker with this MXID.
    await engine.handle_message(message(requester, "approve all"))
    managed_ids.add(coordinator)  # A configured Human coordinator is now a managed Manager.
    await engine.handle_message(message(coordinator, "approve all"))

    assert "not authorized" in replies[-2]
    assert "not authorized" in replies[-1]
    assert len(graph.invocations) == 1


async def test_live_human_remains_authorized_and_lookup_failure_fails_closed(tmp_path: Path) -> None:
    graph = FakeGraph()
    replies: list[str] = []
    lookup = AsyncMock(return_value=False)

    async def graph_factory(_thread_id: str) -> FakeGraph:
        return graph

    async def send_reply(_message: MatrixMessage, body: str) -> None:
        replies.append(body)

    engine = AgentEngine(
        config=runtime_config(),
        graph_factory=graph_factory,
        send_reply=send_reply,
        pending_store=PendingApprovalStore(tmp_path / "pending.json"),
        is_managed_agent=lookup,
    )
    await engine.handle_message(message("@operator:example.org", "investigate"))
    lookup.side_effect = RuntimeError("controller unavailable")
    await engine.handle_message(message("@operator:example.org", "approve all"))
    assert "temporarily unavailable" in replies[-1]
    assert len(graph.invocations) == 1

    lookup.side_effect = None
    lookup.return_value = False
    await engine.handle_message(message("@operator:example.org", "approve all"))
    assert replies[-1] == "approved result"
    assert len(graph.invocations) == 2


async def test_live_non_agent_task_requester_remains_an_authorized_human(tmp_path: Path) -> None:
    graph = FakeGraph()
    replies: list[str] = []

    async def graph_factory(_thread_id: str) -> FakeGraph:
        return graph

    async def send_reply(_message: MatrixMessage, body: str) -> None:
        replies.append(body)

    engine = AgentEngine(
        config=runtime_config(),
        graph_factory=graph_factory,
        send_reply=send_reply,
        pending_store=PendingApprovalStore(tmp_path / "pending.json"),
        is_managed_agent=AsyncMock(return_value=False),
    )
    requester = "@unlisted-human:example.org"
    await engine.handle_message(message(requester, "investigate"))
    await engine.handle_message(message(requester, "approve all"))

    assert replies[-1] == "approved result"
    assert len(graph.invocations) == 2


async def test_snapshot_agent_is_denied_before_live_lookup(tmp_path: Path) -> None:
    graph = FakeGraph()
    replies: list[str] = []
    lookup = AsyncMock(return_value=False)

    async def graph_factory(_thread_id: str) -> FakeGraph:
        return graph

    async def send_reply(_message: MatrixMessage, body: str) -> None:
        replies.append(body)

    engine = AgentEngine(
        config=runtime_config(),
        graph_factory=graph_factory,
        send_reply=send_reply,
        pending_store=PendingApprovalStore(tmp_path / "pending.json"),
        is_managed_agent=lookup,
    )
    await engine.handle_message(message("@operator:example.org", "investigate"))
    await engine.handle_message(message("@worker:example.org", "approve all"))

    assert "not authorized" in replies[-1]
    lookup.assert_not_awaited()
    assert len(graph.invocations) == 1


async def test_managed_agent_identity_client_reads_rotated_token_and_validates_response(tmp_path: Path) -> None:
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, json={"managed": False})

    token_path = tmp_path / "token"
    token_path.write_text("initial-token")
    async with httpx.AsyncClient(transport=httpx.MockTransport(handler), trust_env=False) as client:
        lookup = ManagedAgentIdentityClient(
            controller_url="http://controller:8090",
            worker_name="researcher",
            service_account_token_path=token_path,
            client=client,
        )
        token_path.write_text("first-rotated-token")
        assert await lookup.is_managed_agent("@human:example.org") is False
        token_path.write_text("second-rotated-token")
        assert await lookup.is_managed_agent("@human:example.org") is False

    assert requests[0].url.path == "/api/v1/workers/researcher/managed-agent-identity"
    assert requests[0].headers["Authorization"] == "Bearer first-rotated-token"
    assert requests[1].headers["Authorization"] == "Bearer second-rotated-token"
    assert requests[0].content == b'{"matrixUserId":"@human:example.org"}'


async def test_managed_agent_identity_client_rejects_invalid_or_oversized_responses(tmp_path: Path) -> None:
    token_path = tmp_path / "token"
    token_path.write_text("service-account-token")
    responses = iter(
        (
            httpx.Response(200, json={"managed": "false"}),
            httpx.Response(200, json={"managed": False, "identities": []}),
            httpx.Response(200, content=b"{" + b" " * 2048 + b"}"),
        )
    )

    def handler(_request: httpx.Request) -> httpx.Response:
        return next(responses)

    async with httpx.AsyncClient(transport=httpx.MockTransport(handler), trust_env=False) as client:
        lookup = ManagedAgentIdentityClient(
            controller_url="http://controller:8090",
            worker_name="researcher",
            service_account_token_path=token_path,
            client=client,
        )
        for _ in range(3):
            with pytest.raises(RuntimeError, match="invalid managed-Agent lookup response"):
                await lookup.is_managed_agent("@human:example.org")


def test_pending_approval_store_survives_process_reconstruction(tmp_path: Path) -> None:
    path = tmp_path / "pending.json"
    first = PendingApprovalStore(path)
    first.create(
        thread_id="atd-thread",
        room_id="!room:example.org",
        thread_root_event_id="$root",
        requester="@operator:example.org",
        actions=({"name": "execute", "args": {"command": "true"}},),
    )

    restored = PendingApprovalStore(path).get("atd-thread")

    assert restored is not None
    assert restored.requester == "@operator:example.org"
    assert restored.actions[0]["name"] == "execute"
