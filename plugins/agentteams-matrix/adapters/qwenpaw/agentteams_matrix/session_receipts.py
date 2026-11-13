"""Exact Matrix acknowledgements at the durable session-input boundary."""
from __future__ import annotations

import asyncio
import hashlib
import json
import logging
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

    async def flush(self):
        async with self.flush_lock:
            await self._flush()

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
    return SessionReceiptMiddleware(ctx)


class SessionReceiptMiddleware(MiddlewareBase):
    def __init__(self, ctx):
        self.ctx = ctx

    async def on_reply(self, agent, input_kwargs, next_handler):
        checkpointed = False
        async for event in next_handler(**input_kwargs):
            # AgentScope emits this only after validating and appending inputs,
            # before compression, reasoning, tools, or the final reply.
            if getattr(event, "type", None) == "REPLY_START" and not checkpointed:
                checkpointed = True
                await self._checkpoint(agent)
            yield event

    async def _checkpoint(self, agent):
        ctx = self.ctx
        meta = getattr(ctx.request, "channel_meta", {}) or {}
        room_id = meta.get("room_id")
        event_ids = meta.get("session_source_event_ids") or []
        if not room_id or not event_ids or not ctx.workspace:
            return
        channel = await ctx.workspace.channel_manager.get_channel("agentteams_matrix")
        delivery = getattr(channel, "_session_receipts", None)
        if delivery is None:
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
        except Exception:
            # A later turn-end save must not publish a read whose checkpoint
            # failed. Leave the response path running and omit its receipt.
            for entry in added:
                pending.remove(entry)
            logger.warning("Session input checkpoint failed; read receipt omitted", exc_info=True)
            return
        await delivery.submit(room_id, event_ids, pending)
