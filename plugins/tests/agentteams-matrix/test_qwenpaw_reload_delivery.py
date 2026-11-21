"""Deliver Matrix follow-ups across overlapping SDK workspace generations."""

import asyncio
import importlib.util
import sys
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

@pytest.fixture(params=["legacy", "bundle"])
def matrix_channel(monkeypatch, request):
    pytest.importorskip("qwenpaw.app.channels.base")
    root = Path(__file__).resolve().parents[2]
    path = root / ("agentteams-matrix-channel/agentteams_matrix/channel.py"
                   if request.param == "legacy" else "agentteams-matrix/adapters/qwenpaw/agentteams_matrix/channel.py")
    # The bundle parity tests also import an older adapter with this package
    # name. Load the actual image-owned plugin independently of that cache.
    spec = importlib.util.spec_from_file_location("_reload_delivery_channel", path)
    module = importlib.util.module_from_spec(spec)
    monkeypatch.setitem(sys.modules, spec.name, module)
    spec.loader.exec_module(module)
    return module.AgentTeamsMatrixChannel, module.BaseChannel


def test_reload_waits_for_old_run_and_dispatches_followup_once(monkeypatch, matrix_channel):
    AgentTeamsMatrixChannel, BaseChannel = matrix_channel
    async def scenario():
        release = asyncio.Future()
        calls = []
        tracker = SimpleNamespace(snapshot_active_tasks=AsyncMock(return_value={"chat": release}))
        manager = SimpleNamespace()
        old = SimpleNamespace(agent_id="default", _manager=manager, task_tracker=tracker)
        new = SimpleNamespace(agent_id="default", _manager=manager, task_tracker=tracker)
        manager.get_loaded_agent = lambda _: new
        new.chat_manager = SimpleNamespace(get_or_create_chat=AsyncMock(return_value=SimpleNamespace(id="chat")))
        channels = []
        for workspace in (old, new):
            channel = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
            channel._workspace = workspace
            channels.append(channel)
        new.channel_manager = SimpleNamespace(get_channel=AsyncMock(return_value=channels[1]))

        async def sdk_consume(channel, request, payload):
            if not release.done():
                return  # The SDK currently ignores requests while a run exists.
            calls.append((channel, payload))

        monkeypatch.setattr(BaseChannel, "_consume_with_tracker", sdk_consume)
        request = SimpleNamespace(session_id="matrix:!room", user_id="!room", channel_meta={"event_id": "$followup"})
        first = asyncio.create_task(channels[0]._consume_with_tracker(request, "followup"))
        second = asyncio.create_task(channels[1]._consume_with_tracker(request, "followup"))
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        assert calls == []
        release.set_result(None)
        await asyncio.wait_for(asyncio.gather(first, second), 1)
        assert calls == [(channels[1], "followup")]

    asyncio.run(scenario())


def test_unrelated_rooms_remain_parallel(monkeypatch, matrix_channel):
    AgentTeamsMatrixChannel, BaseChannel = matrix_channel
    async def scenario():
        release = asyncio.Event()
        entered = {room: asyncio.Event() for room in ("a", "b")}
        tracker = SimpleNamespace(snapshot_active_tasks=AsyncMock(return_value={}))
        manager = SimpleNamespace()
        workspace = SimpleNamespace(agent_id="default", _manager=manager, task_tracker=tracker)
        manager.get_loaded_agent = lambda _: workspace
        workspace.chat_manager = SimpleNamespace(get_or_create_chat=AsyncMock(return_value=SimpleNamespace(id="chat")))
        channel = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
        channel._workspace = workspace
        workspace.channel_manager = SimpleNamespace(get_channel=AsyncMock(return_value=channel))

        async def sdk_consume(_channel, request, _payload):
            entered[request.user_id].set()
            await release.wait()

        monkeypatch.setattr(BaseChannel, "_consume_with_tracker", sdk_consume)
        tasks = [asyncio.create_task(channel._consume_with_tracker(
            SimpleNamespace(session_id="matrix:" + room, user_id=room, channel_meta={"event_id": "$" + room}), room,
        )) for room in entered]
        await asyncio.wait_for(asyncio.gather(*(event.wait() for event in entered.values())), 1)
        release.set()
        await asyncio.gather(*tasks)

    asyncio.run(scenario())


