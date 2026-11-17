"""Bind QwenPaw native turns to durable ordinary Matrix work."""
import asyncio
import json
import logging
import time
from uuid import uuid4

from task_recovery import Recovery, RecoveryConflict, RecoveryUnavailable

log = logging.getLogger(__name__)

class MessageExecution:
    def __init__(self, channel, execution):
        self.channel = channel
        self.matrix = execution.matrix
        self.sessions = execution.sessions
        tracker = channel._workspace.task_tracker
        if not hasattr(tracker, "_agentteams_message_turns"):
            tracker._agentteams_message_turns = {}
            tracker._agentteams_message_admission_lock = asyncio.Lock()
        self.turns = tracker._agentteams_message_turns
        self.admission_lock = tracker._agentteams_message_admission_lock

    async def admit(self, key):
        if key in self.turns:
            return None
        entry = await asyncio.to_thread(self.matrix.message, key)
        if entry["recovery"] in {"backfill", "verify"}:
            return None
        claim = await asyncio.to_thread(self.matrix.message_claim, key)
        mode = claim["recovery"]
        if mode == "deliver":
            await self.deliver(claim)
            return None
        if mode != "resume":
            return None
        identity = claim["identity"]
        work = {"kind": "message", "event_key": key}
        view = await asyncio.to_thread(self.sessions.load, work)
        if (view["generation"] != identity["generation"]
                or view["checkpoint_digest"] != claim["checkpoint_digest"]):
            raise RecoveryConflict("Message changed before native session selection")
        prefix = "matrix:message:" + key + ":"
        session_id = (view.get("cache") or {}).get("native_ref", "")
        if session_id.startswith(prefix):
            from qwenpaw.exceptions import AgentStateError
            try:
                native = await self.native_state(session_id, claim["input"]["room_id"])
            except (AgentStateError, OSError, ValueError, KeyError):
                native = None
            if not native or not native.get("agent") or not self.sessions.usable(view, session_id, Recovery.digest(native)):
                session_id = ""
        else:
            session_id = ""
        await asyncio.to_thread(self.matrix.message_current, identity)
        return {"identity": identity, "session_id": session_id or prefix + uuid4().hex, "rebuild": not session_id,
                "resume_text": claim["resume_text"] if claim["execution"].get("checkpoint") else "",
                "room": claim["input"]["room_id"], "lock": asyncio.Lock()}

    async def native_state(self, session, room):
        return await self.channel._workspace.session.get_session_state_dict(
            session_id=session, user_id=room, channel=self.channel.channel, allow_not_exist=True)

    async def deliver(self, claim):
        identity = claim["identity"]
        cp = claim["execution"]["checkpoint"]
        event_id = await self.channel.send_message_reply(claim["input"], claim["saved_result"], cp["reply_txn"])
        return await asyncio.to_thread(self.matrix.message_delivered, identity, event_id)

    async def finish(self, identity, text):
        if not text.strip():
            raise RecoveryConflict("Ordinary Matrix turn has no final result")
        if text.strip() == "NO_REPLY":
            await asyncio.to_thread(self.matrix.message_cancel, identity)
            return
        saved = await asyncio.to_thread(self.matrix.message_save_reply, identity, text)
        delivered = await self.deliver(saved)
        self.turns[identity["event_key"]]["checkpoint_digest"] = delivered["checkpoint_digest"]

    async def stream(self, turn, native):
        identity = turn["identity"]
        key = identity["event_key"]
        try:
            await asyncio.to_thread(self.matrix.message_current, identity)
            async for item in native:
                yield item
        finally:
            try:
                await native.aclose()
                try:
                    await self.remember(turn)
                except (RecoveryConflict, RecoveryUnavailable, OSError, ValueError):
                    log.warning("QwenPaw could not publish the optional native message cache", exc_info=True)
            finally:
                if self.turns.get(key) is turn:
                    del self.turns[key]

    async def remember(self, turn):
        identity = turn["identity"]
        digest = turn.get("checkpoint_digest")
        if not digest:
            return
        work = {"kind": "message", "event_key": identity["event_key"]}
        view = await asyncio.to_thread(self.sessions.load, work)
        if view["checkpoint_digest"] != digest:
            return
        native = await self.native_state(turn["session_id"], turn["room"])
        if native and native.get("agent"):
            await asyncio.to_thread(self.sessions.remember, work, view, identity, turn["session_id"],
                                    Recovery.digest(native), int(time.time()) + 86400)


async def guarded_tool(matrix, turn, call, next_handler, kwargs):
    """Only the final successful AgentScope ToolResponse proves completion."""
    from agentscope.tool import ToolResponse

    identity = turn["identity"]
    async with turn["lock"]:
        call_id = call.id
        arguments = json.loads(call.input) if isinstance(call.input, str) else call.input
        operation = await asyncio.to_thread(matrix.message_begin_tools, identity, [
            {"id": call_id, "name": call.name, "arguments": arguments}])
        final = None
        async for item in next_handler(**kwargs):
            if final is not None:
                raise RecoveryConflict("Native tool emitted output after its final result")
            if isinstance(item, ToolResponse):
                if item.state != "success":
                    raise RecoveryConflict("Native tool outcome requires verification")
                final = item
            else:
                yield item
        if final is None:
            raise RecoveryConflict("Native tool closed without a confirmed result")
        saved = await asyncio.to_thread(matrix.message_finish_tools, identity, operation, [
            {"id": call_id, "content": final.model_dump(mode="json")["content"]}])
        turn["checkpoint_digest"] = saved["checkpoint_digest"]
        yield final
