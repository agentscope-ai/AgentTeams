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
