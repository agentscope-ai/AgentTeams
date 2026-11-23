"""Exact Matrix acknowledgements at the durable session-input boundary."""
from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import os
from pathlib import Path
from types import SimpleNamespace
from urllib.parse import quote

from agentscope.middleware import MiddlewareBase

logger = logging.getLogger(__name__)
RECEIPT_TYPE = "com.agentteams.session_receipt"
PENDING_KEY = "agentteams_session_receipts"


class SessionReceipts:
    """Deliver idempotent receipts; recover pending reads from saved sessions."""

    def __init__(self, channel, session_dir: Path):
        self.channel = channel
        self.session_dir = session_dir
        self.pending = {}
        self.flush_lock = asyncio.Lock()
        self.wakeup = asyncio.Event()
        self.task = None
        self._manager_outbox = None
        if os.getenv("AGENTTEAMS_AGENT_ROLE") == "manager":
            from agentteams_receipts import Receipts

            self._manager_outbox = Receipts(os.getenv("AGENTTEAMS_WORKER_HOME") or Path.home())

    async def start(self):
        for path in self.session_dir.rglob("*.json"):
            try:
                data = json.loads(await asyncio.to_thread(path.read_text))
                pending = data.get("agent", {}).get("state", {}).get("middle_context", {}).get(PENDING_KEY, [])
                for entry in pending:
                    await self.submit(entry["room_id"], [entry["event_id"]])
            except (OSError, ValueError, KeyError, TypeError, AttributeError):
                logger.warning("Could not recover session receipts from %s", path.name)
        self.task = asyncio.create_task(self._run())

    async def stop(self):
        if self.task:
            self.task.cancel()
            try:
                await self.task
            except asyncio.CancelledError:
                pass

    async def submit(self, room_id, event_ids, pending=None, status="read"):
        for event_id in dict.fromkeys(event_ids):
            if not event_id:
                continue
            key = (room_id, event_id, status)
            self.pending.setdefault(key, [])
            if pending is not None and all(ref is not pending for ref in self.pending[key]):
                self.pending[key].append(pending)
        self.wakeup.set()

    async def complete(self, room_id, event_id, *, trace_context=None):
        if self._manager_outbox:
            await asyncio.to_thread(self._manager_outbox.enqueue, room_id, event_id, "completed", trace_context=trace_context)
            self.wakeup.set()

    async def flush(self):
        async with self.flush_lock:
            await self._flush()
            if self._manager_outbox and self.channel._client:
                from agentteams_receipts import matrix_request

                request = matrix_request(self.channel.homeserver, self.channel._client.access_token)
                await asyncio.to_thread(self._manager_outbox.flush, request)

    async def _flush(self):
        for key in list(self.pending):
            room_id, event_id, status = key
            txn = hashlib.sha256(json.dumps(key).encode()).hexdigest()
            url = f"{self.channel.homeserver}/_matrix/client/v3/rooms/{quote(room_id, safe='')}/send/{RECEIPT_TYPE}/{txn}"
            try:
                response = await self.channel._http_client.put(
                    url,
                    headers={"Authorization": f"Bearer {self.channel._client.access_token}"},
                    json={"version": 1, "status": status, "m.relates_to": {"rel_type": RECEIPT_TYPE, "event_id": event_id}},
                    timeout=5,
                )
                response.raise_for_status()
            except Exception:
                logger.debug("Session receipt delivery deferred for %s", event_id, exc_info=True)
                continue
            for pending in self.pending.pop(key):
                entry = {"room_id": room_id, "event_id": event_id}
                if entry in pending:
                    pending.remove(entry)

    async def _run(self):
        while True:
            self.wakeup.clear()
            await self.flush()
            try:
                await asyncio.wait_for(self.wakeup.wait(), timeout=10)
            except asyncio.TimeoutError:
                pass


def session_receipt_middleware(ctx, _agent_config):
    if getattr(ctx.request, "channel", "") != "agentteams_matrix":
        return None
    middleware = SessionReceiptMiddleware(ctx)
    ctx.extras["agentteams_checkpoint_inputs"] = middleware.checkpoint_inputs
    return middleware


