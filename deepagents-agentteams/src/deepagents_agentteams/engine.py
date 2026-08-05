"""Matrix-to-LangGraph orchestration with persistent human approval state."""

from __future__ import annotations

import asyncio
import json
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import asdict, dataclass, replace
from pathlib import Path
from typing import Any
from urllib.parse import quote

import httpx
from langgraph.types import Command

from deepagents_agentteams.approvals import ApprovalPrincipals, MatrixDecision, parse_matrix_decision
from deepagents_agentteams.config import RuntimeConfig
from deepagents_agentteams.matrix import MatrixMessage
from deepagents_agentteams.threads import checkpoint_thread_id


@dataclass(frozen=True)
class PendingApproval:
    """Durable Matrix context for one interrupted LangGraph thread."""

    thread_id: str
    room_id: str
    thread_root_event_id: str
    requester: str
    actions: tuple[dict[str, Any], ...]
    decisions: tuple[dict[str, Any] | None, ...]


class PendingApprovalStore:
    """Small atomic JSON store for pending human decisions."""

    def __init__(self, path: Path) -> None:
        self._path = path
        self._items = self._load()

    def create(
        self,
        *,
        thread_id: str,
        room_id: str,
        thread_root_event_id: str,
        requester: str,
        actions: tuple[dict[str, Any], ...],
    ) -> PendingApproval:
        pending = PendingApproval(
            thread_id=thread_id,
            room_id=room_id,
            thread_root_event_id=thread_root_event_id,
            requester=requester,
            actions=actions,
            decisions=tuple(None for _action in actions),
        )
        self._items[thread_id] = pending
        self._save()
        return pending

    def get(self, thread_id: str) -> PendingApproval | None:
        return self._items.get(thread_id)

    def put(self, pending: PendingApproval) -> None:
        self._items[pending.thread_id] = pending
        self._save()

    def delete(self, thread_id: str) -> None:
        if self._items.pop(thread_id, None) is not None:
            self._save()

    def _load(self) -> dict[str, PendingApproval]:
        if not self._path.exists():
            return {}
        payload = json.loads(self._path.read_text())
        items: dict[str, PendingApproval] = {}
        for thread_id, item in payload.items():
            items[thread_id] = PendingApproval(
                thread_id=thread_id,
                room_id=item["room_id"],
                thread_root_event_id=item["thread_root_event_id"],
                requester=item["requester"],
                actions=tuple(item["actions"]),
                decisions=tuple(item["decisions"]),
            )
        return items

    def _save(self) -> None:
        self._path.parent.mkdir(parents=True, exist_ok=True)
        temporary = self._path.with_suffix(".tmp")
        temporary.write_text(
            json.dumps(
                {thread_id: asdict(item) for thread_id, item in sorted(self._items.items())},
                sort_keys=True,
            )
        )
        temporary.chmod(0o600)
        temporary.replace(self._path)


class ManagedAgentIdentityClient:
    """Ask the controller one live managed-Agent identity question at a time."""

    _MAX_RESPONSE_BYTES = 1024

    def __init__(
        self,
        *,
        controller_url: str,
        worker_name: str,
        service_account_token_path: Path,
        client: httpx.AsyncClient,
    ) -> None:
        if not controller_url:
            raise ValueError("controller URL is required for managed-Agent identity lookup")
        if not worker_name:
            raise ValueError("worker name is required for managed-Agent identity lookup")
        self._controller_url = controller_url.rstrip("/")
        self._worker_name = worker_name
        self._service_account_token_path = service_account_token_path
        self._client = client

    async def is_managed_agent(self, matrix_user_id: str) -> bool:
        """Return the controller's current answer without caching identity state."""
        service_account_token = (
            await asyncio.to_thread(self._service_account_token_path.read_text)
        ).strip()
        if not service_account_token:
            raise RuntimeError("ServiceAccount token is empty")
        worker = quote(self._worker_name, safe="")
        response = await self._client.post(
            f"{self._controller_url}/api/v1/workers/{worker}/managed-agent-identity",
            json={"matrixUserId": matrix_user_id},
            headers={"Authorization": f"Bearer {service_account_token}"},
        )
        response.raise_for_status()
        if len(response.content) > self._MAX_RESPONSE_BYTES:
            raise RuntimeError("controller returned an invalid managed-Agent lookup response")
        try:
            payload = response.json()
        except ValueError as exc:
            raise RuntimeError("controller returned an invalid managed-Agent lookup response") from exc
        if (
            not isinstance(payload, dict)
            or set(payload) != {"managed"}
            or not isinstance(payload["managed"], bool)
        ):
            raise RuntimeError("controller returned an invalid managed-Agent lookup response")
        return payload["managed"]