def test_old_queue_cancellation_does_not_drop_accepted_followup(monkeypatch, matrix_channel):
    AgentTeamsMatrixChannel, BaseChannel = matrix_channel
    async def scenario():
        release = asyncio.Future()
        delivered = asyncio.Event()
        tracker = SimpleNamespace(snapshot_active_tasks=AsyncMock(return_value={"chat": release}))
        manager = SimpleNamespace()
        workspace = SimpleNamespace(agent_id="default", _manager=manager, task_tracker=tracker)
        manager.get_loaded_agent = lambda _: workspace
        workspace.chat_manager = SimpleNamespace(get_or_create_chat=AsyncMock(return_value=SimpleNamespace(id="chat")))
        channel = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
        channel._workspace = workspace
        workspace.channel_manager = SimpleNamespace(get_channel=AsyncMock(return_value=channel))

        async def sdk_consume(_channel, _request, _payload):
            if release.done():
                delivered.set()

        monkeypatch.setattr(BaseChannel, "_consume_with_tracker", sdk_consume)
        request = SimpleNamespace(session_id="matrix:!room", user_id="!room", channel_meta={"event_id": "$followup"})
        consumer = asyncio.create_task(channel._consume_with_tracker(request, "followup"))
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        consumer.cancel()
        await asyncio.gather(consumer, return_exceptions=True)
        assert not release.cancelled()
        release.set_result(None)
        await asyncio.wait_for(delivered.wait(), 1)

    asyncio.run(scenario())


@pytest.mark.parametrize("count", [2, 3])
def test_sdk_batch_retains_each_source_without_forcing_separate_turns(monkeypatch, matrix_channel, count):
    AgentTeamsMatrixChannel, BaseChannel = matrix_channel
    from qwenpaw.app.channels.manager import ChannelManager, _process_batch

    async def scenario():
        channel = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
        inputs = [{"channel_id": channel.channel, "sender_id": "@human:test",
                   "content_parts": [{"type": "text", "text": "same task request"}],
                   "meta": {"room_id": "!room:test", "event_id": "$source-" + str(index),
                            "session_source_event_ids": ["$source-" + str(index)]}}
                  for index in range(count)]
        consumed = []

        async def sdk_consume(_channel, payload):
            consumed.append(payload)

        monkeypatch.setattr(BaseChannel, "_consume_one_request", sdk_consume)
        manager = ChannelManager.__new__(ChannelManager)
        queues = {}
        for payload in inputs:
            key = manager._extract_session_id(channel, payload)
            queues.setdefault(key, []).append(payload)
        for batch in queues.values():
            await _process_batch(channel, batch)
        assert len(consumed) == 1
        merged = consumed[0]
        assert merged["content_parts"] == [p for item in inputs for p in item["content_parts"]]
        assert merged["meta"]["matrix_input_sources"] == [
            {"event_id": "$source-" + str(index), "room_id": "!room:test",
             "sender_id": "@human:test", "content_start": index, "content_end": index + 1}
            for index in range(count)]
        assert channel.resolve_session_id("@human:test", inputs[0]["meta"]) == "matrix:!room:test"

    asyncio.run(scenario())


def test_batch_keeps_a_request_and_its_withdrawal_in_one_context(matrix_channel):
    AgentTeamsMatrixChannel, _ = matrix_channel
    channel = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
    items = [{"channel_id": channel.channel, "sender_id": "@human:test",
              "content_parts": [{"type": "text", "text": text}],
              "meta": {"room_id": "!room:test", "event_id": event}}
             for event, text in [("$request", "Build the project."),
                                 ("$withdrawal", "Cancel that task; do not start it.")]]
    merged = channel.merge_native_items(items)
    assert merged["content_parts"] == [p for item in items for p in item["content_parts"]]
    assert [s["event_id"] for s in merged["meta"]["matrix_input_sources"]] == ["$request", "$withdrawal"]


