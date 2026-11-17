"""QwenPaw's matrix-nio binding for the standard Worker Matrix consumer."""
import asyncio
import logging

from nio import (MegolmEvent, RoomEncryptedAudio, RoomEncryptedFile, RoomEncryptedImage,
                 RoomEncryptedVideo, RoomMessageAudio, RoomMessageFile, RoomMessageImage,
                 RoomMessageText, RoomMessageVideo, RoomMessagesResponse, SyncResponse)

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
        self.ready = False
        # Typed events await their batch/page commit and any prior room gap.
        self.waiting = {}

    def handler(self, event):
        if isinstance(event, RoomMessageText):
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

    async def inputs(self, room, events, until=None):
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
                item = await handler(self.client.rooms[room], event, prepare=self.prepare)
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

    async def dispatch(self, snapshot):
        for key in snapshot["state"]["pending"]:
            if key not in self.waiting:
                continue
            room, event = self.waiting[key]
            if snapshot["state"]["rooms"].get(room, {}).get("gaps"):
                continue
            # Once native admission begins, an exception has an uncertain
            # result. Leave the authoritative input for execution recovery.
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
        async with self.lock:
            timelines = []
            for room, joined in response.rooms.join.items():
                timeline = joined.timeline
                timelines.append({"room_id": room, "inputs": await self.inputs(room, timeline.events),
                                  "limited": timeline.limited, "prev_batch": timeline.prev_batch})
            snapshot = await asyncio.to_thread(self.matrix.load)
            snapshot = await asyncio.to_thread(self.matrix.sync, snapshot, consumer, response.next_batch, timelines)
            if any(room.get("gaps") for room in snapshot["state"]["rooms"].values()):
                failures = await backfill_round(self.matrix, self.fetch_page, self.inputs)
                for room, error in failures.items():
                    log.warning("Matrix history pending in %s: %s", room, type(error).__name__)
                snapshot = await asyncio.to_thread(self.matrix.load)
        await self.dispatch(snapshot)
        for room in response.rooms.invite:
            await self.client.join(room)
        if not self.ready:
            self.channel._mark_channel_ready()
            self.ready = True
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