class AgentEngine:
    """Serialize messages per Matrix thread and drive DeepAgents interrupts."""

    def __init__(
        self,
        *,
        config: RuntimeConfig,
        graph_factory: Callable[[str], Awaitable[Any]],
        send_reply: Callable[[MatrixMessage, str], Awaitable[None]],
        pending_store: PendingApprovalStore,
        is_managed_agent: Callable[[str], Awaitable[bool]],
    ) -> None:
        self._config = config
        self._graph_factory = graph_factory
        self._send_reply = send_reply
        self._pending_store = pending_store
        self._is_managed_agent = is_managed_agent
        self._graphs: dict[str, Any] = {}
        self._locks: dict[str, asyncio.Lock] = {}

    async def handle_message(self, message: MatrixMessage) -> None:
        """Handle a task or approval reply in its checkpoint-scoped critical section."""
        thread_id = checkpoint_thread_id(
            worker_uid=self._config.worker_uid,
            room_id=message.room_id,
            thread_root_event_id=message.thread_root_event_id,
        )
        lock = self._locks.setdefault(thread_id, asyncio.Lock())
        async with lock:
            pending = self._pending_store.get(thread_id)
            if pending is not None:
                await self._handle_decision(message, pending)
                return
            graph = await self._graph(thread_id)
            run_config = _run_config(thread_id)
            result = await graph.ainvoke(
                {"messages": [{"role": "user", "content": message.body}]},
                config=run_config,
            )
            await self._emit_result(
                graph=graph,
                result=result,
                run_config=run_config,
                message=message,
                requester=message.sender,
            )

    async def _handle_decision(self, message: MatrixMessage, pending: PendingApproval) -> None:
        principals = ApprovalPrincipals(
            requester=pending.requester,
            team_admins=self._config.human_approver_ids,
            coordinators=frozenset(self._config.approvals.coordinators),
        )
        if message.sender in self._config.agent_matrix_ids:
            await self._send_reply(message, "This Matrix identity is not authorized to decide this approval.")
            return
        try:
            managed_agent = await self._is_managed_agent(message.sender)
        except Exception:  # noqa: BLE001 - any controller/transport/validation failure denies approval.
            await self._send_reply(
                message,
                "Approval authorization is temporarily unavailable; no decision was applied.",
            )
            return
        if managed_agent:
            await self._send_reply(message, "This Matrix identity is not authorized to decide this approval.")
            return
        if message.sender == pending.requester or message.sender in self._config.human_approver_ids:
            identity_kind = "human"
        else:
            identity_kind = "unknown"
        if not principals.can_decide(sender=message.sender, identity_kind=identity_kind):
            await self._send_reply(message, "This Matrix identity is not authorized to decide this approval.")
            return
        try:
            decision = parse_matrix_decision(message.body)
            updated = _apply_decision(pending, decision)
        except ValueError as exc:
            await self._send_reply(message, f"Invalid approval reply: {exc}")
            return
        self._pending_store.put(updated)
        if any(item is None for item in updated.decisions):
            complete = sum(item is not None for item in updated.decisions)
            await self._send_reply(message, f"Decision recorded ({complete}/{len(updated.decisions)}).")
            return

        graph = await self._graph(updated.thread_id)
        run_config = _run_config(updated.thread_id)
        result = await graph.ainvoke(
            Command(resume={"decisions": list(updated.decisions)}),
            config=run_config,
        )
        self._pending_store.delete(updated.thread_id)
        await self._emit_result(
            graph=graph,
            result=result,
            run_config=run_config,
            message=message,
            requester=updated.requester,
        )

    async def _emit_result(
        self,
        *,
        graph: Any,
        result: Mapping[str, Any],
        run_config: dict[str, Any],
        message: MatrixMessage,
        requester: str,
    ) -> None:
        state = await graph.aget_state(run_config)
        interrupts = tuple(getattr(state, "interrupts", ()) or ())
        if interrupts:
            value = interrupts[0].value
            actions_value = value.get("action_requests", []) if isinstance(value, Mapping) else []
            if not isinstance(actions_value, list) or not actions_value:
                raise RuntimeError("DeepAgents returned an interrupt without action requests")
            actions = tuple(_validated_action(item) for item in actions_value)
            self._pending_store.create(
                thread_id=run_config["configurable"]["thread_id"],
                room_id=message.room_id,
                thread_root_event_id=message.thread_root_event_id,
                requester=requester,
                actions=actions,
            )
            await self._send_reply(message, _render_approval_request(actions))
            return
        await self._send_reply(message, _last_agent_text(result) or "Task completed.")

    async def _graph(self, thread_id: str) -> Any:
        graph = self._graphs.get(thread_id)
        if graph is None:
            graph = await self._graph_factory(thread_id)
            self._graphs[thread_id] = graph
        return graph


