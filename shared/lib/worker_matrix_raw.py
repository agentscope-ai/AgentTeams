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
