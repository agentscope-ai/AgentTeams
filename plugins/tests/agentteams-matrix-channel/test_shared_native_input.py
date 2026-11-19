"""Read actual SafeJSONSession bytes before common persistence facts/model use."""
import asyncio
import importlib.util
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest

pytest.importorskip("qwenpaw")
from qwenpaw.app.chats.session import SafeJSONSession

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "shared/lib"))
from task_recovery import Recovery, RecoveryConflict, RecoveryUnavailable
from worker_matrix_state import matrix_event_key


@pytest.fixture(params=["agentteams-matrix-channel", "agentteams-matrix/adapters/qwenpaw"])
def modules(request):
    loaded = []
    for name in ["task_execution", "session_receipts"]:
        path = ROOT / "plugins" / request.param / "agentteams_matrix" / (name + ".py")
        spec = importlib.util.spec_from_file_location("shared_native_" + name + request.param.replace("/", "_"), path)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        loaded.append(module)
    return loaded


def test_native_fact_and_receipt_follow_real_sdk_save_before_reasoning(modules, tmp_path):
    async def run():
        task, receipts = modules
        order = []
        store = SafeJSONSession(str(tmp_path))
        room, event, session = "!room:test", "$input", "matrix:real-native"
        key = matrix_event_key(room, event)
        input_ = {"room_id": room, "event_id": event}
        def request(method, path, body=None, etag=None, **_):
            if path.endswith("/native"):
                if method == "GET":
                    return {"input": input_, "runtime": "qwenpaw", "instance_id": "actual-instance", "etag": "input-etag", "current": False}
                assert etag == "input-etag" and body["process_id"] == "actual-process"
                order.append(("fact", body["native_digest"]))
                return {}
            assert path == "runtime/matrix/events/" + key
            return input_
        recovery = SimpleNamespace(process="actual-process", request=request)
        workspace = SimpleNamespace(task_tracker=SimpleNamespace(), session=store)
        delivery = SimpleNamespace(submit=AsyncMock(side_effect=lambda *a: order.append(("receipt", None))))
        channel = SimpleNamespace(channel="agentteams_matrix", _workspace=workspace, _session_receipts=delivery)
        channel._task_execution = task.TaskExecution(channel, recovery)
        workspace.channel_manager = SimpleNamespace(get_channel=AsyncMock(return_value=channel))
        state = {"state": {"context": [{"role": "user", "content": "actual input"}], "middle_context": {}}}
        agent = SimpleNamespace(state=SimpleNamespace(middle_context=state["state"]["middle_context"]), state_dict=lambda: state)
        ctx = SimpleNamespace(session_id=session, mode_state={}, workspace=workspace,
            request=SimpleNamespace(channel="agentteams_matrix", user_id=room,
                channel_meta={"room_id": room, "session_source_event_ids": [event], "shared_input_keys": [key]}))
        async def reply(**kwargs):
            yield SimpleNamespace(type="REPLY_START")
            order.append(("model", None))
        async for _ in receipts.SessionReceiptMiddleware(ctx).on_reply(agent, {}, reply):
            pass
        persisted = await store.get_session_state_dict(session_id=session, user_id=room, channel="agentteams_matrix", allow_not_exist=False)
        assert persisted["agent"]["state"]["context"][0]["content"] == "actual input"
        assert order == [("fact", Recovery.digest(persisted)), ("receipt", None), ("model", None)]
    asyncio.run(run())


def test_managed_fact_failure_prevents_receipt_and_reasoning(modules):
    async def run():
        _, receipts = modules
        pending = {}
        agent = SimpleNamespace(state=SimpleNamespace(middle_context=pending),
            state_dict=lambda: {"state": {"middle_context": pending}})
        delivery = SimpleNamespace(submit=AsyncMock())
        execution = SimpleNamespace(commit_inputs=AsyncMock(side_effect=RecoveryUnavailable("CAS/storage unavailable")))
        channel = SimpleNamespace(_task_execution=execution, _session_receipts=delivery)
        ctx = SimpleNamespace(session_id="native", mode_state={},
            request=SimpleNamespace(channel="agentteams_matrix", user_id="!room:test", channel_meta={
                "room_id": "!room:test", "session_source_event_ids": ["$input"], "shared_input_keys": ["required"]}),
            workspace=SimpleNamespace(session=SimpleNamespace(save_session_state=AsyncMock()),
                channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=channel))))
        model = Mock()
        async def reply(**kwargs):
            yield SimpleNamespace(type="REPLY_START")
            model()
        with pytest.raises(RecoveryUnavailable):
            async for _ in receipts.SessionReceiptMiddleware(ctx).on_reply(agent, {}, reply):
                pass
        ctx.workspace.session.save_session_state.assert_awaited_once()
        delivery.submit.assert_not_awaited()
        model.assert_not_called()
        assert not pending[receipts.PENDING_KEY]
    asyncio.run(run())


