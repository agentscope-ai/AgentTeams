"""Real channel callbacks must persist accepted input before native enqueue."""
import asyncio
import importlib.util
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest

pytest.importorskip("nio")
pytest.importorskip("qwenpaw")

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "shared/lib"))
from task_recovery import RecoveryUnavailable


@pytest.fixture
def channel_module():
    path = ROOT / "qwenpaw/adapters/matrix/agentteams_matrix/channel.py"
    spec = importlib.util.spec_from_file_location("shared_input_channel", path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def channel(module):
    ch = module.AgentTeamsMatrixChannel(process=lambda *_: None)
    ch._user_id = "@worker:test"
    ch._is_dm_room = AsyncMock(return_value=True)
    ch._announce_session_receipts = AsyncMock()
    ch._send_read_receipt = AsyncMock()
    ch._send_typing = AsyncMock()
    ch._enqueue = Mock()
    ch._task_execution = SimpleNamespace(accept_input=AsyncMock())
    ch._persist_sync_state = Mock(return_value=True)
    return ch


def source():
    return SimpleNamespace(sender="@human:test", event_id="$source", body="Continue the work",
        server_timestamp=12, source={"type": "m.room.message", "content": {"msgtype": "m.text", "body": "Continue the work"}})


def test_storage_failure_neither_enqueues_nor_accepts_the_callback(channel_module):
    async def run():
        ch = channel(channel_module)
        ch._task_execution.accept_input.side_effect = RecoveryUnavailable("storage unavailable")
        room = SimpleNamespace(room_id="!room:test")
        if hasattr(ch, "_run_event_callback"):
            future = asyncio.get_running_loop().create_future()
            await ch._run_event_callback(ch._on_room_event, room, source(), future)
            assert future.result() is False
            ch._save_sync_token = Mock()
            ch._queue_sync_checkpoint("next-token", [future])
            ch._save_sync_token.assert_not_called()
        else:
            # The compatibility adapter's SyncEngine awaits handlers directly.
            with pytest.raises(RecoveryUnavailable):
                await ch._on_room_event(room, source())
        ch._enqueue.assert_not_called()
        ch._send_read_receipt.assert_not_awaited()
        ch._task_execution.accept_input.assert_awaited_once()
    asyncio.run(run())


def test_durable_input_precedes_enqueue_and_callback_acceptance(channel_module):
    async def run():
        ch = channel(channel_module)
        order = []
        ch._task_execution.accept_input.side_effect = lambda *a, **k: order.append("persist")
        ch._enqueue.side_effect = lambda p: order.append("enqueue")
        room = SimpleNamespace(room_id="!room:test")
        if hasattr(ch, "_run_event_callback"):
            future = asyncio.get_running_loop().create_future()
            await ch._run_event_callback(ch._on_room_event, room, source(), future)
            assert future.result() is True
        else:
            await ch._on_room_event(room, source())
        assert order == ["persist", "enqueue"]
    asyncio.run(run())


def test_verified_task_input_is_saved_before_task_recovery(channel_module, monkeypatch):
    import task_coordination
    async def run():
        ch = channel(channel_module)
        order = []
        ch._task_execution.accept_input.side_effect = lambda *a, **k: order.append("persist")
        ch._task_execution.recover = AsyncMock(side_effect=lambda *a, **k: order.append("recover"))
        event = source()
        event.source["content"]["org.agentteams.coordination"] = {}
        monkeypatch.setattr(task_coordination, "notification", lambda *a, event_id: {
            "text": "Canonical assignment", "meta": {"teamharness_coordination": {"kind": "task_assigned", "task_id": "T"}}})
        await ch._on_room_event(SimpleNamespace(room_id="!room:test"), event)
        assert order == ["persist", "recover"]
        assert ch._task_execution.accept_input.await_args.kwargs == {"task_id": "T"}
        ch._enqueue.assert_not_called()
    asyncio.run(run())


def test_coordination_handoff_does_not_create_ordinary_message_execution(channel_module, monkeypatch):
    import task_coordination
    async def run():
        ch = channel(channel_module)
        event = source()
        meta = {"sender_id": event.sender, "agent_matrix_user_id": ch._user_id,
            "room_id": "!room:test", "teamharness_coordination": {"kind": "handoff", "id": "a" * 32}}
        event.source["content"]["org.agentteams.coordination"] = {
            "channel_meta": meta, "session_id": "coordination-handoff:" + "a" * 32,
            "input": [{"content": [{"text": "Canonical handoff"}]}]}
        monkeypatch.setattr(task_coordination, "notification", lambda *a, event_id: {"meta": meta})
        await ch._on_room_event(SimpleNamespace(room_id="!room:test"), event)
        assert ch._task_execution.accept_input.await_args.args[2] == "control"
        ch._enqueue.assert_called_once()
    asyncio.run(run())


def test_unmentioned_context_is_durable_before_history_cache(channel_module):
    async def run():
        ch = channel(channel_module)
        ch._is_dm_room.return_value = False
        ch._was_mentioned = Mock(return_value=False)
        ch._get_display_name = Mock(return_value="Human")
        order = []
        ch._task_execution.accept_input.side_effect = lambda *a, **k: order.append("persist")
        ch._record_history = Mock(side_effect=lambda *a: order.append("history"))
        await ch._on_room_event(SimpleNamespace(room_id="!room:test"), source())
        assert order == ["persist", "history"]
        assert ch._task_execution.accept_input.await_args.args[2] == "context"
        ch._enqueue.assert_not_called()
    asyncio.run(run())
