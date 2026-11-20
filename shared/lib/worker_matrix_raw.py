"""Publish saved replies through the synchronous, unencrypted MatrixClient."""
import asyncio
import hashlib
import json
from urllib.error import HTTPError
from urllib.parse import quote, urlencode

from task_recovery import RecoveryConflict, RecoveryUnavailable
from worker_matrix_delivery import publish_saved


def require_unencrypted_room(client, room_id):
    """Raw clients may act only when the homeserver confirms encryption state is absent."""
    path = "/_matrix/client/v3/rooms/" + quote(room_id, safe="") + "/state/m.room.encryption/"
    try:
        client.request("GET", path, None)
    except HTTPError as error:
        if error.code == 404 and json.load(error).get("errcode") == "M_NOT_FOUND":
            return
        raise
    raise RecoveryConflict("Raw Matrix transport cannot handle an encrypted room")


def raw_room_history(client, matrix, account, room, source, admit):
    """Read earlier authorized room messages for a new native session without dispatching them."""
    from worker_matrix_state import matrix_event_key

    path = "/_matrix/client/v3/rooms/" + quote(room, safe="") + "/context/" + quote(source, safe="") + "?limit=20"
    page = client.request("GET", path)
    anchor = page.get("event") or {}
    if anchor.get("event_id") != source or anchor.get("room_id", room) != room or not isinstance(page.get("events_before"), list):
        raise RecoveryConflict("Cannot establish Matrix room context at the source event")
    history = []
    for event in reversed(page["events_before"]):
        content = event.get("content") or {}
        event_id, sender, text = event.get("event_id"), event.get("sender"), content.get("body")
        if (not event_id or not sender or event.get("room_id", room) != room or event.get("type") != "m.room.message"
                or (event.get("unsigned") or {}).get("redacted_because") or content.get("msgtype") != "m.text"
                or not isinstance(text, str)):
            continue
        parent = (content.get("m.relates_to") or {}).get("m.in_reply_to", {}).get("event_id") if sender == account else event_id
        prior = matrix.input(matrix_event_key(room, parent)) if parent else None
        if not prior or prior.get("intent") != "message" or prior.get("room_id") != room or prior.get("event_id") != parent:
            continue
        if sender == account:
            cp = (prior.get("execution") or {}).get("checkpoint") or {}
            if (cp.get("delivery") != "delivered" or cp.get("reply_event") != event_id
                    or (cp.get("result") or {}).get("sha256") != hashlib.sha256(text.encode()).hexdigest()):
                continue
        elif prior["input"]["sender"] != sender or prior["input"]["content"] != content:
            continue
        if admit(room, {**prior["input"], "event_id": parent, "room_id": room}):
            history.append({"sender": sender, "event_id": event_id, "text": text})
    return history


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
        require_unencrypted_room(client, room_id)
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
        self.checked_rooms = set()

    def require_room(self, room):
        """Share a room policy lookup for this sync, its history pages and pending inputs."""
        if room not in self.checked_rooms:
            require_unencrypted_room(self.client, room)
            self.checked_rooms.add(room)

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
            self.require_room(room)
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

        self.checked_rooms.clear()
        snapshot = self.matrix.load()
        account = self.client.request("GET", "/_matrix/client/v3/account/whoami")
        if account.get("user_id") != self.sender or not account.get("device_id"):
            raise RecoveryConflict("Raw Matrix consumer requires the Worker's account and device")
        consumer = self.matrix.consumer(snapshot, account=self.sender, device=account["device_id"], format="matrix-raw/sync-v3")
        if not self.ready and consumer["token"]:
            # Invites ignored by an older consumer are absent from incremental
            # sync. Discover them without replacing the durable device cursor
            # or dispatching the discovery response's historical timeline.
            discovery = self.client.request("GET", "/_matrix/client/v3/sync?" + urlencode({
                "timeout": 0, "filter": json.dumps({"room": {"timeline": {"limit": 0}}})}))
            self.observe_rooms({"rooms": {"invite": discovery.get("rooms", {}).get("invite", {})}})
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