def test_task_callback_uses_authoritative_team_and_never_fabricates_cursor(modules):
    async def run():
        task, _ = modules
        authority = SimpleNamespace(process="actual-process", discover=lambda: {"tasks": [
            {"team_id": "stable-team", "task": {"task_id": "T", "room_id": "!room:test"}}]})
        channel = SimpleNamespace(_workspace=SimpleNamespace(task_tracker=SimpleNamespace()))
        execution = task.TaskExecution(channel, authority)
        execution.matrix.start = Mock(return_value={"etag": "index"})
        execution.matrix.input = Mock(return_value=None)
        execution.matrix.ingest = Mock()
        event = SimpleNamespace(event_id="$input", sender="@leader:test", server_timestamp=12,
            source={"type": "m.room.message", "content": {"body": "wire input"}})
        await execution.accept_input("!room:test", event, "task_notification", task_id="T")
        args = execution.matrix.ingest.call_args.args
        assert args[:2] == ({"etag": "index"}, None)
        assert args[2][0]["work"] == {"kind": "task", "team_id": "stable-team", "task_id": "T"}
        assert args[2][0]["input"]["content"] == event.source["content"]
        with pytest.raises(RecoveryConflict):
            await execution.accept_input("!other:test", event, "task_notification", task_id="T")
        assert execution.matrix.ingest.call_count == 1
        authority.discover = lambda: {"tasks": [
            {"team_id": team, "task": {"task_id": "T", "room_id": "!room:test"}}
            for team in ["team-one", "team-two"]]}
        with pytest.raises(RecoveryConflict):
            await execution.accept_input("!room:test", event, "task_notification", task_id="T")
        assert execution.matrix.ingest.call_count == 1
    asyncio.run(run())


def test_redelivery_keeps_original_work_and_input_read_failure_is_not_absence(modules):
    async def run():
        task, _ = modules
        channel = SimpleNamespace(_workspace=SimpleNamespace(task_tracker=SimpleNamespace()))
        execution = task.TaskExecution(channel, SimpleNamespace(process="actual-process"))
        execution.matrix.start = Mock(return_value={"etag": "index"})
        execution.matrix.input = Mock(return_value={"intent": "message", "work": {"kind": "message"}})
        execution.matrix.ingest = Mock()
        event = SimpleNamespace(event_id="$input", sender="@human:test", server_timestamp=12,
            source={"type": "m.room.message", "content": {"body": "accepted original input"}})
        await execution.accept_input("!room:test", event, "context")
        input_ = execution.matrix.ingest.call_args.args[2][0]
        assert input_["intent"] == "message" and input_["work"] == {"kind": "message"}
        execution.matrix.input.side_effect = RecoveryUnavailable("read unavailable")
        with pytest.raises(RecoveryUnavailable):
            await execution.accept_input("!room:test", event, "message")
        assert execution.matrix.ingest.call_count == 1
    asyncio.run(run())


@pytest.mark.parametrize("adapter_present", [True, False])
def test_required_input_guard_does_not_depend_on_receipt_service(modules, adapter_present):
    async def run():
        _, receipts = modules
        order = []
        state = {}
        agent = SimpleNamespace(state=SimpleNamespace(middle_context=state),
            state_dict=lambda: {"state": {"middle_context": state}})
        execution = SimpleNamespace(commit_inputs=AsyncMock(side_effect=lambda *a: order.append("fact"))) if adapter_present else None
        channel = SimpleNamespace(_session_receipts=None, _task_execution=execution)
        ctx = SimpleNamespace(session_id="native", mode_state={},
            request=SimpleNamespace(channel="agentteams_matrix", user_id="!room:test", channel_meta={
                "room_id": "!room:test", "session_source_event_ids": ["$input"], "shared_input_keys": ["required"]}),
            workspace=SimpleNamespace(session=SimpleNamespace(save_session_state=AsyncMock(side_effect=lambda **k: order.append("save"))),
                channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=channel))))
        async def reply(**kwargs):
            yield SimpleNamespace(type="REPLY_START")
            order.append("model")
        async def consume():
            async for _ in receipts.SessionReceiptMiddleware(ctx).on_reply(agent, {}, reply):
                pass
        if adapter_present:
            await consume()
            assert order == ["save", "fact", "model"]
        else:
            with pytest.raises(RuntimeError, match="persistence adapter"):
                await consume()
            assert not order
    asyncio.run(run())
