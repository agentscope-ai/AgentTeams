from pathlib import Path
from types import SimpleNamespace

from langgraph.types import Command, Interrupt

from deepagents_agentteams.engine import AgentEngine, PendingApprovalStore
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
        human_approver_ids=frozenset({"@operator:example.org"}),
        agent_matrix_ids=frozenset({"@manager:example.org", "@worker:example.org"}),
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
    )

    await engine.handle_message(message("@manager:example.org", "investigate"))
    await engine.handle_message(message("@manager:example.org", "approve all"))
    await engine.handle_message(message("@operator:example.org", "approve all"))

    assert "Approval required" in replies[0]
    assert "not authorized" in replies[1]
    assert replies[2] == "approved result"
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