class SessionReceiptMiddleware(MiddlewareBase):
    def __init__(self, ctx):
        self.ctx = ctx

    async def _message_turn(self):
        identity = (getattr(self.ctx.request, "channel_meta", None) or {}).get("message_execution")
        if not identity:
            return None
        from task_recovery import RecoveryConflict
        channel = await self.ctx.workspace.channel_manager.get_channel("agentteams_matrix")
        messages = channel._task_execution.messages
        turn = messages.turns.get(identity["event_key"])
        if not turn or turn["identity"] != identity:
            raise RecoveryConflict("Native Matrix message was replaced before model or tool admission")
        return messages.matrix, turn

    async def on_reasoning(self, agent, input_kwargs, next_handler):
        if (getattr(self.ctx.request, "channel_meta", {}) or {}).get("task_execution"):
            channel = await self.ctx.workspace.channel_manager.get_channel("agentteams_matrix")
            await channel._task_execution.assignments.before_reasoning(self.ctx, agent)
        managed = await self._message_turn()
        if managed:
            matrix, turn = managed
            await asyncio.to_thread(matrix.message_current, turn["identity"])
        async for item in next_handler(**input_kwargs):
            yield item

    async def on_acting(self, agent, input_kwargs, next_handler):
        admission = self.ctx.extras.get('agentteams_canonical_admit')
        if admission:
            await admission(agent)
        managed = await self._message_turn()
        if not managed:
            identity = (getattr(self.ctx.request, 'channel_meta', None) or {}).get('task_execution')
            if identity:
                channel = await self.ctx.workspace.channel_manager.get_channel('agentteams_matrix')
                execution = channel._task_execution
                await asyncio.to_thread(execution.verify, identity)
                turn = self.ctx.extras.setdefault('agentteams_task_tool_turn',
                    {'identity': identity, 'lock': asyncio.Lock()})
                managed = execution.recovery, turn
        if managed:
            call = input_kwargs["tool_call"]
            if call.name == "run_tool_batch":
                from agentscope.message import TextBlock
                from agentscope.tool import ToolResponse

                # Toolkit subcalls in this SDK bypass Agent acting middleware.
                # Decline before opening a journal entry or invoking any child.
                yield ToolResponse(state="error", content=[TextBlock(
                    text="Batch execution is unavailable for this managed turn. Call each required tool directly.")])
                return
            from qwenpaw_message_execution import guarded_tool, read_only_registration
            binding = self.ctx.extras.get('agentteams_task_registration')
            registration = await binding(agent, call) if binding else None
            read_only = call.name == "read_file" and await read_only_registration(agent, call)
            stream = guarded_tool(*managed, call, next_handler, input_kwargs,
                                  registration=registration, read_only=read_only)
        else:
            stream = next_handler(**input_kwargs)
        async for item in stream:
            yield item

    async def on_model_call(self, agent, input_kwargs, next_handler):
        admission = self.ctx.extras.get('agentteams_canonical_admit')
        if admission:
            await admission(agent)
        managed = await self._message_turn()
        identity = (getattr(self.ctx.request, 'channel_meta', None) or {}).get('task_execution')
        if managed or identity:
            from task_operation import admit

        if managed:
            matrix, turn = managed
            snapshot = await asyncio.to_thread(matrix.message_current, turn['identity'])
            admit(snapshot['execution'].get('checkpoint'))
        elif identity:
            channel = await self.ctx.workspace.channel_manager.get_channel('agentteams_matrix')
            task = await asyncio.to_thread(channel._task_execution.recovery.tools_current, identity)
            admit(task['execution'].get('checkpoint'))
        if (managed or identity) and input_kwargs.get('tools'):
            input_kwargs = {**input_kwargs, 'tools': [
                schema for schema in input_kwargs['tools']
                if (schema.get('function') or {}).get('name') != 'run_tool_batch']}
        return await next_handler(**input_kwargs)

    async def on_reply(self, agent, input_kwargs, next_handler):
        checkpointed = False
        async for event in next_handler(**input_kwargs):
            # AgentScope emits this only after validating and appending inputs,
            # before compression, reasoning, tools, or the final reply.
            if getattr(event, "type", None) == "REPLY_START" and not checkpointed:
                checkpointed = True
                await self._checkpoint(agent)
            yield event

    async def checkpoint_inputs(self, agent, sources):
        meta = getattr(self.ctx.request, "channel_meta", {}) or {}
        if not sources or any(source["roomId"] != meta.get("room_id") for source in sources):
            raise ValueError("Verified Human inputs must belong to the current Matrix room")
        meta["session_source_event_ids"] = list(dict.fromkeys([
            *(meta.get("session_source_event_ids") or []), *(source["eventId"] for source in sources)]))
        if not await self._checkpoint(agent, strict=True):
            raise RuntimeError("Native Human input checkpoint support is unavailable")

    async def _checkpoint(self, agent, *, strict=False):
        ctx = self.ctx
        meta = getattr(ctx.request, "channel_meta", {}) or {}
        room_id = meta.get("room_id")
        event_ids = meta.get("session_source_event_ids") or []
        if not room_id or not event_ids or not ctx.workspace:
            return
        channel = await ctx.workspace.channel_manager.get_channel("agentteams_matrix")
        delivery = getattr(channel, "_session_receipts", None)
        required = meta.get("shared_input_keys") or []
        execution = getattr(channel, "_task_execution", None)
        if required and execution is None:
            raise RuntimeError("Managed Matrix input persistence adapter is unavailable")
        if delivery is None and not required:
            return
        pending = agent.state.middle_context.setdefault(PENDING_KEY, [])
        added = []
        for event_id in dict.fromkeys(event_ids):
            entry = {"room_id": room_id, "event_id": event_id}
            if entry not in pending:
                pending.append(entry)
                added.append(entry)
        snapshot = agent.state_dict()
        snapshot["mode_state"] = ctx.mode_state
        proxy = SimpleNamespace(state_dict=lambda: snapshot)
        try:
            await ctx.workspace.session.save_session_state(
                session_id=ctx.session_id,
                user_id=getattr(ctx.request, "user_id", "") or ctx.session_id,
                channel="agentteams_matrix",
                agent=proxy,
            )
            if required and execution is not None:
                await execution.commit_inputs(ctx.session_id,
                    getattr(ctx.request, "user_id", "") or ctx.session_id, room_id, event_ids, required)
        except Exception:
            # A later turn-end save must not publish a read whose checkpoint
            # failed. Required managed inputs also stop before native inference.
            for entry in added:
                pending.remove(entry)
            logger.warning("Session input checkpoint failed; read receipt omitted", exc_info=True)
            if strict or required and execution is not None:
                raise  # A verified new input must not enter inference unrecorded.
            return
        if delivery is not None:
            await delivery.submit(room_id, event_ids, pending)
        return True