def test_nested_batches_keep_original_content_ranges(matrix_channel):
    AgentTeamsMatrixChannel, _ = matrix_channel
    channel = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
    items = [{"channel_id": channel.channel, "sender_id": "@human:test",
              "content_parts": [{"type": "text", "text": str(index)}],
              "meta": {"room_id": "!room:test", "event_id": "$" + str(index)}} for index in range(3)]
    merged = channel.merge_native_items([items[0], channel.merge_native_items(items[1:])])
    assert [(s["event_id"], s["content_start"], s["content_end"])
            for s in merged["meta"]["matrix_input_sources"]] == [("$0", 0, 1), ("$1", 1, 2), ("$2", 2, 3)]


def test_source_ranges_preserve_each_sender_and_reply_route(matrix_channel):
    AgentTeamsMatrixChannel, _ = matrix_channel
    channel = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
    routes = [{"target_user": "@a:test", "target_session": "!human-a:test"},
              {"target_user": "@b:test", "target_session": "!human-b:test"}]
    parts = [[{"type": "text", "text": "request"}, {"type": "text", "text": "attachment"}],
             [{"type": "text", "text": "follow-up"}]]
    items = [{"channel_id": channel.channel, "sender_id": sender, "content_parts": content,
              "meta": {"room_id": "!room:test", "event_id": "$" + str(index),
                       "teamharness_requester_route": routes[index]}}
             for index, (sender, content) in enumerate(zip(("@a:test", "@b:test"), parts))]
    merged = channel.merge_native_items(items)
    sources = merged["meta"]["matrix_input_sources"]
    assert [s["sender_id"] for s in sources] == ["@a:test", "@b:test"]
    assert [s["teamharness_requester_route"] for s in sources] == routes
    assert [(s["content_start"], s["content_end"]) for s in sources] == [(0, 2), (2, 3)]


def test_native_request_labels_sources_without_duplicating_their_content(matrix_channel):
    AgentTeamsMatrixChannel, _ = matrix_channel
    module = sys.modules[AgentTeamsMatrixChannel.__module__]
    channel = AgentTeamsMatrixChannel(process=lambda *_: None)
    items = [{"channel_id": channel.channel, "sender_id": "@human:test",
              "content_parts": [module.TextContent(type=module.ContentType.TEXT, text=text)],
              "meta": {"room_id": "!room:test", "event_id": "$" + str(index)}}
             for index, text in enumerate(("Original unique request", "Withdraw the previous request"))]
    request = channel.build_agent_request_from_native(channel.merge_native_items(items))
    text = str(request.model_dump())
    assert "[Message $0]" in text and "[Message $1]" in text
    assert text.count("Original unique request") == 1
    assert text.count("Withdraw the previous request") == 1
    assert request.session_id == "matrix:!room:test"


def test_single_and_batched_native_inputs_keep_the_same_message_identity(matrix_channel):
    AgentTeamsMatrixChannel, _ = matrix_channel
    module = sys.modules[AgentTeamsMatrixChannel.__module__]
    channel = AgentTeamsMatrixChannel(process=lambda *_: None)
    items = [{"channel_id": channel.channel, "sender_id": "@human:test",
              "content_parts": [module.TextContent(type=module.ContentType.TEXT, text="Identical request")],
              "meta": {"room_id": "!room:test", "event_id": event}}
             for event in ("$first", "$second")]
    for item in items:
        text = str(channel.build_agent_request_from_native(item).model_dump())
        assert "[Message " + item["meta"]["event_id"] + "]" in text
        assert text.count("Identical request") == 1
    merged = str(channel.build_agent_request_from_native(channel.merge_native_items(items)).model_dump())
    assert "[Message $first]" in merged and "[Message $second]" in merged
    assert merged.count("Identical request") == 2
