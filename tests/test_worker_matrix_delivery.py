import asyncio
import hashlib
from pathlib import Path
import sys
from unittest.mock import AsyncMock

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import RecoveryConflict
from worker_matrix_delivery import publish_saved


def test_ack_loss_same_device_retries_and_other_device_can_only_reconcile():
    cp = {"version":1, "delivery":"pending", "reply_txn":"txn", "reply_attempt":{"state":"ready"},
          "result":{"size":2, "sha256":hashlib.sha256(b"ok").hexdigest()}}
    order = []
    who = AsyncMock(return_value=("@worker:example","A"))
    async def bind(new_cp):
        order.append("bind")
        cp.update(new_cp)
    async def lost_ack(txn):
        order.append("send")
        raise TimeoutError("accepted; ACK lost")
    find = AsyncMock(return_value="$actually-delivered")
    with pytest.raises(TimeoutError):
        asyncio.run(publish_saved(cp, "ok", who, bind, find, lost_ack))
    assert order == ["bind","send"] and cp["reply_attempt"]["device_id"] == "A"
    send = AsyncMock(return_value="$actually-delivered")
    assert asyncio.run(publish_saved(cp, "ok", who, bind, find, send)) == "$actually-delivered"
    find.assert_not_awaited()
    who.return_value = ("@worker:example","B")
    assert asyncio.run(publish_saved(cp, "ok", who, bind, find, send)) == "$actually-delivered"
    find.assert_awaited_once_with("txn","ok","@worker:example")
    send.assert_awaited_once_with("txn")
    for attempt in (cp["reply_attempt"], None):
        cp["reply_attempt"] = attempt
        find.return_value = None
        with pytest.raises(RecoveryConflict):
            asyncio.run(publish_saved(cp, "ok", who, bind, find, send))
    send.assert_awaited_once()
    with pytest.raises(RecoveryConflict):
        asyncio.run(publish_saved(cp,"changed text",who,bind,find,send))


def test_qwen_first_send_after_controller_timestamps_device_binding():
    from types import SimpleNamespace
    from qwenpaw_message_execution import publish_nio_reply

    identity = {"instance_id": "container", "generation": 1, "process_id": "boot"}
    cp = {"version": 3, "saved_at": "old", "result": {"size": 2, "sha256": hashlib.sha256(b"ok").hexdigest()},
          "delivery": "pending", "reply_txn": "txn", "reply_attempt": {"state": "ready"}}
    channel = SimpleNamespace(matrix_user_id="@q:test", _client=SimpleNamespace(
        whoami=AsyncMock(return_value=SimpleNamespace(user_id="@q:test", device_id="A"))))
    def save(next_cp):
        return {"execution": {**identity, "runtime": "qwenpaw", "state": "active",
                              "checkpoint": {**next_cp, "saved_at": "server-time"}}}
    send = AsyncMock(return_value="$actual")
    assert asyncio.run(publish_nio_reply(channel, "!r:test", cp, "ok", identity, save, send)) == "$actual"
    send.assert_awaited_once_with("txn")


@pytest.mark.parametrize("field,value", [
    ("runtime", "hermes"), ("state", "released"), ("instance_id", "other"), ("generation", 2),
    ("process_id", "old"), ("version", 8), ("reply_txn", "different"), ("delivery", "delivered"),
    ("reply_attempt", {"state": "ready"}), ("result", {"sha256": "other"}),
])
def test_device_confirmation_fences_actual_execution_and_reply(field, value):
    from worker_matrix_delivery import confirm_bound
    identity = {"instance_id": "container", "process_id": "boot", "generation": 1}
    cp = {"version": 4, "delivery": "pending", "reply_txn": "txn", "reply_attempt": {"state": "sending"},
          "result": {"sha256": "saved"}}
    execution = {**identity, "state": "active", "runtime": "qwenpaw", "checkpoint": {**cp, "saved_at": "server-time"}}
    if field in cp:
        execution["checkpoint"][field] = value
    else:
        execution[field] = value
    with pytest.raises(RecoveryConflict):
        confirm_bound(execution, identity, "qwenpaw", cp)
