"""Deliver Matrix follow-ups across overlapping SDK workspace generations."""

import asyncio
import sys
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

pytest.importorskip("qwenpaw")
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "agentteams-matrix-channel"))
from agentteams_matrix.channel import AgentTeamsMatrixChannel, BaseChannel


def test_reload_waits_for_old_run_and_dispatches_followup_once(monkeypatch):
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


def test_unrelated_rooms_remain_parallel(monkeypatch):
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


def test_old_queue_cancellation_does_not_drop_accepted_followup(monkeypatch):
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
