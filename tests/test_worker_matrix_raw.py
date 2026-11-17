"""Raw Matrix device binding shared by Codex and DeepSeek adapters."""
import hashlib
import io
import sys
from pathlib import Path
from unittest.mock import Mock
from urllib.parse import parse_qs, urlsplit
from urllib.error import HTTPError

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import RecoveryConflict
from worker_matrix_raw import publish_raw_saved


def checkpoint(attempt, text="saved", txn="stable"):
    cp = {"delivery": "pending", "reply_txn": txn, "version": 1,
          "result": {"key": "report", "size": len(text.encode()), "sha256": hashlib.sha256(text.encode()).hexdigest()}}
    if attempt:
        cp["reply_attempt"] = attempt
    return cp


def test_persist_device_before_native_send_and_preserve_relation():
    client = Mock()
    client.request.side_effect = [{"user_id": "@a:hs", "device_id": "A"},
        HTTPError("https://hs", 404, "missing", {}, io.BytesIO(b'{"errcode":"M_NOT_FOUND"}'))]
    sent = []
    bind = Mock(side_effect=lambda cp: sent.append(cp["reply_attempt"]))
    content = {"org.agentteams.progress_only": True, "m.relates_to": {"m.in_reply_to": {"event_id": "$src"}}}
    def send(room, body, txn):
        assert sent == [{"state": "sending", "user_id": "@a:hs", "device_id": "A"}]
        assert (room, txn) == ("!room:hs", "stable")
        assert body == {**content, "msgtype": "m.text", "body": "saved", "org.agentteams.reply_txn": txn}
        return "$saved"
    client.send_content.side_effect = send
    assert publish_raw_saved(client, "!room:hs", "saved", checkpoint({"state": "ready"}), "@a:hs", bind, content=content) == "$saved"
    bind.assert_called_once()


@pytest.mark.parametrize("proof", ["marker", "legacy", "absent", "encrypted"])
def test_changed_device_only_confirms_verified_native_event(proof):
    room, user = "!room:hs", "@a:hs"
    scope = None if proof == "legacy" else {"state": "sending", "user_id": user, "device_id": "A"}
    cp = checkpoint(scope)
    base = {"sender": user, "type": "m.room.message", "event_id": "$original",
            "room_id": room, "content": {"msgtype": "m.text", "body": "saved", "org.agentteams.reply_txn": "stable"}}
    decoys = [{**base, "sender": "@other:hs"}, {**base, "room_id": "!other:hs"},
              {**base, "content": {**base["content"], "body": "other"}}, {**base, "type": "m.room.encrypted"}]
    events = {"marker": [base], "legacy": [{**base, "content": {"msgtype": "m.text", "body": "saved"},
               "unsigned": {"transaction_id": "dsh-transformed"}}], "absent": [], "encrypted": [{**base, "type": "m.room.encrypted"}]}[proof]
    client = Mock()
    def request(method, path, body=None):
        assert method == "GET" and body is None
        if path.endswith("/whoami"):
            return {"user_id": user, "device_id": "B"}
        query = parse_qs(urlsplit(path).query)
        assert query["dir"] == ["b"] and "%21room%3Ahs" in path
        return {"end": "next", "chunk": decoys} if "from" not in query else {"chunk": events}
    client.request.side_effect = request
    bind = Mock()
    if proof in {"absent", "encrypted"}:
        with pytest.raises(RecoveryConflict, match="verification"):
            publish_raw_saved(client, room, "saved", cp, user, bind)
    else:
        assert publish_raw_saved(client, room, "saved", cp, user, bind, legacy_transaction_id=lambda _: "dsh-transformed") == "$original"
    client.send_content.assert_not_called()
    bind.assert_not_called()


def test_encrypted_room_rejects_native_plaintext():
    client = Mock()
    client.request.side_effect = [{"user_id": "@a:hs", "device_id": "A"}, {"algorithm": "m.megolm.v1.aes-sha2"}]
    cp = checkpoint({"state": "sending", "user_id": "@a:hs", "device_id": "A"})
    with pytest.raises(RecoveryConflict, match="encrypted"):
        publish_raw_saved(client, "!room:hs", "saved", cp, "@a:hs", Mock())
    client.send_content.assert_not_called()
