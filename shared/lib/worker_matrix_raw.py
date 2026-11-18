"""Publish saved replies through the synchronous, unencrypted MatrixClient."""
import asyncio
import json
from urllib.error import HTTPError
from urllib.parse import quote, urlencode

from task_recovery import RecoveryConflict, RecoveryUnavailable
from worker_matrix_delivery import publish_saved


def publish_raw_saved(client, room_id, text, checkpoint, expected_sender, bind, *, content=None, legacy_transaction_id=None):
    """bind(next_checkpoint) must confirm the device write on the active execution."""
    room = "/_matrix/client/v3/rooms/" + quote(room_id, safe="")

    async def whoami():
        response = client.request("GET", "/_matrix/client/v3/account/whoami", None)
        if response.get("user_id") != expected_sender:
            raise RecoveryConflict("Matrix credential belongs to another user")
        return response["user_id"], response.get("device_id")

    async def persist(cp):
        bind(cp)

    async def find(txn, body, sender):
        token = None
        seen = set()
        legacy = {txn}
        if legacy_transaction_id:
            legacy.add(legacy_transaction_id(txn))
        while token not in seen:
            seen.add(token)
            query = {"dir": "b", "limit": 100, "filter": json.dumps({"senders": [sender]})}
            if token:
                query["from"] = token
            page = client.request("GET", room + "/messages?" + urlencode(query), None)
            if not isinstance(page.get("chunk"), list):
                raise RecoveryUnavailable("Matrix reply history is unreadable")
            for event in page["chunk"]:
                if (event.get("room_id", room_id) != room_id or event.get("sender") != sender
                        or event.get("type") != "m.room.message"):
                    continue
                data = event.get("content") or {}
                if data.get("msgtype") != "m.text" or data.get("body") != body:
                    continue
                marker = data.get("org.agentteams.reply_txn")
                if marker == txn or (not checkpoint.get("reply_attempt") and marker is None
                        and (event.get("unsigned") or {}).get("transaction_id") in legacy):
                    return event.get("event_id")
            token = page.get("end")
            if not token:
                break
        return None

    async def send(txn):
        # This native transport has no Olm session. Even if the installation
        # projects E2EE=false, a particular joined room can require encryption.
        try:
            client.request("GET", room + "/state/m.room.encryption/", None)
        except HTTPError as error:
            if error.code != 404 or json.load(error).get("errcode") != "M_NOT_FOUND":
                raise
        else:
            raise RecoveryConflict("Raw Matrix delivery cannot send to an encrypted room")
        data = {**(content or {}), "msgtype": "m.text", "body": text, "org.agentteams.reply_txn": txn}
        return client.send_content(room_id, data, txn)

    return asyncio.run(publish_saved(checkpoint, text, whoami, persist, find, send))


class RawWorkerSync:
    """Commit unencrypted native sync and history pages before dispatching shared work."""

    def __init__(self, matrix, client, sender, classify, observe_rooms):
        self.matrix = matrix
        self.client = client
        self.sender = sender
        self.classify = classify
        self.observe_rooms = observe_rooms
        self.ready = False

    def inputs(self, room, events, until=None):
        from worker_matrix_state import matrix_event_key

        inputs = []
        for event in events:
            if event.get("room_id", room) != room:
                raise RecoveryConflict("Matrix returned an event for a different room")
            if event.get("type") == "m.room.encrypted":
                raise RecoveryConflict("Raw Matrix consumer cannot decode encrypted input")
            if event.get("type") != "m.room.message" or not event.get("event_id"):
                continue
            event_id = event["event_id"]
            previous = self.matrix.input(matrix_event_key(room, event_id))
            decision = (previous["intent"], None) if previous else self.classify(room, event)
            if decision:
                intent, task_id = decision
                source = {k: event[k] for k in ("sender", "type", "content")}
                source["origin_server_ts"] = event.get("origin_server_ts", 0)
                inputs.append(self.matrix.prepare_input(room, event_id, source, intent, task_id=task_id))
            if event_id == until:
                break
        return inputs

    def poll(self, timeout_ms=1000):
        from worker_matrix_history import backfill_round

        snapshot = self.matrix.load()
        account = self.client.request("GET", "/_matrix/client/v3/account/whoami")
        if account.get("user_id") != self.sender or not account.get("device_id"):
            raise RecoveryConflict("Raw Matrix consumer requires the Worker's account and device")
        consumer = self.matrix.consumer(snapshot, account=self.sender, device=account["device_id"], format="matrix-raw/sync-v3")
        query = {"timeout": timeout_ms if self.ready else 0}
        if consumer["token"]:
            query["since"] = consumer["token"]
        if not self.ready:
            query["full_state"] = "true"
        payload = self.client.request("GET", "/_matrix/client/v3/sync?" + urlencode(query),
                                      timeout=max(10, timeout_ms // 1000 + 10))
        rooms = self.observe_rooms(payload)
        timelines = []
        for room, joined in payload.get("rooms", {}).get("join", {}).items():
            if room in rooms:
                timeline = joined.get("timeline", {})
                timelines.append({"room_id": room, "inputs": self.inputs(room, timeline.get("events", [])),
                    "limited": timeline.get("limited", False), "prev_batch": timeline.get("prev_batch")})
        snapshot = self.matrix.sync(snapshot, consumer, payload["next_batch"], timelines)

        async def fetch(room, token):
            if room not in rooms:
                raise RecoveryConflict("Cannot recover history outside current room membership")
            query = urlencode({"dir": "b", "from": token, "limit": 100})
            page = self.client.request("GET", "/_matrix/client/v3/rooms/" + quote(room, safe="") + "/messages?" + query)
            if page.get("start") != token or not isinstance(page.get("chunk"), list):
                raise RecoveryConflict("Matrix history response has the wrong boundary")
            return page["chunk"], page.get("end")

        async def prepare(room, events, until):
            return self.inputs(room, events, until)

        if any(room.get("gaps") for room in snapshot["state"]["rooms"].values()):
            failures = asyncio.run(backfill_round(self.matrix, fetch, prepare))
            snapshot = self.matrix.load()
        else:
            failures = {}
        self.ready = True
        return snapshot, failures
