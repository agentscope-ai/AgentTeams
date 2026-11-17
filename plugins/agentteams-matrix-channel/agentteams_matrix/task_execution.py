"""Authoritative Task admission for QwenPaw; native sessions are caches only."""
from __future__ import annotations

import asyncio
import json
import logging
import os
from pathlib import Path
import tempfile
import time
from uuid import uuid4

from task_recovery import Recovery, RecoveryConflict

logger = logging.getLogger(__name__)


class TaskExecution:
    def __init__(self, channel, recovery=None):
        self.channel = channel
        self.recovery = recovery or Recovery()
        tracker = channel._workspace.task_tracker
        if not hasattr(tracker, "_agentteams_task_admissions"):
            tracker._agentteams_task_admissions = set()
            tracker._agentteams_task_admission_lock = asyncio.Lock()
        self.admitted = tracker._agentteams_task_admissions
        self.lock = tracker._agentteams_task_admission_lock
        self.cache_path = Path(channel._workspace.session.save_dir) / ".agentteams-task-recovery"

    @staticmethod
    def key(team, task_id):
        return Recovery.digest([team, task_id])

    def _cache(self):
        try:
            data = json.loads(self.cache_path.read_text())
            return data if isinstance(data, dict) else {}
        except (OSError, ValueError):
            return {}

    def verify(self, identity, *, finished=False):
        task = self.recovery.current(identity["task_id"], identity["team_id"])
        execution = task.get("execution", {}) if task else {}
        if (execution.get("process_id") != self.recovery.process
                or execution.get("generation") != identity["generation"]
                or identity.get("instance_id", execution.get("instance_id")) != execution.get("instance_id")
                or (not finished and execution.get("state") not in {"active", "needs_verification"})):
            raise RecoveryConflict("Task was cancelled, reassigned or replaced before native execution")
        return task

    async def select_session(self, team, task):
        key = self.key(team, task["task_id"])
        prefix = "matrix:task:" + key + ":"
        cached = self._cache().get(key)
        if not isinstance(cached, dict):
            cached = {}
        session_id = cached.get("session_id", "")
        if (isinstance(session_id, str) and session_id.startswith(prefix)
                and Recovery.session_usable(cached, task, "qwenpaw")):
            try:
                state = await self.channel._workspace.session.get_session_state_dict(
                    session_id=session_id, user_id=task["room_id"], channel=self.channel.channel,
                )
                if state.get("agent") and cached.get("native_digest") == Recovery.digest(state):
                    return session_id
            except Exception:
                logger.warning("Native Task session is unreadable; rebuilding from authority")
        return prefix + uuid4().hex

    async def remember(self, identity, session_id):
        task = await asyncio.to_thread(self.recovery.current, identity["task_id"], identity["team_id"])
        if task is None:
            return
        await asyncio.to_thread(self.verify, identity, finished=True)
        state = await self.channel._workspace.session.get_session_state_dict(
            session_id=session_id, user_id=task["room_id"], channel=self.channel.channel,
        )
        if not state.get("agent"):
            return
        cache = self._cache()
        cache[self.key(identity["team_id"], identity["task_id"])] = {
            "runtime": "qwenpaw", "session_id": session_id, "expires_at": time.time() + 86400,
            "checkpoint_digest": Recovery.digest(task["execution"].get("checkpoint")),
            "native_digest": Recovery.digest(state),
        }
        self.cache_path.parent.mkdir(parents=True, exist_ok=True)
        candidate = None
        try:
            with tempfile.NamedTemporaryFile(mode="w", dir=self.cache_path.parent, delete=False) as output:
                candidate = Path(output.name)
                json.dump(cache, output)
                output.flush()
                os.fsync(output.fileno())
            candidate.replace(self.cache_path)
        finally:
            if candidate is not None:
                candidate.unlink(missing_ok=True)

    async def recover(self, only_task=None, source_event=None, *, source_room=None):
        async with self.lock:
            snapshot = await asyncio.to_thread(self.recovery.discover)
            for entry in snapshot["tasks"]:
                task_id, team = entry["task"]["task_id"], entry["team_id"]
                if only_task is not None and task_id != only_task:
                    continue
                if source_room is not None and entry["task"].get("room_id") != source_room:
                    continue
                try:
                    claimed = await asyncio.to_thread(self.recovery.claim, snapshot, entry)
                except RecoveryConflict:
                    # A competing Worker must not prevent unrelated owned work.
                    continue
                task = claimed["task"]
                if source_room is not None and task.get("room_id") != source_room:
                    continue
                identity = {"task_id": task_id, "team_id": team,
                            "generation": task["execution"]["generation"],
                            "instance_id": task["execution"]["instance_id"]}
                cp = task["execution"].get("checkpoint") or {}
                if cp.get("delivery") == "pending":
                    event_id = await self.channel.send_task_reply(task["room_id"], claimed["saved_result"], cp["reply_txn"])
                    await asyncio.to_thread(self.recovery.delivered, task_id, team, event_id)
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
                             "room_id": task["room_id"], "sender_id": self.channel.matrix_user_id,
                             "event_id": f"recovery:{team}/{task_id}:{identity['generation']}",
                             "session_source_event_ids": [source_event.event_id] if source_event else [],
                             "is_group": True, "is_dm": False},
                })
                self.admitted.add(admission)

    async def finish(self, identity, session_id, text):
        saved, text = await asyncio.to_thread(self.recovery.prepare_reply, identity["task_id"], identity["team_id"], text)
        cp = saved["execution"]["checkpoint"]
        if cp.get("delivery") != "delivered":
            room = saved.get("task", saved)["room_id"]
            event_id = await self.channel.send_task_reply(room, text, cp["reply_txn"])
            await asyncio.to_thread(self.recovery.delivered, identity["task_id"], identity["team_id"], event_id)
        await self.remember(identity, session_id)
