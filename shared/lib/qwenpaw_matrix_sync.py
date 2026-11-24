"""QwenPaw's matrix-nio binding for the standard Worker Matrix consumer."""
import asyncio
import hashlib
import json
import logging

from nio import (Event, MegolmEvent, RoomEncryptedAudio, RoomEncryptedFile, RoomEncryptedImage,
                 RoomEncryptedVideo, RoomContextResponse, RoomMessageAudio, RoomMessageFile, RoomMessageImage,
                 RoomMessageNotice, RoomMessageText, RoomMessageVideo, RoomMessagesResponse, SyncResponse)

from task_recovery import RecoveryConflict
from worker_matrix_history import backfill_round
from worker_matrix_state import matrix_event_key

log = logging.getLogger(__name__)


class ManagedNioSync:
    def __init__(self, channel, execution):
        self.channel = channel
        self.client = channel._client
        self.matrix = execution.matrix
        self.lock = execution.lock
        self.messages = getattr(execution, "messages", None)
        self.ready = False
        self.tasks_scanned = False
        # Typed events await their batch/page commit and any prior room gap.
        self.waiting = {}
        self.assignments = getattr(execution, "assignments", None)

    def task_scan_complete(self):
        if not self.tasks_scanned:
            self.tasks_scanned = True
            if self.ready:
                from worker_handoff import recovery_scanned
                recovery_scanned()

    def handler(self, event):
        if isinstance(event, RoomMessageText):
            return self.channel._on_room_event
        if isinstance(event, RoomMessageNotice):
            control = (getattr(event, "source", {}) or {}).get("content", {}).get("org.agentteams.coordination")
            meta = control.get("channel_meta", {}) if isinstance(control, dict) else {}
            assignment = meta.get("teamharness_coordination", {}) if isinstance(meta, dict) else {}
            if isinstance(assignment, dict) and assignment.get("kind") == "assistant_digest":
                return self.channel._on_room_event
        if isinstance(event, (RoomEncryptedImage, RoomEncryptedAudio, RoomEncryptedVideo, RoomEncryptedFile)):
            return self.channel._on_room_encrypted_media_event
        if isinstance(event, (RoomMessageImage, RoomMessageAudio, RoomMessageVideo, RoomMessageFile)):
            return self.channel._on_room_media_event
        if isinstance(event, MegolmEvent) or getattr(event, "source", {}).get("type") == "m.room.encrypted":
            raise RecoveryConflict("Matrix timeline still contains an undecrypted event")
        return None

    async def prepare(self, room, event, intent, *, task_id=None):
        source = {"sender": event.sender, "type": event.source["type"],
                  "origin_server_ts": event.server_timestamp, "content": event.source["content"]}
        return await asyncio.to_thread(self.matrix.prepare_input, room, event.event_id, source, intent, task_id=task_id)

    async def acl_denial(self, room, sender, body):
        channel = self.channel
        if not channel.access_control_enabled:
            return None
        is_dm = await channel._is_dm_room(room, sender, self.client.rooms[room])
        if not (channel.access_control_dm if is_dm else channel.access_control_group):
            return None
        if channel._get_acl_store().is_whitelisted(channel.channel, sender):
            return None
        return {"sender_id": sender, "acl_sender_id": sender,
                "meta": {"room_id": room, "is_group": not is_dm},
                "content_parts": [{"type": "text", "text": body}]}

    async def inputs(self, room, events, until=None, *, denials=None):
        async def prepare(room, event, intent, *, task_id=None):
            # Task notifications have their own Controller authorization.
            # The SDK's normal ACL gate has side effects (pending request and
            # deny notice), so defer those until after committing a live batch.
            if intent != "task_notification":
                blocked = await self.acl_denial(room, event.sender, event.body or "")
                if blocked:
                    if denials is not None and intent != "context":
                        denials.append(blocked)
                    return None
            return await self.prepare(room, event, intent, task_id=task_id)

        result = []
        for event in events:
            handler = self.handler(event)
            if not handler:
                continue
            key = matrix_event_key(room, event.event_id)
            previous = await asyncio.to_thread(self.matrix.input, key)
            if previous:
                # Verify the decoded source against immutable stored bytes, even
                # if new membership or assignment would classify it differently.
                item = await self.prepare(room, event, previous["intent"])
            else:
                item = await handler(self.client.rooms[room], event, prepare=prepare)
                if item:
                    self.waiting[key] = (room, event)
            if item:
                result.append(item)
            if until and event.event_id == until:
                break
        return result

    async def fetch_page(self, room, token):
        response = await self.client.room_messages(room, start=token)
        if not isinstance(response, RoomMessagesResponse):
            raise RecoveryConflict("Matrix history request failed: " + type(response).__name__)
        if response.room_id != room or response.start != token:
            raise RecoveryConflict("Matrix history returned a different pagination boundary")
        return response.chunk, response.end

    async def room_context(self, record):
        limit = self.channel.history_limit
        if not limit:
            return ""
        room, event_id = record["room_id"], record["event_id"]
        response = await self.client.room_context(room, event_id, limit=limit)
        if (not isinstance(response, RoomContextResponse) or response.room_id != room
                or getattr(response.event, "event_id", None) != event_id):
            raise RecoveryConflict("Could not establish the source event's room history")
        history = []
        for event in reversed(response.events_before):
            if not isinstance(event, RoomMessageText):
                continue
            saved = await asyncio.to_thread(self.matrix.input, matrix_event_key(room, event.event_id))
            if saved and saved["intent"] in {"message", "context"}:
                if (saved["input"]["content"] != event.source["content"]
                        or saved["input"]["sender"] != event.sender):
                    continue
                sender = event.sender
            elif event.sender == self.client.user_id:
                # Accept a previous assistant reply only when both the Matrix
                # event and its text match the durable delivery receipt.
                parent = event.source["content"].get("m.relates_to", {}).get("m.in_reply_to", {}).get("event_id")
                if not parent:
                    continue
                prior = await asyncio.to_thread(self.matrix.input, matrix_event_key(room, parent))
                if not prior:
                    continue
                cp = (prior.get("execution") or {}).get("checkpoint") or {}
                if (cp.get("reply_event") != event.event_id or
                        (cp.get("result") or {}).get("sha256") != hashlib.sha256(event.body.encode()).hexdigest()):
                    continue
                sender = prior["input"]["sender"]
            else:
                continue
            if await self.acl_denial(room, sender, ""):
                continue
            if self.channel.share_session_in_group or sender == record["input"]["sender"]:
                history.append({"sender": event.sender, "event_id": event.event_id, "body": event.body})
        return "Earlier messages in this room:\n" + json.dumps(history, ensure_ascii=False) if history else ""

    async def dispatch_message(self, key, room, record, waiting):
        # The typed event is optional. A replacement reconstructs
        # the accepted input from durable decoded bytes.
        event = waiting[1] if waiting else Event.parse_event({
            **record["input"], "room_id": room, "event_id": record["event_id"]})
        handler = self.handler(event)
        if handler and not await self.acl_denial(room, event.sender, event.body or ""):
            async with self.messages.admission_lock:
                control = record["input"]["content"].get("com.agentteams.control")
                steering = (hasattr(self.channel, "_handle_direct_control")
                            and isinstance(control, dict) and control.get("action") == "steer")
                if steering or not any(turn["room"] == room for turn in self.messages.turns.values()):
                    turn = await self.messages.admit(key)
                    if turn:
                        if turn["rebuild"]:
                            turn["history"] = await self.room_context(record)
                        await handler(self.client.rooms[room], event, input_key=key, turn=turn)

    async def dispatch(self, snapshot):
        deferred_rooms = set()
        if self.assignments:
            self.assignments.notified.intersection_update(snapshot["state"]["pending"])
        for key in snapshot["state"]["pending"]:
            waiting = self.waiting.get(key)
            if self.messages:
                record = await asyncio.to_thread(self.matrix.input, key)
                if not record:
                    raise RecoveryConflict("Shared pending input is unavailable")
                if record["intent"] == "task_notification":
                    room = record["room_id"]
                    commit = record.get("native_commit") or {}
                    if key in self.assignments.notified or room not in self.client.rooms or snapshot["state"]["rooms"].get(room, {}).get("gaps"):
                        continue
                    if commit and commit["start"] == snapshot["state"]["active_start"]:
                        await self.assignments.acknowledge(room, record["event_id"])
                        self.waiting.pop(key, None)
                        continue
                    event = waiting[1] if waiting else Event.parse_event({
                        **record["input"], "room_id": room, "event_id": record["event_id"]})
                    try:
                        await self.channel._on_room_event(self.client.rooms[room], event, input_key=key)
                    except Exception as error:
                        log.warning("Task notification pending in %s: %s", room, type(error).__name__)
                    continue
                if record["intent"] == "message":
                    room = record["room_id"]
                    if (room not in self.client.rooms or room in deferred_rooms
                            or snapshot["state"]["rooms"].get(room, {}).get("gaps")):
                        continue
                    try:
                        await self.dispatch_message(key, room, record, waiting)
                    except Exception as error:
                        # A failed room history fetch or pending delivery must
                        # not starve other rooms in the same Worker.
                        deferred_rooms.add(room)
                        log.warning("Ordinary Matrix work pending in %s: %s", room, type(error).__name__)
                    self.waiting.pop(key, None)
                    continue
            if not waiting:
                continue
            room, event = waiting
            if snapshot["state"]["rooms"].get(room, {}).get("gaps"):
                continue
            del self.waiting[key]
            await self.handler(event)(self.client.rooms[room], event, input_key=key)

    async def poll(self):
        snapshot = await asyncio.to_thread(self.matrix.load)
        consumer = self.matrix.consumer(snapshot, account=self.client.user_id, device=self.client.device_id or "",
                                        format="matrix-nio/sync-v3")
        # nio falls back to either private field when 'since' is empty and
        # suppresses decoding a response whose token equals its in-memory token.
        # A new SDK still needs full room state when the server has no new
        # events and returns exactly the already durable token.
        self.client.next_batch = consumer["token"] if self.ready else ""
        self.client.loaded_sync_token = ""
        response = await self.client.sync(since=consumer["token"] or None, full_state=not self.ready,
                                          timeout=self.channel.sync_timeout_ms if self.ready else 0)
        if not isinstance(response, SyncResponse):
            if getattr(response, "status_code", None) in {"M_UNKNOWN_TOKEN", "M_MISSING_TOKEN"}:
                self.client.access_token = self.channel.access_token = ""
                return False
            raise RecoveryConflict("Matrix sync failed: " + type(response).__name__)
        await self.channel._e2ee_maintenance()
        denials = []
        if response.next_batch != consumer["token"]:
            timelines = []
            for room, joined in response.rooms.join.items():
                timeline = joined.timeline
                timelines.append({"room_id": room, "inputs": await self.inputs(room, timeline.events, denials=denials),
                                  "limited": timeline.limited, "prev_batch": timeline.prev_batch})
            async with self.lock:
                snapshot = await asyncio.to_thread(self.matrix.load)
                snapshot = await asyncio.to_thread(self.matrix.sync, snapshot, consumer, response.next_batch, timelines)
        if any(room.get("gaps") for room in snapshot["state"]["rooms"].values()):
            failures = await backfill_round(self.matrix, self.fetch_page, self.inputs)
            for room, error in failures.items():
                log.warning("Matrix history pending in %s: %s", room, type(error).__name__)
            snapshot = await asyncio.to_thread(self.matrix.load)
        for payload in denials:
            await self.channel._access_control_gate(payload)
        await self.dispatch(snapshot)
        for room in response.rooms.invite:
            await self.client.join(room)
        if not self.ready:
            self.channel._mark_channel_ready()
            self.ready = True
            if self.tasks_scanned:
                from worker_handoff import recovery_scanned
                recovery_scanned()
        return True

    async def run(self):
        registered = False
        while True:
            try:
                if not registered:
                    async with self.lock:
                        await asyncio.to_thread(self.matrix.start)
                    registered = True
                if not await self.poll():
                    return
            except Exception:
                log.exception("Worker Matrix sync failed; retrying from the shared cursor")
                await asyncio.sleep(5)
