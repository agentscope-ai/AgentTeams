import asyncio
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock

import pytest
from nio import RoomMessageNotice, RoomMessageText

from agentteams_matrix.channel import AgentTeamsMatrixChannel


@pytest.mark.parametrize("blacklisted", [False, True])
@pytest.mark.parametrize("is_group", [False, True])
@pytest.mark.parametrize("native_request", [False, True])
def test_denial_is_a_notice_that_does_not_trigger_another_agent(blacklisted, is_group, native_request):
    channel = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
    channel._user_id = "@worker:localhost"
    channel._language = "en"
    channel.access_control_dm = True
    channel.access_control_group = True
    store = MagicMock()
    store.is_whitelisted.return_value = False
    store.is_blacklisted.return_value = blacklisted
    channel._get_acl_store = lambda: store
    channel._client = SimpleNamespace(rooms={}, add_event_callback=MagicMock())
    channel._room_send_with_retry = AsyncMock(return_value=SimpleNamespace(event_id="$denial"))
    channel._send_typing = AsyncMock()
    payload = {
        "sender_id": "@leader:localhost",
        "content_parts": [],
        "meta": {"room_id": "!room:localhost", "is_group": is_group},
    }
    original_meta = dict(payload["meta"])
    if native_request:
        payload = SimpleNamespace(user_id=payload["sender_id"], channel_meta=payload["meta"], input=[])

    assert asyncio.run(channel._access_control_gate(payload)) is True
    content = channel._room_send_with_retry.call_args.args[2]
    assert content["msgtype"] == "m.notice"
    assert content["body"]
    assert (payload.channel_meta if native_request else payload["meta"]) == original_meta
    if not blacklisted:
        store.add_pending.assert_called_once()

    event = RoomMessageNotice.from_dict({
        "type": "m.room.message", "event_id": "$denial", "sender": channel._user_id,
        "origin_server_ts": 1, "content": content,
    })
    assert not isinstance(event, RoomMessageText)
    channel._register_plain_room_callbacks()
    for call in channel._client.add_event_callback.call_args_list:
        assert not isinstance(event, call.args[1])


def test_allowed_message_keeps_original_metadata():
    channel = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
    channel.access_control_dm = True
    channel.access_control_group = True
    store = MagicMock()
    store.is_whitelisted.return_value = True
    channel._get_acl_store = lambda: store
    payload = {"sender_id": "@human:localhost", "meta": {"room_id": "!room:localhost"}}
    assert asyncio.run(channel._access_control_gate(payload)) is False
    assert payload["meta"] == {"room_id": "!room:localhost"}
    store.add_pending.assert_not_called()
