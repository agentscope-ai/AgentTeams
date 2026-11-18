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
from worker_matrix_raw import publish_raw_saved, require_unencrypted_room


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


def test_raw_worker_consumes_only_its_device_cursor_and_preserves_initial_timeline():
    from worker_matrix_raw import RawWorkerSync
    from worker_matrix_state import WorkerMatrix

    raw = {"event_id": "$assignment", "sender": "@manager:hs", "type": "m.room.message",
           "origin_server_ts": 123, "content": {"body": "assignment"}}
    room = "!task:hs"
    state = {"state": {"rooms": {}}, "instance_id": "container"}
    matrix = Mock(spec=WorkerMatrix)
    matrix.load.return_value = matrix.sync.return_value = state
    matrix.consumer.return_value = {"account": "@w:hs", "device": "current", "runtime": "codex",
                                    "format": "matrix-raw/sync-v3", "token": "my_cursor"}
    matrix.input.return_value = None
    matrix.prepare_input.return_value = {"room_id": room, "event_id": "$assignment"}
    client = Mock()
    def respond(method, path, body=None, **kwargs):
        if path.endswith("/state/m.room.encryption/"):
            raise HTTPError(path, 404, "missing", {}, io.BytesIO(b'{"errcode":"M_NOT_FOUND"}'))
        if path.endswith("/whoami"):
            return {"user_id": "@w:hs", "device_id": "current"}
        assert parse_qs(urlsplit(path).query) == {"timeout": ["0"], "full_state": ["true"], "since": ["my_cursor"]}
        return {"next_batch": "new", "rooms": {"join": {room: {"timeline": {"events": [raw], "limited": True, "prev_batch": "back"}}}}}
    client.request.side_effect = respond
    classify = Mock(return_value=("task_notification", "T"))
    consumer = RawWorkerSync(matrix, client, "@w:hs", classify, lambda _: {room})
    assert consumer.poll() == (state, {})
    matrix.consumer.assert_called_once_with(state, account="@w:hs", device="current", format="matrix-raw/sync-v3")
    classify.assert_called_once_with(room, raw)
    expected = {k: raw[k] for k in ("sender", "type", "origin_server_ts", "content")}
    matrix.prepare_input.assert_called_once_with(room, "$assignment", expected, "task_notification", task_id="T")
    matrix.sync.assert_called_once_with(state, matrix.consumer.return_value, "new",
        [{"room_id": room, "inputs": [matrix.prepare_input.return_value], "limited": True, "prev_batch": "back"}])


def test_raw_worker_retains_source_classification_and_stops_on_undecryptable_input():
    from worker_matrix_raw import RawWorkerSync

    matrix, client, classify = Mock(), Mock(), Mock()
    matrix.input.return_value = {"intent": "context"}
    client.request.side_effect = HTTPError("https://hs", 404, "missing", {}, io.BytesIO(b'{"errcode":"M_NOT_FOUND"}'))
    consumer = RawWorkerSync(matrix, client, "@w:hs", classify, Mock())
    raw = {"event_id": "$old", "type": "m.room.message", "sender": "@user:hs", "content": {"body": "text"}}
    consumer.inputs("!room:hs", [raw], "$old")
    classify.assert_not_called()
    assert matrix.prepare_input.call_args.args[-1] == "context"
    with pytest.raises(RecoveryConflict, match="decode"):
        consumer.inputs("!room:hs", [{**raw, "type": "m.room.encrypted"}])
    with pytest.raises(RecoveryConflict, match="different room"):
        consumer.inputs("!room:hs", [{**raw, "room_id": "!other:hs"}])
    matrix.sync.assert_not_called()


def test_native_room_check_is_shared_by_history_rows_but_media_checks_current_state_again():
    from worker_matrix_raw import RawWorkerSync

    client, matrix = Mock(), Mock()
    raw = {"event_id": "$one", "type": "m.room.message", "sender": "@u", "content": {"body": "A"}}
    room = "!room:hs"
    client.request.side_effect = [HTTPError("https://hs", 404, "missing", {}, io.BytesIO(b'{"errcode":"M_NOT_FOUND"}')),
                                  {"algorithm": "m.megolm.v1.aes-sha2"}]
    matrix.input.return_value = None
    sync = RawWorkerSync(matrix, client, "@w:hs", lambda *_: ("message", None), Mock())
    sync.inputs(room, [raw, {**raw, "event_id":"$two"}])
    sync.inputs(room, [raw])  # A history page or pending input in the same round.
    sync.require_room(room)
    client.request.assert_called_once()
    with pytest.raises(RecoveryConflict, match="encrypted"):
        require_unencrypted_room(client, room)
    assert client.request.call_count == 2


@pytest.mark.parametrize(("status", "code"), [(403, "M_FORBIDDEN"), (404, "M_FORBIDDEN"), (500, "M_UNKNOWN")])
def test_unreadable_encryption_policy_never_allows_raw_media(status, code):
    client = Mock()
    client.request.side_effect = HTTPError("https://hs", status, "unreadable", {},
                                         io.BytesIO(('{"errcode":"' + code + '"}').encode()))
    with pytest.raises(HTTPError):
        require_unencrypted_room(client, "!room:hs")
