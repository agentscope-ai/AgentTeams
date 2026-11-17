"""Authoritative Task admission for QwenPaw; native sessions are caches only."""
from __future__ import annotations

import asyncio
import logging
import time
from uuid import uuid4

from task_recovery import Recovery, RecoveryConflict, RecoveryUnavailable
from worker_matrix_state import WorkerMatrix, matrix_event_key
from worker_sessions import WorkerSessions
from qwenpaw_message_execution import MessageExecution, publish_nio_reply
from qwenpaw_task_assignment import TaskAssignments

logger = logging.getLogger(__name__)


class TaskExecution:
    def __init__(self, channel, recovery=None):
        self.channel = channel
        self.recovery = recovery or Recovery(device_delivery=True)
        tracker = channel._workspace.task_tracker
        if not hasattr(tracker, "_agentteams_task_admissions"):
            tracker._agentteams_task_admissions = set()
            tracker._agentteams_task_admission_lock = asyncio.Lock()
        if not hasattr(tracker, "_agentteams_task_native_active"):
            tracker._agentteams_task_native_active = set()
        self.admitted = tracker._agentteams_task_admissions
        self.native_active = tracker._agentteams_task_native_active
        self.lock = tracker._agentteams_task_admission_lock
        self.sessions = WorkerSessions("qwenpaw", recovery=self.recovery)
        self.matrix = WorkerMatrix("qwenpaw", recovery=self.recovery)
        self.messages = MessageExecution(channel, self)
        self.assignments = TaskAssignments(self)

    async def accept_input(self, room, event, intent, *, task_id=None):
        """Persist an accepted native callback without inventing a batch token."""
        async with self.lock:
            source = event.source
            decoded = {"sender": event.sender, "type": source.get("type", "m.room.message"),
                "origin_server_ts": source.get("origin_server_ts", getattr(event, "server_timestamp", 0)),
                "content": source["content"]}
            return await asyncio.to_thread(self.matrix.accept, room, event.event_id, decoded, intent, task_id=task_id)

    async def commit_inputs(self, session_id, user_id, room, event_ids, required_keys):
        """Publish facts from the SDK's saved bytes, before reasoning/read ACK."""
        keys = {matrix_event_key(room, event_id) for event_id in event_ids}
        if not set(required_keys) <= keys:
            raise RecoveryConflict("Native input facts belong to another Matrix source")
        native = await self.channel._workspace.session.get_session_state_dict(
            session_id=session_id, user_id=user_id, channel=self.channel.channel, allow_not_exist=False)
        pending = native.get("agent", {}).get("state", {}).get("middle_context", {}).get("agentteams_session_receipts", [])
        if any({"room_id": room, "event_id": event_id} not in pending for event_id in event_ids):
            raise RecoveryConflict("Actual native state does not retain these accepted Matrix inputs")
        digest = Recovery.digest(native)
        for event_id in dict.fromkeys(event_ids):
            key = matrix_event_key(room, event_id)
            input_ = await asyncio.to_thread(self.matrix.input, key)
            if input_ is None:
                if key in required_keys:
                    raise RecoveryConflict("Required Matrix input is not durable")
                continue  # Legacy context has no new-standard persistence claim.
            snapshot = await asyncio.to_thread(self.matrix.native_input, key)
            if not self.matrix.native_current(snapshot, session_id, digest):
                await asyncio.to_thread(self.matrix.native_committed, key, snapshot, session_id, digest)

    @staticmethod
    def key(team, task_id):
        return Recovery.digest([team, task_id])

    def verify(self, identity, *, finished=False, monitoring=False):
        task = self.recovery.current(identity["task_id"], identity["team_id"])
        execution = task.get("execution", {}) if task else {}
        operation_state = (((execution.get("checkpoint") or {}).get("operation")) or {}).get("state")
        states = {"active", "released"} if finished or monitoring else {"active"}
        # The Controller marks a durable started operation as needing
        # verification while its admitted native tool is still running.
        # Monitoring that owner must not cancel the tool before it can verify
        # its result. This never authorizes a new admission or unknown effect.
        if monitoring and operation_state == "started":
            states.add("needs_verification")
        if (execution.get("process_id") != self.recovery.process
                or execution.get("generation") != identity["generation"]
                or identity.get("instance_id", execution.get("instance_id")) != execution.get("instance_id")
                or execution.get("runtime") != "qwenpaw"
                or not task or task.get("status") not in {"assigned", "in_progress", "submitted"}
                or execution.get("state") not in states):
            raise RecoveryConflict("Task was cancelled, reassigned or replaced before native execution "
                f"(state={execution.get('state')}, operation={operation_state})")
        return task

    async def select_session(self, team, task):
        key = self.key(team, task["task_id"])
        prefix = "matrix:task:" + key + ":"
        view = await asyncio.to_thread(self.sessions.load,
            {"kind": "task", "team_id": team, "task_id": task["task_id"]})
        if (view["generation"] != task["execution"]["generation"]
                or view["checkpoint_digest"] != Recovery.digest(task["execution"].get("checkpoint"))):
            raise RecoveryConflict("Task checkpoint advanced before native session selection")
        cached = view.get("cache") or {}
        session_id = cached.get("native_ref", "")
        if isinstance(session_id, str) and session_id.startswith(prefix):
            try:
                state = await self.channel._workspace.session.get_session_state_dict(
                    session_id=session_id, user_id=task["room_id"], channel=self.channel.channel,
                )
                if state.get("agent") and self.sessions.usable(view, session_id, Recovery.digest(state)):
                    return session_id
            except Exception:
                logger.warning("Native Task session is unreadable; rebuilding from authority")
        return prefix + uuid4().hex

    async def remember(self, identity, session_id):
        task = await asyncio.to_thread(self.recovery.current, identity["task_id"], identity["team_id"])
        if task is None:
            return
        if task["status"] == "submitted":
            return
        await asyncio.to_thread(self.verify, identity, finished=True)
        state = await self.channel._workspace.session.get_session_state_dict(
            session_id=session_id, user_id=task["room_id"], channel=self.channel.channel,
        )
        if not state.get("agent"):
            return
        work = {"kind": "task", "team_id": identity["team_id"], "task_id": identity["task_id"]}
        try:
            view = await asyncio.to_thread(self.sessions.load, work)
            # Native usage can be saved after the final callback. The snapshot
            # from that callback must still match; never relabel old native
            # bytes with a checkpoint advanced by a later operation.
            if view["checkpoint_digest"] != identity.get("checkpoint_digest"):
                return
            await asyncio.to_thread(self.sessions.remember, work, view, identity,
                session_id, Recovery.digest(state), int(time.time()) + 86400)
        except (RecoveryConflict, RecoveryUnavailable):
            # The saved reply remains authoritative when an optional cache
            # loses its CAS race or storage becomes unavailable.
            logger.warning("Native Task cache was not committed; retain authority and rebuild")

    async def recover(self, only_task=None, source_event=None, *, source_room=None, source_text=None):
        async with self.lock:
            await asyncio.to_thread(self.matrix.start)
            source = None
            if source_event is not None and source_text is not None:
                source = await asyncio.to_thread(self.matrix.input, matrix_event_key(source_room, source_event.event_id))
                if (not source or source.get("intent") != "task_notification"
                        or (source.get("work") or {}).get("kind") != "task"
                        or source["work"]["task_id"] != only_task or source["room_id"] != source_room
                        or source["event_id"] != source_event.event_id or source["input"]["sender"] != source_event.sender
                        or source["input"]["content"] != source_event.source["content"]):
                    raise RecoveryConflict("Task notification differs from its accepted assignment")
            snapshot = await asyncio.to_thread(self.recovery.discover)
            for entry in snapshot["tasks"]:
                task_id, team = entry["task"]["task_id"], entry["team_id"]
                if (only_task is not None and task_id != only_task) or (source and team != source["work"]["team_id"]):
                    continue
                if source_room is not None and entry["task"].get("room_id") != source_room:
                    continue
                execution = entry["task"].get("execution") or {}
                if (execution.get("process_id") == self.recovery.process
                        and execution.get("instance_id") == snapshot["instance_id"]
                        and (team, task_id, execution.get("generation")) in self.admitted):
                    admission = (team, task_id, execution.get("generation"))
                    if source:
                        await self.assignments.accept({"task_id": task_id, "team_id": team,
                            "generation": admission[2], "instance_id": snapshot["instance_id"],
                            "process_id": self.recovery.process}, source, source_text)
                        continue
                    # A live native turn owns its checkpoints and reply. Once
                    # fully closed, only a saved pending reply may be retried.
                    if (admission in self.native_active
                            or (execution.get("checkpoint") or {}).get("delivery") != "pending"):
                        continue
                try:
                    claimed = await asyncio.to_thread(self.recovery.claim, snapshot, entry)
                except RecoveryConflict:
                    # A competing Worker must not prevent unrelated owned work.
                    continue
                task = claimed["task"]
                if task["execution"]["state"] == "needs_verification":
                    logger.warning("Task %s requires side-effect verification before native execution", task_id)
                    continue
                if source_room is not None and task.get("room_id") != source_room:
                    continue
                identity = {"task_id": task_id, "team_id": team,
                            "generation": task["execution"]["generation"],
                            "instance_id": task["execution"]["instance_id"],
                            "process_id": self.recovery.process}
                cp = task["execution"].get("checkpoint") or {}
                if cp.get("delivery") == "pending":
                    try:
                        await self.deliver(identity, task["room_id"], cp, claimed["saved_result"])
                    except (RecoveryConflict, RecoveryUnavailable):
                        logger.warning("Saved Task reply awaits verification for %s", task_id)
                        continue
                    task = await asyncio.to_thread(self.recovery.current, task_id, team)
                    if task is None:
                        continue
                admission = (team, task_id, identity["generation"])
                if task["status"] == "submitted" or admission in self.admitted:
                    continue
                session_id = await self.select_session(team, task)
                await asyncio.to_thread(self.verify, identity)
                await self.channel.enqueue_recovered_task({
                    "recovery_text": Recovery.resume_text({**task, "team_id": team, "spec": claimed["spec"]}),
                    "meta": {"task_execution": identity, "task_session_id": session_id,
                             "task_id": task_id, "team_id": team,
                             "execution_generation": identity["generation"],
                             "room_id": task["room_id"], "sender_id": self.channel.matrix_user_id,
                             "event_id": f"recovery:{team}/{task_id}:{identity['generation']}",
                             "session_source_event_ids": [source_event.event_id] if source_event else [],
                             "shared_input_keys": [matrix_event_key(task["room_id"], source_event.event_id)] if source_event else [],
                             "is_group": True, "is_dm": False},
                })
                self.admitted.add(admission)
                self.native_active.add(admission)

    def native_closed(self, identity):
        self.native_active.discard((identity["team_id"], identity["task_id"], identity["generation"]))

    async def deliver(self, identity, room, checkpoint, text):
        task_id, team = identity["task_id"], identity["team_id"]
        event_id = await publish_nio_reply(self.channel, room, checkpoint, text, identity,
            lambda next_cp: self.recovery.checkpoint(task_id, team, next_cp),
            lambda txn: self.channel.send_task_reply(room, text, txn))
        return await asyncio.to_thread(self.recovery.delivered, task_id, team, event_id)

    async def finish(self, identity, session_id, text):
        if not text:
            task = await asyncio.to_thread(self.verify, identity, finished=True)
            cp = task["execution"].get("checkpoint") or {}
            if task["status"] != "submitted" or not (task.get("result_path") or cp.get("result")):
                raise RuntimeError("Task turn ended without a durable result")
            # submit_task can end native inference without a final message.
            # prepare_reply verifies and reads the authoritative stored report.
        saved, text = await asyncio.to_thread(self.recovery.prepare_reply, identity["task_id"], identity["team_id"], text)
        cp = saved["execution"]["checkpoint"]
        if cp.get("delivery") != "delivered":
            room = saved.get("task", saved)["room_id"]
            saved = await self.deliver(identity, room, cp, text)
        identity["checkpoint_digest"] = Recovery.digest(saved["execution"]["checkpoint"])