def _run_config(thread_id: str) -> dict[str, Any]:
    return {"configurable": {"thread_id": thread_id}}


def _validated_action(value: object) -> dict[str, Any]:
    if not isinstance(value, Mapping):
        raise RuntimeError("DeepAgents approval action must be an object")
    name = value.get("name")
    args = value.get("args")
    if not isinstance(name, str) or not name or not isinstance(args, Mapping):
        raise RuntimeError("DeepAgents approval action must contain a name and object arguments")
    return {"name": name, "args": dict(args)}


def _apply_decision(pending: PendingApproval, decision: MatrixDecision) -> PendingApproval:
    decisions = list(pending.decisions)
    if decision.action == "approve_all":
        return replace(pending, decisions=tuple({"type": "approve"} for _action in pending.actions))
    if decision.index is None or decision.index > len(decisions):
        raise ValueError("approval action index is out of range")
    index = decision.index - 1
    if decision.action == "approve":
        decisions[index] = {"type": "approve"}
    elif decision.action == "reject":
        decisions[index] = {"type": "reject", "message": decision.reason or "Rejected by a Human"}
    elif decision.action == "edit":
        decisions[index] = {
            "type": "edit",
            "edited_action": {
                "name": pending.actions[index]["name"],
                "args": decision.edited_arguments or {},
            },
        }
    else:
        raise ValueError("unsupported approval decision")
    return replace(pending, decisions=tuple(decisions))


def _render_approval_request(actions: tuple[dict[str, Any], ...]) -> str:
    lines = ["Approval required before these actions can run:"]
    for index, action in enumerate(actions, start=1):
        arguments = json.dumps(action["args"], ensure_ascii=False, sort_keys=True)
        if len(arguments) > 800:
            arguments = arguments[:797] + "..."
        lines.append(f"{index}. {action['name']} {arguments}")
    lines.extend(
        (
            "Reply with: approve <n> | reject <n> <reason> | edit <n> <JSON> | approve all",
            "Only an explicitly authorized Human identity can decide.",
        )
    )
    return "\n".join(lines)


def _last_agent_text(result: Mapping[str, Any]) -> str:
    messages = result.get("messages", [])
    if not isinstance(messages, list):
        return ""
    for message in reversed(messages):
        message_type = getattr(message, "type", None)
        if message_type not in {None, "ai"}:
            continue
        content = getattr(message, "content", "")
        if isinstance(content, str):
            return content[:12_000]
        if isinstance(content, list):
            text = "\n".join(
                str(block.get("text", ""))
                for block in content
                if isinstance(block, Mapping) and block.get("type") == "text"
            )
            if text:
                return text[:12_000]
    return ""
