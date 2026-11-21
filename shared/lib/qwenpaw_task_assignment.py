"""Attach a late assignment to its already admitted QwenPaw Task without running a turn."""
import asyncio
import copy
import time
from types import SimpleNamespace

from task_recovery import Recovery, RecoveryConflict
from worker_matrix_state import matrix_event_key


class TaskAssignments:
    def __init__(self, execution):
        self.execution = execution
        self.notified = set()
        tracker = execution.channel._workspace.task_tracker
        if not hasattr(tracker, "_agentteams_late_assignments"):
            tracker._agentteams_late_assignments = {}
        self.waiting = tracker._agentteams_late_assignments

    async def acknowledge(self, room, event_id):
        key = matrix_event_key(room, event_id)
        receipts = self.execution.channel._session_receipts
        if receipts is not None and key not in self.notified:
            await receipts.submit(room, [event_id])
            self.notified.add(key)

    async def accept(self, identity, record, text):
        execution = self.execution
        admission = (identity["team_id"], identity["task_id"], identity["generation"])
        key = matrix_event_key(record["room_id"], record["event_id"])
        if admission in execution.native_active:
            # The running SDK owns the session writer. Observe at its next native
            # reasoning boundary; a closed turn is handled by the durable scan.
            self.waiting.setdefault(admission, {})[key] = (record, text)
            return
        await self.attach(identity, record, text)
        self.waiting.pop(admission, None)

    async def before_reasoning(self, ctx, agent):
        identity = (getattr(ctx.request, "channel_meta", {}) or {}).get("task_execution")
        if not identity:
            return
        key = (identity["team_id"], identity["task_id"], identity["generation"])
        pending = self.waiting.get(key, {})
        for source, (record, text) in list(pending.items()):
            # Inputs supplied with the initial turn have already passed the
            # regular REPLY_START native checkpoint before this callback.
            if record["event_id"] not in (ctx.request.channel_meta.get("session_source_event_ids") or []):
                await self.attach(identity, record, text, ctx=ctx, agent=agent)
            pending.pop(source, None)
        if not pending:
            self.waiting.pop(key, None)

    async def attach(self, identity, record, text, *, ctx=None, agent=None):
        from agentscope.message import UserMsg

        execution = self.execution
        channel = execution.channel
        store = channel._workspace.session
        room, event_id = record["room_id"], record["event_id"]
        work = record["work"]
        key = matrix_event_key(room, event_id)
        # Observing a late input inside the current SDK writer is monitoring
        # its owner, not admitting a new execution of an unfinished operation.
        live = ctx is not None and agent is not None
        task = await asyncio.to_thread(execution.verify, identity, monitoring=live)
        if task["room_id"] != room:
            raise RecoveryConflict("Task assignment room changed")
        checkpoint = Recovery.digest(task["execution"].get("checkpoint"))
        view = await asyncio.to_thread(execution.sessions.load, work)
        if (view["generation"] != identity["generation"] or view["instance_id"] != identity["instance_id"]
                or view["checkpoint_digest"] != checkpoint):
            raise RecoveryConflict("Task checkpoint changed before assignment attachment")
        cache = view.get("cache") or {}
        ref = ctx.session_id if ctx else cache.get("native_ref", "")
        if not ref.startswith("matrix:task:" + execution.key(identity["team_id"], identity["task_id"]) + ":"):
            raise RecoveryConflict("No current native Task session for this assignment")
        address = {"session_id": ref, "user_id": room, "channel": channel.channel}
        message_id = "matrix:" + key
        metadata = {"matrix_room_id": room, "matrix_event_id": event_id, "matrix_source": record["input"]}
        message = UserMsg(record["input"]["sender"], text, id=message_id, metadata=metadata)

        def retained(context):
            found = [m for m in context if m.get("id") == message_id]
            if found and (len(found) != 1 or found[0]["metadata"] != metadata
                          or [b.get("text") for b in found[0]["content"]] != [text]):
                raise RecoveryConflict("Native assignment differs from the accepted event")
            return bool(found)

        if agent is not None:
            if not retained([m.model_dump(mode="json") for m in agent.state.context]):
                await agent.observe(message)
            snapshot = {**agent.state_dict(), "mode_state": ctx.mode_state}
            await store.save_session_state(**address, agent=SimpleNamespace(state_dict=lambda: snapshot))
        else:
            native = await store.get_session_state_dict(**address, allow_not_exist=False)
            context = native["agent"]["state"]["context"]
            exists = retained(context)
            digest = Recovery.digest(native)
            if exists and not execution.sessions.usable(view, ref, digest):
                # Recover a lost descriptor CAS after the SDK saved our one input.
                # Removing exactly that message must reconstruct the known bytes.
                original = copy.deepcopy(native)
                original["agent"]["state"]["context"] = [m for m in context if m.get("id") != message_id]
                digest = Recovery.digest(original)
            if not execution.sessions.usable(view, ref, digest):
                raise RecoveryConflict("Native Task session differs from the current shared checkpoint")
            if not exists:
                await store.update_session_state(**address, key=["agent", "state", "context"],
                    value=[*context, message.model_dump(mode="json")], create_if_not_exist=False)

        saved = await store.get_session_state_dict(**address, allow_not_exist=False)
        if not retained(saved["agent"]["state"]["context"]):
            raise RecoveryConflict("SDK did not persist the accepted Task assignment")
        current = await asyncio.to_thread(execution.verify, identity, monitoring=live)
        if Recovery.digest(current["execution"].get("checkpoint")) != checkpoint:
            raise RecoveryConflict("Task checkpoint changed while attaching its assignment")
        digest = Recovery.digest(saved)
        if agent is None and not execution.sessions.usable(view, ref, digest):
            await asyncio.to_thread(execution.sessions.remember, work, view, identity, ref, digest, int(time.time()) + 86400)
        evidence = await asyncio.to_thread(execution.matrix.native_input, key)
        if not execution.matrix.native_current(evidence, ref, digest):
            await asyncio.to_thread(execution.matrix.native_committed, key, evidence, ref, digest)
        await self.acknowledge(room, event_id)
