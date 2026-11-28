"""Startup recovery uses authority before native admission, without Matrix input."""
import asyncio
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import time
from types import SimpleNamespace
from unittest.mock import ANY, AsyncMock

import pytest

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "shared/lib"))
from task_recovery import Recovery, RecoveryConflict, RecoveryUnavailable


def pending_reply(text, txn):
    data = text.encode()
    return {"result": {"key": "saved-report", "sha256": hashlib.sha256(data).hexdigest(), "size": len(data)},
            "delivery": "pending", "reply_txn": txn, "reply_attempt": {"state": "ready"}}


@pytest.fixture
def adapter(monkeypatch, tmp_path):
    path = ROOT / "qwenpaw/adapters/matrix/agentteams_matrix/task_execution.py"
    spec = importlib.util.spec_from_file_location("_task_execution_test", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    monkeypatch.setenv("AGENTTEAMS_PROCESS_ID", "native-process")
    task = {"task_id": "T", "room_id": "!task:hs", "status": "in_progress", "title": "Three steps",
            "execution": {"generation": 2, "instance_id": "container/start", "process_id": "native-process", "state": "active", "runtime": "qwenpaw",
                          "checkpoint": {"version": 4, "completed_steps": ["one"], "remaining_steps": ["two", "three"]}}}
    authority = SimpleNamespace(
        process="native-process",
        discover=lambda: {"instance_id": "container/start", "tasks": [{"team_id": "team", "task": copy.deepcopy(task), "etag": "v1"}]},
        claim=lambda snapshot, entry: {"task": copy.deepcopy(task), "spec": "Continue two and three", "execution": task["execution"]},
        current=lambda task_id, team: copy.deepcopy(task),
        interrupted=lambda *args: None,
    )
    def checkpoint(task_id, team, cp):
        assert (task_id, team) == ("T", "team")
        old = task["execution"]["checkpoint"]
        assert cp["version"] == old["version"] + 1
        old.update(cp)
        return copy.deepcopy(task)
    authority.checkpoint = checkpoint
    authority.cache = None
    authority.config_generation = 3
    authority.session_etag = ""
    def request(method, path, body=None, etag=None, *, create=False):
        if path == "runtime/matrix":
            return {"runtime": "qwenpaw", "instance_id": "container/start", "etag": ""}
        assert path.startswith("runtime/sessions?")
        cp = task["execution"]["checkpoint"]
        view = {"runtime": "qwenpaw", "instance_id": "container/start", "generation": task["execution"]["generation"],
                "work": {"kind": "task", "team_id": "team", "task_id": "T"},
                "checkpoint_version": cp["version"], "checkpoint_digest": Recovery.digest(cp),
                "config_generation": authority.config_generation, "cache": copy.deepcopy(authority.cache),
                "etag": authority.session_etag, "compatible": True}
        if method == "GET":
            return view
        assert method == "PUT"
        assert (create and not authority.session_etag) or etag == authority.session_etag
        for key in ("generation", "checkpoint_version", "checkpoint_digest", "config_generation"):
            assert body[key] == view[key]
        authority.cache = {**body, "runtime": "qwenpaw"}
        authority.session_etag = "committed-cache"
        return {**view, "cache": copy.deepcopy(authority.cache), "etag": authority.session_etag}
    authority.request = request
    store = SimpleNamespace(get_session_state_dict=AsyncMock(return_value={}), save_dir=str(tmp_path / "sessions"))
    channel = SimpleNamespace(channel="agentteams_matrix", matrix_user_id="@worker:hs",
                              _client=SimpleNamespace(whoami=AsyncMock(return_value=SimpleNamespace(user_id="@worker:hs", device_id="DEVICE"))),
                              _workspace=SimpleNamespace(session=store, task_tracker=SimpleNamespace()), _workspace_dir=tmp_path,
                              send_task_reply=AsyncMock(return_value="$reply"), enqueue_recovered_task=AsyncMock())
    recovery = module.TaskExecution(channel, authority)
    return recovery, authority, task, channel


def test_startup_claims_and_resumes_without_an_external_message(adapter):
    recovery, authority, task, channel = adapter
    order = []
    claim = authority.claim
    authority.claim = lambda *args: (order.append("claim"), claim(*args))[1]
    request = authority.request
    def fenced_request(method, path, *args, **kwargs):
        if method == "POST" and path == "runtime/matrix":
            order.append("start")
        return request(method, path, *args, **kwargs)
    authority.request = fenced_request
    async def enqueue(payload):
        order.append("native")
        assert payload["meta"]["task_execution"]["task_id"] == "T"
        assert payload["meta"]["task_id"] == "T"
        assert payload["meta"]["team_id"] == "team"
        assert payload["meta"]["execution_generation"] == 2
        assert payload["meta"]["session_source_event_ids"] == []
        assert payload["meta"]["room_id"] == "!task:hs"
        assert '"completed_steps": ["one"]' in payload["recovery_text"]
        assert "Continue two and three" in payload["recovery_text"]
    channel.enqueue_recovered_task.side_effect = enqueue
    asyncio.run(recovery.recover())
    assert order == ["start", "claim", "native"]


def test_same_generation_is_admitted_once_and_conflicts_never_enter_native(adapter):
    recovery, authority, task, channel = adapter
    async def scenario():
        await recovery.recover()
        await recovery.recover()
        assert channel.enqueue_recovered_task.await_count == 1
        task["execution"]["generation"] += 1
        authority.claim = lambda *args: (_ for _ in ()).throw(RecoveryConflict("owned by another process"))
        await recovery.recover()
        assert channel.enqueue_recovered_task.await_count == 1
    asyncio.run(scenario())


def test_polling_does_not_reclaim_a_running_operation_or_deliver_its_reply(adapter):
    recovery, authority, task, channel = adapter
    calls = []
    claim = authority.claim
    def counted_claim(*args):
        calls.append(True)
        return claim(*args)
    authority.claim = counted_claim
    async def scenario():
        await recovery.recover()
        task["execution"]["checkpoint"]["operation"] = {"id": "effect-two", "state": "started"}
        await recovery.recover()
        assert len(calls) == 1
        # A turn owns delivery until it finishes; a scan must not reclaim
        # execution or race its pending checkpoint/Matrix acknowledgement.
        task["execution"]["checkpoint"].update(delivery="pending", reply_txn="turn-reply")
        await recovery.recover()
        assert len(calls) == 1
        channel.enqueue_recovered_task.assert_awaited_once()
        channel.send_task_reply.assert_not_awaited()
    asyncio.run(scenario())


@pytest.mark.parametrize("changed", ["process_id", "instance_id", "generation"])
def test_admission_does_not_hide_a_replaced_execution(adapter, changed):
    recovery, authority, task, channel = adapter
    async def scenario():
        await recovery.recover()
        task["execution"][changed] = 3 if changed == "generation" else "successor"
        calls = []
        def conflict(*args):
            calls.append(True)
            raise RecoveryConflict("owned by another process")
        authority.claim = conflict
        await recovery.recover()
        assert calls == [True]
        channel.enqueue_recovered_task.assert_awaited_once()
    asyncio.run(scenario())


def test_saved_result_is_only_resent_with_its_durable_transaction(adapter):
    recovery, authority, task, channel = adapter
    task["status"] = "submitted"
    task["execution"]["checkpoint"].update(pending_reply("saved answer", "stable-transaction"))
    claim = authority.claim
    authority.claim = lambda *args: {**claim(*args), "saved_result": "saved answer"}
    delivered = []
    authority.delivered = lambda *args: (delivered.append(args), copy.deepcopy(task))[1]
    asyncio.run(recovery.recover())
    channel.send_task_reply.assert_awaited_once_with("!task:hs", "saved answer", "stable-transaction", ANY)
    identity = channel.send_task_reply.await_args.args[3]
    assert (identity["team_id"], identity["task_id"], identity["generation"]) == ("team", "T", 2)
    assert delivered == [("T", "team", "$reply")]
    channel.enqueue_recovered_task.assert_not_awaited()


@pytest.mark.parametrize("failure", ["send", "ack"])
def test_same_process_retries_saved_delivery_only_after_native_stream_closes(
        adapter, native_channel, tmp_path, monkeypatch, failure):
    from qwenpaw.app.chats.session import SafeJSONSession
    recovery, authority, task, _ = adapter
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None)
    channel._workspace = SimpleNamespace(session=SafeJSONSession(str(tmp_path / "native")),
                                         task_tracker=SimpleNamespace())
    channel._send_typing = AsyncMock()
    channel.enqueue_recovered_task = AsyncMock()
    channel.matrix_user_id = "@worker:hs"
    channel._client = SimpleNamespace(whoami=AsyncMock(return_value=SimpleNamespace(user_id="@worker:hs", device_id="DEVICE")))
    channel.send_task_reply = AsyncMock(return_value="$reply")
    recovery = type(recovery)(channel, authority)
    channel._task_execution = recovery
    cp = task["execution"]["checkpoint"]
    attempts = []
    def prepared(*args):
        task["status"] = "submitted"
        cp.update(pending_reply("saved answer", "stable-txn"))
        return task, "saved answer"
    authority.prepare_reply = prepared
    claim = authority.claim
    authority.claim = lambda *args: {**claim(*args), "saved_result": "saved answer"}
    def delivered(*args):
        attempts.append(args)
        if failure == "ack" and len(attempts) == 1:
            raise RecoveryUnavailable("delivery acknowledgement unavailable")
        cp.update(delivery="delivered")
    authority.delivered = delivered
    if failure == "send":
        channel.send_task_reply.side_effect = [RuntimeError("Matrix send unavailable"), "$reply"]
    async def native_stream(self, payload):
        # A scan during completion must not race the admitted native turn.
        prepared()
        await recovery.recover()
        channel.send_task_reply.assert_not_awaited()
        await channel._on_process_completed(SimpleNamespace(session_id=payload["meta"]["task_session_id"]), "!task:hs", {
            **payload["meta"], native_channel._MATRIX_STREAMING_FINAL_TEXT_KEY: "native answer",
        })
        yield "finished"
    monkeypatch.setattr(native_channel.BaseChannel, "_stream_with_tracker", native_stream)
    async def scenario():
        await recovery.recover()
        payload = channel.enqueue_recovered_task.call_args.args[0]
        with pytest.raises((RuntimeError, RecoveryUnavailable)):
            async for _ in channel._stream_recovered_task(payload):
                pass
        assert cp["delivery"] == "pending"
        await recovery.recover()  # No restart or new Matrix message.
        assert cp["delivery"] == "delivered"
        assert channel.send_task_reply.await_args_list[0] == channel.send_task_reply.await_args_list[1]
        channel.send_task_reply.assert_awaited_with("!task:hs", "saved answer", "stable-txn", ANY)
        channel.enqueue_recovered_task.assert_awaited_once()
        assert task["execution"]["generation"] == 2
        await recovery.recover()
        assert channel.send_task_reply.await_count == 2
    asyncio.run(scenario())


@pytest.mark.parametrize('status,report,allowed', [
    ('submitted', 'shared/tasks/T/result.md', True),
    ('in_progress', 'shared/tasks/T/result.md', False),
    ('submitted', None, False),
])
def test_empty_native_final_requires_a_submitted_authoritative_report(adapter, status, report, allowed):
    recovery, authority, task, channel = adapter
    task['status'] = status
    if report:
        task['result_path'] = report
    cp = task['execution']['checkpoint']
    if allowed:
        cp.update(pending_reply('authoritative report', 'saved-report'))
    prepared = []
    authority.prepare_reply = lambda *args: (prepared.append(args), ({'execution': {'checkpoint': cp}, 'room_id': '!task:hs'}, 'authoritative report'))[1]
    authority.delivered = lambda *args: copy.deepcopy(task)
    async def scenario():
        if allowed:
            await recovery.finish({'task_id': 'T', 'team_id': 'team', **task['execution']}, 'session', '')
            assert prepared == [('T', 'team', '')]
            channel.send_task_reply.assert_awaited_once_with('!task:hs', 'authoritative report', 'saved-report', ANY)
        else:
            with pytest.raises(RuntimeError, match='without a durable result'):
                await recovery.finish({'task_id': 'T', 'team_id': 'team', **task['execution']}, 'session', '')
            assert not prepared
            channel.send_task_reply.assert_not_awaited()
    asyncio.run(scenario())


def test_native_completion_after_submit_does_not_require_another_model_message(native_channel):
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None)
    channel._task_execution = SimpleNamespace(finish=AsyncMock())
    channel._send_typing = AsyncMock()
    identity = {'task_id': 'T', 'team_id': 'team', 'generation': 2}
    asyncio.run(channel._on_process_completed(SimpleNamespace(session_id='native-session'), '!room', {'task_execution': identity}))
    channel._task_execution.finish.assert_awaited_once_with(identity, 'native-session', '')
    channel._send_typing.assert_awaited_once_with('!room', False)


def test_saved_report_does_not_bypass_unknown_operation_verification(adapter):
    recovery, authority, task, channel = adapter
    task.update(status='submitted', result_path='shared/tasks/T/result.md')
    operation = {'id': 'unknown-effect', 'state': 'unknown'}
    task['execution']['checkpoint']['operation'] = operation.copy()
    authority.prepare_reply = lambda *args: (_ for _ in ()).throw(RecoveryConflict('verify the previous operation'))
    with pytest.raises(RecoveryConflict, match='verify the previous operation'):
        asyncio.run(recovery.finish({'task_id': 'T', 'team_id': 'team', 'generation': 2}, 'session', ''))
    assert task['execution']['checkpoint']['operation'] == operation
    channel.send_task_reply.assert_not_awaited()


def test_empty_final_cannot_deliver_a_reassigned_task_report(adapter):
    recovery, authority, task, channel = adapter
    task.update(status='submitted', result_path='shared/tasks/T/result.md')
    task['execution']['process_id'] = 'successor'
    with pytest.raises(RecoveryConflict, match='replaced'):
        asyncio.run(recovery.finish({'task_id': 'T', 'team_id': 'team', 'generation': 2}, 'session', ''))
    channel.send_task_reply.assert_not_awaited()


def test_unknown_operation_stays_parked_without_native_reasoning(adapter):
    recovery, authority, task, channel = adapter
    task["execution"]["checkpoint"]["operation"] = {"id": "effect-one", "state": "unknown"}
    task["execution"]["state"] = "needs_verification"
    asyncio.run(recovery.recover())
    channel.enqueue_recovered_task.assert_not_awaited()
    assert task["execution"]["checkpoint"]["operation"] == {"id": "effect-one", "state": "unknown"}


def test_cancelled_or_reassigned_and_replaced_process_cannot_run(adapter):
    recovery, authority, task, channel = adapter
    identity = {"task_id": "T", "team_id": "team", "generation": 2}
    authority.current = lambda *args: None
    with pytest.raises(RecoveryConflict):
        recovery.verify(identity)
    authority.current = lambda *args: task
    task["execution"]["process_id"] = "successor"
    with pytest.raises(RecoveryConflict):
        recovery.verify(identity)
    channel.enqueue_recovered_task.assert_not_awaited()


def test_native_cache_reuse_requires_file_expiry_and_latest_checkpoint(adapter):
    recovery, authority, task, channel = adapter
    async def scenario():
        first = await recovery.select_session("team", task)
        await recovery.remember({"task_id": "T", "team_id": "team", **task["execution"], "checkpoint_digest": Recovery.digest(task["execution"]["checkpoint"])}, first)
        # No persisted native session: the index alone cannot authorize reuse.
        missing = await recovery.select_session("team", task)
        assert missing != first
        channel._workspace.session.get_session_state_dict.return_value = {"agent": {"state": {"memory": "step one"}}}
        await recovery.remember({"task_id": "T", "team_id": "team", **task["execution"], "checkpoint_digest": Recovery.digest(task["execution"]["checkpoint"])}, first)
        assert await recovery.select_session("team", task) == first
        task["execution"]["checkpoint"]["version"] += 1
        newer = await recovery.select_session("team", task)
        assert newer != first  # Switching back cannot use an older checkpoint.
        task["execution"]["checkpoint"]["version"] -= 1
        authority.cache["expires_at"] = 0
        assert await recovery.select_session("team", task) != first
    asyncio.run(scenario())


def test_native_file_changed_since_checkpoint_does_not_reuse(adapter):
    recovery, authority, task, channel = adapter
    async def scenario():
        channel._workspace.session.get_session_state_dict.return_value = {"agent": {"state": {"memory": "step one"}}}
        session = await recovery.select_session("team", task)
        await recovery.remember({"task_id": "T", "team_id": "team", **task["execution"], "checkpoint_digest": Recovery.digest(task["execution"]["checkpoint"])}, session)
        channel._workspace.session.get_session_state_dict.return_value = {"agent": {"state": {"memory": "uncommitted side effect"}}}
        assert await recovery.select_session("team", task) != session
        assert not (Path(channel._workspace.session.save_dir) / ".agentteams-task-recovery").exists()
    asyncio.run(scenario())


def test_unavailable_authority_is_not_a_cancellation_or_new_execution(adapter):
    recovery, authority, task, channel = adapter
    authority.discover = lambda: (_ for _ in ()).throw(RecoveryUnavailable("offline"))
    with pytest.raises(RecoveryUnavailable):
        asyncio.run(recovery.recover())
    channel.enqueue_recovered_task.assert_not_awaited()


@pytest.fixture
def native_channel(monkeypatch):
    pytest.importorskip("qwenpaw.app.channels.base")
    path = ROOT / "qwenpaw/adapters/matrix/agentteams_matrix/channel.py"
    spec = importlib.util.spec_from_file_location("_native_task_channel", path)
    module = importlib.util.module_from_spec(spec)
    monkeypatch.setitem(sys.modules, spec.name, module)
    spec.loader.exec_module(module)
    return module


def test_actual_sdk_session_store_and_request_reconstruction(adapter, native_channel, tmp_path):
    from qwenpaw.app.chats.session import SafeJSONSession
    recovery, authority, task, _ = adapter
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None, matrix_user_id="@worker:hs")
    store = SafeJSONSession(str(tmp_path / "native"))
    channel._workspace = SimpleNamespace(session=store, task_tracker=SimpleNamespace())
    recovery = type(recovery)(channel, authority)
    queued = []
    channel._enqueue = queued.append
    async def scenario():
        await recovery.recover()
        assert len(queued) == 1
        request = channel.build_agent_request_from_native(queued[0])
        assert request.session_id == queued[0]["meta"]["task_session_id"]
        assert request.user_id == "!task:hs"
        assert channel.get_to_handle_from_request(request) == "!task:hs"
        assert request.channel_meta["session_source_event_ids"] == []
        state = {"state": {"memory": {"messages": ["step one done"]}}}
        await store.save_session_state(session_id=request.session_id, user_id=request.user_id,
                                       channel=channel.channel, agent=SimpleNamespace(state_dict=lambda: state))
        request.channel_meta["task_execution"]["checkpoint_digest"] = Recovery.digest(task["execution"]["checkpoint"])
        await recovery.remember(request.channel_meta["task_execution"], request.session_id)
        assert await recovery.select_session("team", task) == request.session_id
        task["execution"]["checkpoint"]["version"] += 1
        assert await recovery.select_session("team", task) != request.session_id
        assert await store.get_session_state_dict(session_id=request.session_id, user_id=request.user_id,
                                                  channel=channel.channel) == {"agent": state}
    asyncio.run(scenario())


def test_task_cache_follows_sdk_post_completion_session_save(adapter, native_channel, tmp_path, monkeypatch):
    from qwenpaw.app.chats.session import SafeJSONSession
    recovery, authority, task, _ = adapter
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None)
    store = SafeJSONSession(str(tmp_path / "native"))
    channel._workspace = SimpleNamespace(session=store, task_tracker=SimpleNamespace())
    channel._send_typing = AsyncMock()
    channel.matrix_user_id = "@worker:hs"
    channel._client = SimpleNamespace(whoami=AsyncMock(return_value=SimpleNamespace(user_id="@worker:hs", device_id="DEVICE")))
    channel.send_task_reply = AsyncMock(return_value="$reply")
    recovery = type(recovery)(channel, authority)
    channel._task_execution = recovery
    identity = {"task_id": "T", "team_id": "team", **task["execution"]}
    session = "matrix:task:" + recovery.key("team", "T") + ":native"
    task["execution"]["checkpoint"].update(pending_reply("saved result", "result"))
    authority.prepare_reply = lambda *args: (task, "saved result")
    authority.delivered = lambda *args: (task["execution"]["checkpoint"].update(delivery="delivered"), copy.deepcopy(task))[1]
    async def save(memory):
        await store.save_session_state(session_id=session, user_id="!task:hs", channel=channel.channel,
                                       agent=SimpleNamespace(state_dict=lambda: {"memory": memory}))
    async def native_stream(self, payload):
        await save("native tool result")
        await channel._on_process_completed(SimpleNamespace(session_id=session), "!task:hs", {
            "task_execution": identity, native_channel._MATRIX_STREAMING_FINAL_TEXT_KEY: "result",
        })
        # BaseChannel commits turn usage after the completion callback and
        # can persist the native Agent again before closing its stream.
        await save("native tool result plus persisted turn usage")
        yield "finished"
    monkeypatch.setattr(native_channel.BaseChannel, "_stream_with_tracker", native_stream)
    async def scenario():
        async for _ in channel._stream_recovered_task({"meta": {"task_execution": identity, "task_session_id": session}}):
            pass
        assert await recovery.select_session("team", task) == session
    asyncio.run(scenario())


def test_startup_scan_waits_for_committed_workspace_without_new_matrix_input(native_channel):
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None)
    channel._workspace = SimpleNamespace(_manager=None)
    recovered = asyncio.Event()
    channel._task_execution = SimpleNamespace(recover=AsyncMock(side_effect=lambda: recovered.set()))
    async def scenario():
        loop = asyncio.create_task(channel._recover_owned_tasks())
        await asyncio.sleep(0)
        channel._task_execution.recover.assert_not_awaited()
        channel._workspace._manager = object()
        await asyncio.wait_for(recovered.wait(), 1)
        loop.cancel()
        await asyncio.gather(loop, return_exceptions=True)
    asyncio.run(scenario())


def test_reply_uses_saved_transaction_and_requires_matrix_event_id(native_channel):
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None)
    channel._room_send_with_retry = AsyncMock(return_value=SimpleNamespace(event_id="$persisted"))
    async def scenario():
        assert await channel.send_task_reply("!room", "saved result", "stable-txn", {"team_id": "team", "task_id": "T", "generation": 2}) == "$persisted"
        channel._room_send_with_retry.assert_awaited_once_with("!room", "m.room.message", {
            "msgtype": "m.text", "body": "saved result", "org.agentteams.progress_only": True, "org.agentteams.reply_txn": "stable-txn",
            "org.agentteams.run_id": "task:team:T:2",
            "org.agentteams.run": {"version": "1", "run_id": "task:team:T:2", "blocks": [{"type": "text", "text": "saved result"}]},
        }, tx_id="stable-txn")
        channel._room_send_with_retry.return_value = SimpleNamespace()
        with pytest.raises(RuntimeError, match="not acknowledged"):
            await channel.send_task_reply("!room", "saved result", "stable-txn", {"team_id": "team", "task_id": "T", "generation": 2})
    asyncio.run(scenario())


@pytest.mark.parametrize("work", ["task", "message"])
@pytest.mark.parametrize("proof", ["marker", "legacy", "absent"])
def test_device_replacement_requires_own_verified_reply(native_channel, adapter, work, proof):
    import nio
    from unittest.mock import Mock
    from qwenpaw_message_execution import MessageExecution

    recovery, authority, task, _ = adapter
    room, worker, txn = task["room_id"], "@worker:hs", "saved-txn"
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None, matrix_user_id=worker)
    channel._workspace = SimpleNamespace(task_tracker=SimpleNamespace())
    client = channel._client = nio.AsyncClient("https://hs", worker, "D2")
    client.whoami = AsyncMock(return_value=nio.WhoamiResponse(worker, "D2", False))
    def event(event_id, sender, body, marker=None, unsigned=None):
        content = {"msgtype": "m.text", "body": body}
        if marker is not None:
            content["org.agentteams.reply_txn"] = marker
        return nio.Event.parse_event({"type": "m.room.message", "sender": sender, "room_id": room, "event_id": event_id,
            "origin_server_ts": 1, "content": content, "unsigned": {"transaction_id": unsigned} if unsigned else {}})
    decoys = [event("$wrong-sender", "@other:hs", "saved answer", txn), event("$wrong-text", worker, "different", txn),
              event("$wrong-marker", worker, "saved answer", "different", txn)]
    if proof != "legacy":
        decoys.append(event("$missing-marker", worker, "saved answer", unsigned=txn))
    match = {"marker": [event("$original", worker, "saved answer", txn)],
             "legacy": [event("$original", worker, "saved answer", unsigned=txn)], "absent": []}[proof]
    client.room_messages = AsyncMock(side_effect=[
        nio.RoomMessagesResponse(room, decoys, "newest", "next"), nio.RoomMessagesResponse(room, match, "next", None)])
    cp = {**task["execution"]["checkpoint"], **pending_reply("saved answer", txn),
          "reply_attempt": {"state": "sending", "user_id": worker, "device_id": "D1"}}
    if proof == "legacy":
        del cp["reply_attempt"]
    identity = {"task_id": "T", "team_id": "team", **task["execution"]}
    ack = Mock()
    channel._room_send_with_retry = AsyncMock()
    recovery = type(recovery)(channel, authority)
    if work == "task":
        authority.delivered = ack
        publish = recovery.deliver(identity, room, cp, "saved answer")
    else:
        recovery.matrix.message_delivered = ack
        publish = MessageExecution(channel, recovery).deliver({"identity": {"event_key": "e", **identity},
            "execution": {"checkpoint": cp}, "input": {"room_id": room}, "saved_result": "saved answer"})
    async def scenario():
        if proof == "absent":
            with pytest.raises(RecoveryConflict, match="needs verification"):
                await publish
            ack.assert_not_called()
        else:
            await publish
            assert ack.call_count == 1 and ack.call_args.args[-1] == "$original"
        channel._room_send_with_retry.assert_not_awaited()
        assert [c.kwargs["start"] for c in client.room_messages.await_args_list] == [None, "next"]
        await client.close()
    asyncio.run(scenario())


@pytest.mark.parametrize("lost", ["state", "generation", "checkpoint"])
def test_task_reply_losing_device_bind_cannot_send(adapter, lost):
    recovery, authority, task, channel = adapter
    cp = {**task["execution"]["checkpoint"], **pending_reply("saved report", "txn")}
    identity = {"team_id": "team", "task_id": "T", **task["execution"]}
    write = authority.checkpoint
    def changed(*args):
        response = write(*args)
        execution = response["execution"]
        if lost == "state":
            execution["state"] = "released"
        elif lost == "generation":
            execution["generation"] += 1
        else:
            execution["checkpoint"]["reply_attempt"] = {"state": "ready"}
        return response
    authority.checkpoint = changed
    with pytest.raises(RecoveryConflict, match="not bound"):
        asyncio.run(recovery.deliver(identity, task["room_id"], cp, "saved report"))
    channel.send_task_reply.assert_not_awaited()


def test_replacement_cancels_native_stream_before_interruption_checkpoint(native_channel, monkeypatch):
    module = native_channel
    monkeypatch.setattr(module, "TASK_OWNERSHIP_POLL_S", 0.01)
    channel = module.AgentTeamsMatrixChannel(process=lambda _: None)
    order = []
    def verify(*args, **kwargs):
        if "started" in order:
            raise RecoveryConflict("reassigned")
    def interrupted(*args):
        order.append("checkpoint")
    channel._task_execution = SimpleNamespace(verify=verify, recovery=SimpleNamespace(interrupted=interrupted),
                                             native_closed=lambda identity: order.append("native closed"))
    async def native_stream(self, payload):
        order.append("started")
        try:
            yield "native output"
            await asyncio.Event().wait()
        finally:
            order.append("tools stopped")
    monkeypatch.setattr(module.BaseChannel, "_stream_with_tracker", native_stream)
    async def scenario():
        async def consume():
            async for _ in channel._stream_recovered_task({"meta": {"task_execution": {"task_id": "T", "team_id": "team"}}}):
                pass
        with pytest.raises(asyncio.CancelledError):
            await asyncio.wait_for(consume(), 1)
    asyncio.run(scenario())
    assert order == ["started", "tools stopped", "checkpoint", "native closed"]


def test_task_queue_keys_do_not_merge_different_tasks_in_one_room(native_channel):
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None)
    payloads = [{"meta": {"room_id": "!room", "task_execution": {"task_id": task},
                          "task_session_id": "native-" + task}} for task in ("a", "b")]
    assert channel.get_debounce_key(payloads[0]) != channel.get_debounce_key(payloads[1])


def test_reload_shares_admission_and_does_not_start_task_twice(adapter):
    recovery, authority, task, channel = adapter
    async def scenario():
        await recovery.recover()
        replacement = type(recovery)(channel, authority)
        assert replacement.native_active is recovery.native_active
        task["execution"]["checkpoint"].update(delivery="pending", reply_txn="active-turn")
        await replacement.recover()
        channel.enqueue_recovered_task.assert_awaited_once()
        channel.send_task_reply.assert_not_awaited()
    asyncio.run(scenario())


def test_distinct_task_sessions_in_one_room_wait_for_previous_execution(native_channel, monkeypatch):
    module = native_channel
    channel = module.AgentTeamsMatrixChannel(process=lambda _: None)
    tracker = SimpleNamespace(snapshot_active_tasks=AsyncMock(return_value={}))
    manager = SimpleNamespace()
    workspace = SimpleNamespace(_manager=manager, agent_id="default", task_tracker=tracker)
    manager.get_loaded_agent = lambda _: workspace
    workspace.chat_manager = SimpleNamespace(get_or_create_chat=AsyncMock(return_value=SimpleNamespace(id="chat")))
    workspace.channel_manager = SimpleNamespace(get_channel=AsyncMock(return_value=channel))
    channel._workspace = workspace
    entered = []
    release = asyncio.Event()
    async def native_consume(self, request, payload):
        entered.append(request.session_id)
        await release.wait()
    monkeypatch.setattr(module.BaseChannel, "_consume_with_tracker", native_consume)
    async def scenario():
        requests = [SimpleNamespace(session_id="task-" + task, user_id="!room", channel_meta={
            "room_id": "!room", "event_id": task,
        }) for task in ("a", "b")]
        consumers = [asyncio.create_task(channel._consume_with_tracker(request, {})) for request in requests]
        for _ in range(10):
            await asyncio.sleep(0)
        assert entered == ["task-a"]
        release.set()
        await asyncio.gather(*consumers)
        assert entered == ["task-a", "task-b"]
    asyncio.run(scenario())


def test_final_callback_prefers_authoritative_saved_result(adapter):
    recovery, authority, task, channel = adapter
    task["execution"]["checkpoint"].update(pending_reply("saved authoritative answer", "saved-txn"))
    authority.prepare_reply = lambda *args: ({"task": task, "execution": task["execution"]}, "saved authoritative answer")
    delivered = []
    authority.delivered = lambda *args: (delivered.append(args), copy.deepcopy(task))[1]
    asyncio.run(recovery.finish({"task_id": "T", "team_id": "team", **task["execution"]}, "native-session", "stale native answer"))
    channel.send_task_reply.assert_awaited_once_with("!task:hs", "saved authoritative answer", "saved-txn", ANY)
    assert delivered == [("T", "team", "$reply")]


@pytest.mark.parametrize("state", ["active", "needs_verification"])
def test_recovered_native_reasoning_is_fenced_before_task_tools(adapter, monkeypatch, state):
    import task_recovery
    recovery, authority, task, channel = adapter
    task["execution"]["state"] = state
    path = ROOT / "qwenpaw/adapters/teamharness/plugin.py"
    spec = importlib.util.spec_from_file_location("_recovery_teamharness", path)
    plugin = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(plugin)
    monkeypatch.setattr(task_recovery, "Recovery", lambda: authority)
    ctx = SimpleNamespace(agent=object(), request=SimpleNamespace(channel_meta={"task_execution": {
        "task_id": "T", "team_id": "team", "generation": 2, "instance_id": "container/start",
    }}), extras={}, inject_context=lambda *args, **kwargs: None)
    async def scenario():
        if state == "needs_verification":
            with pytest.raises(RecoveryConflict):
                await plugin._manager_report_context(ctx)
            assert "execution_task_token" not in ctx.extras
            return
        assert await plugin._manager_report_context(ctx) is False
        assert plugin._execution_task.get() == "T"
        plugin._execution_task.reset(ctx.extras.pop("execution_task_token"))
        task["execution"]["generation"] = 3
        with pytest.raises(RecoveryConflict):
            await plugin._manager_report_context(ctx)
    asyncio.run(scenario())


def test_verified_matrix_source_cannot_be_attached_to_another_task_room(adapter):
    recovery, authority, task, channel = adapter
    asyncio.run(recovery.recover("T", SimpleNamespace(event_id="$source"), source_room="!other:hs"))
    channel.enqueue_recovered_task.assert_not_awaited()


def test_recovered_error_records_uncertainty_without_requesting_a_new_message(native_channel):
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None)
    checkpointed = []
    channel._task_execution = SimpleNamespace(recovery=SimpleNamespace(interrupted=lambda *args: checkpointed.append(args)))
    channel._send_typing = AsyncMock()
    request = SimpleNamespace(channel_meta={"task_execution": {"task_id": "T", "team_id": "team"}})
    asyncio.run(channel._on_consume_error(request, "!room", "native failure"))
    assert checkpointed == [("T", "team")]
    channel._send_typing.assert_awaited_once()


def test_ordinary_error_path_keeps_native_error_handling(native_channel, monkeypatch):
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None)
    base = AsyncMock()
    monkeypatch.setattr(native_channel.BaseChannel, "_on_consume_error", base)
    request = SimpleNamespace(channel_meta={})
    asyncio.run(channel._on_consume_error(request, "!room", "native failure"))
    base.assert_awaited_once()


def test_common_descriptor_reuses_native_without_a_private_index(adapter):
    import time
    from task_recovery import Recovery
    from worker_sessions import WorkerSessions
    recovery, authority, task, channel = adapter
    native = {"agent": {"memory": "step one tool result"}}
    session = "matrix:task:" + recovery.key("team", "T") + ":persisted"
    cache = {"runtime": "qwenpaw", "native_ref": session, "native_digest": Recovery.digest(native),
             "expires_at": time.time()+3600, "checkpoint_version": 4,
             "checkpoint_digest": Recovery.digest(task["execution"]["checkpoint"]), "config_generation": 3}
    view = {"runtime": "qwenpaw", "cache": cache, "compatible": True, "generation": 2,
            **{key: cache[key] for key in ("checkpoint_version", "checkpoint_digest", "config_generation")}}
    authority.request = lambda *args, **kwargs: copy.deepcopy(view)
    channel._workspace.session.get_session_state_dict.return_value = native
    assert asyncio.run(recovery.select_session("team", task)) == session
    view["config_generation"] = 4
    view["compatible"] = False
    assert asyncio.run(recovery.select_session("team", task)) != session


def test_unknown_effect_is_parked_before_native_session_selection(adapter):
    recovery, authority, task, channel = adapter
    task["execution"]["state"] = "needs_verification"
    task["execution"]["checkpoint"]["operation"] = {"id": "external-effect", "state": "unknown"}
    asyncio.run(recovery.recover())
    channel.enqueue_recovered_task.assert_not_awaited()
    channel._workspace.session.get_session_state_dict.assert_not_awaited()


def test_private_cache_is_retained_but_cannot_override_common_authority(adapter):
    recovery, authority, task, channel = adapter
    path = Path(channel._workspace.session.save_dir) / ".agentteams-task-recovery"
    path.parent.mkdir(parents=True)
    legacy = json.dumps({recovery.key("team", "T"): {
        "session_id": "matrix:task:"+recovery.key("team", "T")+":old",
        "runtime": "qwenpaw", "checkpoint_digest": Recovery.digest(task["execution"]["checkpoint"]),
        "expires_at": 9999999999}})
    path.write_text(legacy)
    selected = asyncio.run(recovery.select_session("team", task))
    assert not selected.endswith(":old")
    assert path.read_text() == legacy


def test_late_sdk_save_cannot_relabel_its_old_checkpoint(adapter):
    recovery, authority, task, channel = adapter
    identity = {"task_id": "T", "team_id": "team", **task["execution"],
                "checkpoint_digest": Recovery.digest(task["execution"]["checkpoint"])}
    channel._workspace.session.get_session_state_dict.return_value = {"agent": {"memory": "old turn"}}
    task["execution"]["checkpoint"]["version"] += 1
    asyncio.run(recovery.remember(identity, "native"))
    assert authority.cache is None


@pytest.mark.parametrize("failure", [RecoveryConflict, RecoveryUnavailable])
def test_optional_cache_failure_preserves_the_saved_checkpoint(adapter, failure):
    recovery, authority, task, channel = adapter
    before = copy.deepcopy(task)
    identity = {"task_id": "T", "team_id": "team", **task["execution"],
                "checkpoint_digest": Recovery.digest(task["execution"]["checkpoint"])}
    channel._workspace.session.get_session_state_dict.return_value = {"agent": {"memory": "final SDK turn"}}
    request = authority.request
    def unavailable(method, path, *args, **kwargs):
        if method == "PUT":
            raise failure("cache write did not commit")
        return request(method, path, *args, **kwargs)
    authority.request = unavailable
    asyncio.run(recovery.remember(identity, "native"))
    assert authority.cache is None
    assert task == before


def test_ownership_monitor_does_not_cancel_its_current_started_operation(adapter, native_channel, monkeypatch):
    recovery, authority, task, _ = adapter
    monkeypatch.setattr(native_channel, "TASK_OWNERSHIP_POLL_S", 0.005)
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None)
    channel._task_execution = recovery
    recovery.remember = AsyncMock()
    identity = {"task_id": "T", "team_id": "team", **task["execution"]}
    calls = []
    async def stream(self, payload):
        task["execution"]["state"] = "needs_verification"
        task["execution"]["checkpoint"]["operation"] = {"id": "current-tool", "state": "started"}
        # A durable started checkpoint exists while the same admitted native
        # tool runs. It is not evidence that this process lost execution.
        await asyncio.sleep(0.04)
        calls.append("effect verified")
        task["execution"]["checkpoint"]["operation"]["state"] = "verified"
        task["execution"]["state"] = "active"
        yield "completed"
    monkeypatch.setattr(native_channel.BaseChannel, "_stream_with_tracker", stream)
    async def scenario():
        async for _ in channel._stream_recovered_task({"meta": {
                "task_execution": identity, "task_session_id": "native"}}):
            pass
    asyncio.run(scenario())
    assert calls == ["effect verified"]
    recovery.remember.assert_awaited_once()


@pytest.mark.parametrize("changed", ["unknown", "generation", "instance_id", "process_id", "runtime", "cancelled"])
def test_started_operation_monitor_still_rejects_uncertainty_or_changed_authority(adapter, changed):
    recovery, authority, task, _ = adapter
    identity = {"task_id": "T", "team_id": "team", **task["execution"]}
    task["execution"]["state"] = "needs_verification"
    task["execution"]["checkpoint"]["operation"] = {"id": "inflight", "state": "started"}
    recovery.verify(identity, monitoring=True)
    with pytest.raises(RecoveryConflict):
        recovery.verify(identity)  # No new inference admission.
    with pytest.raises(RecoveryConflict):
        recovery.verify(identity, finished=True)  # No cache/final permission.
    if changed == "unknown":
        task["execution"]["checkpoint"]["operation"]["state"] = "unknown"
    elif changed == "cancelled":
        task["status"] = "cancelled"
    else:
        task["execution"][changed] = 3 if changed == "generation" else "successor"
    with pytest.raises(RecoveryConflict):
        recovery.verify(identity, monitoring=True)


def test_nio_sync_retries_from_authority_and_dispatches_after_ordered_commit(native_channel):
    from urllib.parse import parse_qs, urlsplit
    import nio
    from qwenpaw_matrix_sync import ManagedNioSync
    from worker_matrix_state import WorkerMatrix, matrix_event_key

    worker, room = "@worker:hs", "!room:hs"
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None, matrix_user_id=worker)
    channel._user_id = worker
    client = nio.AsyncClient("https://hs", worker, "DEVICE")
    client.user_id, client.access_token = worker, "token"
    client.next_batch = client.loaded_sync_token = "stale-private"
    channel._client = client
    channel._is_dm_room = AsyncMock(return_value=True)
    channel._announce_session_receipts = AsyncMock()
    channel._send_typing = AsyncMock()
    channel.access_control_dm = True
    channel._get_acl_store = lambda: SimpleNamespace(is_whitelisted=lambda _channel, sender: sender == "@human:hs")
    channel._access_control_gate = AsyncMock(return_value=True)
    channel._e2ee_maintenance = AsyncMock()
    channel._mark_channel_ready = lambda: None
    order, queries, commits = [], [], []
    accepted = {}
    matrix = WorkerMatrix("qwenpaw", recovery=SimpleNamespace(process="process"))
    state = {"runtime": "qwenpaw", "instance_id": "instance", "state": {
        "active_start": {"instance_id": "instance", "process_id": "process", "runtime": "qwenpaw"},
        "consumers": {"another-device": {"account": worker, "device": "other", "format": "matrix-nio/sync-v3",
                                         "runtime": "qwenpaw", "token": "foreign"}},
        "rooms": {}, "pending": []}}
    matrix.load = lambda: copy.deepcopy(state)
    matrix.input = accepted.get
    def ingest(snapshot, consumer, inputs, rooms):
        commits.append([entry["event_id"] for entry in inputs])
        if len(commits) == 1:
            raise RecoveryConflict("batch CAS failed")
        state["state"]["consumers"]["own"] = dict(consumer)
        state["state"]["rooms"].update(rooms)
        for entry in inputs:
            key = matrix_event_key(room, entry["event_id"])
            if key not in accepted:
                state["state"]["pending"].append(key)
            accepted[key] = entry
        order.append("commit")
        return copy.deepcopy(state)
    matrix.ingest = ingest
    channel._task_execution = SimpleNamespace(matrix=matrix, lock=asyncio.Lock(), supplement=AsyncMock(return_value=False))
    channel._enqueue = lambda payload: order.append(payload["meta"]["event_id"])
    runner = ManagedNioSync(channel, channel._task_execution)
    def event(event_id, body):
        return {"type": "m.room.message", "sender": "@human:hs", "event_id": event_id,
                "origin_server_ts": 1, "content": {"msgtype": "m.text", "body": body}}
    async def send(response_type, method, path, **kwargs):
        queries.append(parse_qs(urlsplit(path).query))
        events = [event("$z", "first"), event("$ignored", "NO_REPLY"), event("$a", "second")]
        blocked = event("$denied", "denied sender")
        blocked["sender"] = "@unapproved:hs"
        events.append(blocked)
        response = nio.SyncResponse.from_dict({"next_batch": "n1", "rooms": {"join": {room: {
            "timeline": {"events": events, "limited": False, "prev_batch": "p"}}}}})
        await client.receive_response(response)
        return response
    client._send = send
    async def scenario():
        with pytest.raises(RecoveryConflict, match="batch CAS"):
            await runner.poll()
        assert client.next_batch == "n1" and order == []
        channel._announce_session_receipts.assert_not_awaited()
        channel._send_typing.assert_not_awaited()
        channel._access_control_gate.assert_not_awaited()
        assert await runner.poll()
        assert order == ["commit", "$z", "$a"]
        channel._access_control_gate.assert_awaited_once()
        assert commits == [["$z", "$a"], ["$z", "$a"]]
        assert "since" not in queries[0] and "since" not in queries[1]
        assert await runner.poll()
        assert queries[-1]["since"] == ["n1"]
        assert order.count("$z") == order.count("$a") == 1
        # The replacement SDK has no in-memory rooms. A quiet full-state
        # response can have exactly the same token as the previous process.
        runner.ready = False
        client.rooms.clear()
        assert await runner.poll()
        assert room in client.rooms
        assert order.count("$z") == order.count("$a") == 1
        await client.close()
    asyncio.run(scenario())


def test_nio_history_page_uses_immutable_anchor_and_native_policy(native_channel):
    import nio
    from qwenpaw_matrix_sync import ManagedNioSync
    from worker_matrix_state import WorkerMatrix, matrix_event_key

    room, worker = "!room:hs", "@worker:hs"
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None, matrix_user_id=worker)
    channel._user_id = worker
    client = nio.AsyncClient("https://hs", worker, "DEVICE")
    channel._client = client
    client.rooms[room] = nio.MatrixRoom(room, worker)
    matrix = WorkerMatrix("qwenpaw", recovery=SimpleNamespace())
    channel._task_execution = SimpleNamespace(matrix=matrix, lock=asyncio.Lock(), supplement=AsyncMock(return_value=False))
    runner = ManagedNioSync(channel, channel._task_execution)
    channel._is_dm_room = AsyncMock(return_value=True)
    channel._announce_session_receipts = AsyncMock()
    channel._send_typing = AsyncMock()
    queued = []
    channel._enqueue = lambda payload: queued.append(payload["meta"]["event_id"])
    def event(event_id, body):
        return nio.Event.parse_event({"type": "m.room.message", "sender": worker if event_id == "$anchor" else "@human:hs",
            "event_id": event_id, "origin_server_ts": 1, "content": {"msgtype": "m.text", "body": body}})
    anchor = event("$anchor", "already accepted by the previous Harness")
    older, newer = event("$older", "older"), event("$later", "later")
    anchor_key = matrix_event_key(room, anchor.event_id)
    matrix.input = lambda key: {"intent": "context"} if key == anchor_key else None
    client.room_messages = AsyncMock(return_value=nio.RoomMessagesResponse(
        room, [newer, event("$ignore", "NO_REPLY"), older, anchor, event("$outside", "outside boundary")], "page", "next"))
    async def scenario():
        events, token = await runner.fetch_page(room, "page")
        client.room_messages.assert_awaited_once_with(room, start="page")
        assert token == "next"
        prepared = await runner.inputs(room, events, "$anchor")
        assert [(item["event_id"], item["intent"]) for item in prepared] == [
            ("$later", "message"), ("$older", "message"), ("$anchor", "context")]
        channel._announce_session_receipts.assert_not_awaited()
        channel._send_typing.assert_not_awaited()
        assert not queued
        snapshot = {"state": {"rooms": {room: {"gaps": ["page"]}},
                              "pending": [anchor_key, matrix_event_key(room, "$older"), matrix_event_key(room, "$later")]}}
        await runner.dispatch(snapshot)
        assert not queued
        snapshot["state"]["rooms"][room]["gaps"] = []
        await runner.dispatch(snapshot)
        assert queued == ["$older", "$later"]
        encrypted = nio.Event.parse_event({"type": "m.room.encrypted", "sender": "@human:hs",
            "event_id": "$encrypted", "origin_server_ts": 2, "content": {"algorithm": "m.megolm.v1.aes-sha2",
            "sender_key": "key", "session_id": "id", "device_id": "D", "ciphertext": "no-key"}})
        with pytest.raises(RecoveryConflict, match="undecrypted"):
            await runner.inputs(room, [encrypted])
        await client.close()
    asyncio.run(scenario())


def test_pending_ordinary_message_rebuilds_then_retries_only_saved_reply(native_channel, tmp_path, monkeypatch):
    import nio
    from qwenpaw.app.chats.session import SafeJSONSession
    from qwenpaw_matrix_sync import ManagedNioSync
    from qwenpaw_message_execution import MessageExecution
    from worker_matrix_state import matrix_event_key

    monkeypatch.setattr("task_coordination.controller_json", lambda _path: {})

    worker, room, event_id = "@worker:hs", "!room:hs", "$original"
    key = matrix_event_key(room, event_id)
    identity = {"event_key": key, "process_id": "process", "instance_id": "instance", "generation": 2}
    record = {"room_id": room, "event_id": event_id, "intent": "message", "input": {"sender": "@human:hs",
              "type": "m.room.message", "origin_server_ts": 1, "content": {"msgtype": "m.text", "body": "Continue."}}}
    state = {"mode": "resume", "checkpoint": {"version": 1, "completed_steps": ["step one"]}}
    order = []
    def claim(_key):
        assert _key == key
        return {"execution": {**identity, "checkpoint": state["checkpoint"], "runtime": "qwenpaw", "state": "active"}, "input": record,
                "identity": identity, "checkpoint_digest": Recovery.digest(state["checkpoint"]),
                "recovery": state["mode"], "resume_text": "step one already done. Continue step two.",
                "saved_result": "saved second step"}
    def store(actual, text):
        assert actual == identity and text == "saved second step" and state["mode"] == "resume"
        order.append("save")
        state.update(mode="deliver", checkpoint={"version": 2, **pending_reply(text, "stable")})
        return claim(key)
    def bind(actual, cp):
        assert actual == identity
        assert cp["reply_attempt"] == {"state": "sending", "user_id": worker, "device_id": "D"}
        order.append("bind")
        state["checkpoint"] = cp
        return claim(key)
    def delivered(actual, event):
        assert actual == identity and event == "$reply"
        order.append("ack")
        state.update(mode="done", checkpoint={"version": 3, "reply_event": event})
        return claim(key)
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None, matrix_user_id=worker)
    channel._user_id = worker
    client = nio.AsyncClient("https://hs", worker, "D")
    client.whoami = AsyncMock(return_value=nio.WhoamiResponse(worker, "D", False))
    channel._client = client
    client.rooms[room] = nio.MatrixRoom(room, worker)
    def event(identifier, body):
        return nio.Event.parse_event({"type": "m.room.message", "sender": "@human:hs", "event_id": identifier,
              "origin_server_ts": 1, "content": {"msgtype": "m.text", "body": body}})
    previous = event("$accepted-context", "existing workspace discussion")
    client.room_context = AsyncMock(return_value=nio.RoomContextResponse(room, "p", "n",
        event(event_id, "Continue."), [event("$never-accepted", "unrelated instruction"), previous], [], []))
    previous_key = matrix_event_key(room, previous.event_id)
    channel._workspace = SimpleNamespace(session=SafeJSONSession(str(tmp_path / "sessions")), task_tracker=SimpleNamespace())
    channel._is_dm_room = AsyncMock(return_value=True)
    channel._announce_session_receipts = AsyncMock()
    channel._send_typing = AsyncMock()
    channel._publish_direct_execution = AsyncMock()
    async def send(_room, _type, content, *, tx_id):
        assert (_room, _type, content["body"], tx_id) == (room, "m.room.message", "saved second step", "stable")
        assert content["m.relates_to"]["m.in_reply_to"]["event_id"] == event_id
        assert content["org.agentteams.reply_txn"] == tx_id
        order.append("send")
        if order.count("send") == 1:
            raise TimeoutError("Matrix may already have accepted this transaction")
        return SimpleNamespace(event_id="$reply")
    channel._room_send_with_retry = send
    stored = {key: record, previous_key: {"intent": "context", "input": previous.source}}
    matrix = SimpleNamespace(input=stored.get, message=lambda _key: claim(_key),
        message_claim=claim, message_current=lambda actual: claim(actual["event_key"]),
        message_save_reply=store, message_checkpoint=bind, message_delivered=delivered)
    sessions = SimpleNamespace(load=lambda work: {"generation": 2, "checkpoint_digest": Recovery.digest(state["checkpoint"]),
        "cache": {"native_ref": "matrix:message:" + key + ":lost"}})
    execution = SimpleNamespace(matrix=matrix, sessions=sessions, lock=asyncio.Lock(),
                                supplement=AsyncMock(return_value=False))
    execution.messages = MessageExecution(channel, execution)
    channel._task_execution = execution
    runner = ManagedNioSync(channel, execution)
    queued = []
    channel._enqueue = queued.append
    async def native_stream(_self, payload):
        req = channel.build_agent_request_from_native(payload)
        order.append("inference")
        await channel._on_process_completed(req, room, {**req.channel_meta,
            native_channel._MATRIX_STREAMING_FINAL_TEXT_KEY: "saved second step"})
        yield "done"
    monkeypatch.setattr(native_channel.BaseChannel, "_stream_with_tracker", native_stream)
    snapshot = {"state": {"pending": [key], "rooms": {room: {}}}}
    async def scenario():
        # No typed event, live callback or new Matrix message.
        await runner.dispatch(snapshot)
        await runner.dispatch(snapshot)
        assert len(queued) == 1
        payload = queued.pop()
        req = channel.build_agent_request_from_native(payload)
        assert req.session_id.startswith("matrix:message:" + key + ":") and not req.session_id.endswith(":lost")
        assert req.channel_meta["message_execution"] == identity
        assert "step one already done" in payload["content_parts"][-1].text
        assert "existing workspace discussion" in payload["content_parts"][0].text
        assert "unrelated instruction" not in payload["content_parts"][0].text
        with pytest.raises(TimeoutError):
            async for _ in channel._stream_with_tracker(payload):
                pass
        assert order == ["inference", "save", "bind", "send"]
        assert not execution.messages.turns
        # A replacement with an empty local session store sees only pending delivery.
        await ManagedNioSync(channel, execution).dispatch(snapshot)
        await runner.dispatch(snapshot)
        assert order == ["inference", "save", "bind", "send", "send", "ack"] and not queued
        state["mode"] = "verify"
        await runner.dispatch(snapshot)
        assert not queued
        await client.close()
    asyncio.run(scenario())


@pytest.mark.parametrize("outcome", ["success", "timeout", "late_timeout", "error"])
def test_qwenpaw_native_tool_hook_holds_unknown_effects(native_channel, outcome):
    from agentscope.agent import Agent
    from agentscope.message import TextBlock, ToolCallBlock
    from agentscope.tool import ToolResponse
    path = Path(native_channel.__file__).with_name("session_receipts.py")
    spec = importlib.util.spec_from_file_location("_native_message_hook", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    order = []
    identity = {"event_key": "key", "generation": 2, "instance_id": "i", "process_id": "p"}
    call = ToolCallBlock(id="call-one", name="write_artifact", input='{"path":"output"}')
    def begin(actual, calls):
        assert actual == identity and calls == [{"id": "call-one", "name": "write_artifact", "arguments": {"path": "output"}}]
        order.append("started")
        return "operation"
    def finish(actual, operation, results):
        assert actual == identity and operation == "operation" and results[0]["content"][0]["text"] == "saved"
        order.append("verified")
        return {"checkpoint_digest": "v2"}
    turn = {"identity": identity, "lock": asyncio.Lock()}
    matrix = SimpleNamespace(message_begin_tools=begin, message_finish_tools=finish)
    messages = SimpleNamespace(matrix=matrix, turns={"key": turn})
    channel = SimpleNamespace(_task_execution=SimpleNamespace(messages=messages))
    ctx = SimpleNamespace(extras={}, request=SimpleNamespace(channel_meta={"message_execution": identity}),
        workspace=SimpleNamespace(channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=channel))))
    async def invoke(_call):
        assert _call is call
        order.append("effect")
        if outcome == "timeout":
            raise TimeoutError("outcome unknown")
        yield ToolResponse(state="success" if outcome == "late_timeout" else outcome, content=[TextBlock(text="saved")])
        if outcome == "late_timeout":
            raise TimeoutError("tool stream disconnected during finalization")
    agent = SimpleNamespace(_acting_middlewares=[module.SessionReceiptMiddleware(ctx)], _acting_impl=invoke,
                            toolkit=SimpleNamespace(get_tool=AsyncMock(return_value=None)))
    async def scenario():
        if outcome == "success":
            results = [r async for r in Agent._acting(agent, call)]
            assert len(results) == 1 and turn["checkpoint_digest"] == "v2"
            assert order == ["started", "effect", "verified"]
        else:
            with pytest.raises(TimeoutError if "timeout" in outcome else RecoveryConflict):
                async for _ in Agent._acting(agent, call):
                    pass
            assert order == ["started", "effect"]
        messages.turns.clear()
        with pytest.raises(RecoveryConflict, match="replaced"):
            async for _ in Agent._acting(agent, call):
                pass
    asyncio.run(scenario())


def test_ordinary_message_cache_requires_native_bytes_and_latest_shared_version(native_channel, tmp_path):
    from qwenpaw.app.chats.session import SafeJSONSession
    from qwenpaw_message_execution import MessageExecution
    from worker_sessions import WorkerSessions

    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None)
    store = SafeJSONSession(str(tmp_path / "sessions"))
    channel._workspace = SimpleNamespace(session=store, task_tracker=SimpleNamespace())
    key, room = "a" * 64, "!room:hs"
    session = "matrix:message:" + key + ":original"
    identity = {"event_key": key, "process_id": "process", "instance_id": "instance", "generation": 2}
    view = {"generation": 2, "checkpoint_version": 1, "checkpoint_digest": "digest",
            "config_generation": 3, "runtime": "qwenpaw", "compatible": True}
    claim = {"identity": identity, "input": {"room_id": room}, "execution": {"checkpoint": {}},
             "recovery": "resume", "checkpoint_digest": "digest", "resume_text": "continue"}
    matrix = SimpleNamespace(message=lambda _: claim, message_claim=lambda _: claim, message_current=lambda _: claim)
    sessions = WorkerSessions("qwenpaw", recovery=SimpleNamespace())
    sessions.load = lambda _: view
    messages = MessageExecution(channel, SimpleNamespace(matrix=matrix, sessions=sessions))
    async def scenario():
        await store.save_session_state(session_id=session, user_id=room, channel=channel.channel,
                                       agent=SimpleNamespace(state_dict=lambda: {"native": "saved"}))
        saved = await messages.native_state(session, room)
        cache = {**view, "runtime": "qwenpaw", "native_ref": session, "native_digest": Recovery.digest(saved),
                 "expires_at": 9999999999}
        for changes, reused in [({}, True), ({"expires_at": 1}, False), ({"checkpoint_version": 0}, False),
                                ({"config_generation": 1}, False), ({"native_digest": "changed"}, False)]:
            view["cache"] = {**cache, **changes}
            turn = await messages.admit(key)
            assert (turn["session_id"] == session) is reused
            assert turn["rebuild"] is not reused
    asyncio.run(scenario())


@pytest.mark.parametrize("phase", ["closed", "active", "active_started", "active_unknown", "stale", "lost_descriptor_ack"])
def test_late_assignment_is_observed_in_existing_task_session(adapter, native_channel, tmp_path, monkeypatch, phase):
    import nio
    from unittest.mock import Mock
    from agentscope.agent import Agent
    from agentscope.message import UserMsg
    from qwenpaw.app.chats.session import SafeJSONSession
    from qwenpaw_matrix_sync import ManagedNioSync
    from worker_matrix_state import matrix_event_key

    execution, authority, task, _ = adapter
    channel = native_channel.AgentTeamsMatrixChannel(process=lambda _: None, matrix_user_id="@worker:hs")
    store = SafeJSONSession(str(tmp_path / "sdk"))
    channel._workspace = SimpleNamespace(session=store, task_tracker=SimpleNamespace())
    channel._task_execution = execution = type(execution)(channel, authority)
    room, event_id = task["room_id"], "$late-assignment"
    key = matrix_event_key(room, event_id)
    source = {"type": "m.room.message", "sender": "@lead:hs", "origin_server_ts": 42,
              "content": {"msgtype": "m.text", "body": "Assigned Task T", "org.agentteams.coordination": {"id": "receipt"}}}
    work = {"kind": "task", "team_id": "team", "task_id": "T"}
    record = {"intent": "task_notification", "work": work, "input": source, "room_id": room, "event_id": event_id}
    identity = {"team_id": "team", "task_id": "T", **task["execution"]}
    admission = ("team", "T", identity["generation"])
    session = "matrix:task:" + execution.key("team", "T") + ":original"
    address = {"session_id": session, "user_id": room, "channel": channel.channel}
    client = channel._client = nio.AsyncClient("https://hs", "@worker:hs", "D")
    client.rooms[room] = nio.MatrixRoom(room, "@worker:hs")
    channel._session_receipts = SimpleNamespace(submit=AsyncMock(), was_completed=AsyncMock(return_value=False))
    channel.enqueue_recovered_task = AsyncMock()
    authority.claim = Mock(side_effect=AssertionError("Existing Task must not be claimed or executed again"))
    execution.admitted.add(admission)
    execution.matrix.input = Mock(return_value=record)
    execution.matrix.native_input = Mock(return_value={"input": record})
    execution.matrix.native_current = Mock(return_value=False)
    order = []
    def commit(source_key, _, native_ref, native_digest):
        assert source_key == key and native_ref == session
        assert phase.startswith("active") or authority.cache["native_digest"] == native_digest
        record["native_commit"] = {"native_ref": native_ref, "native_digest": native_digest,
                                   "start": {"process_id": authority.process}}
        order.append("native fact")
    execution.matrix.native_committed = Mock(side_effect=commit)
    channel._session_receipts.submit.side_effect = lambda *_: order.append("read")
    canonical = {"text": "Canonical assigned Task T", "meta": {"teamharness_coordination": {"kind": "task_assigned", "task_id": "T"}}}
    def notification(*_args, event_id):
        assert event_id == record["event_id"]
        return canonical
    monkeypatch.setattr("task_coordination.notification", notification)
    channel._user_id = "@worker:hs"
    runner = ManagedNioSync(channel, execution)
    snapshot = {"state": {"pending": [key], "rooms": {room: {}}}}

    async def run():
        # Persist a real AgentScope state through the same SDK store that runs the turn.
        model = Mock()
        agent = Agent("worker", "Execute the Task", model)
        await agent.observe(UserMsg("worker", "Completed step one"))
        agent.state_dict = lambda: {"state": agent.state.model_dump(mode="json")}
        await store.save_session_state(**address, agent=agent)
        original = await store.get_session_state_dict(**address)
        view = execution.sessions.load(work)
        execution.sessions.remember(work, view, identity, session, Recovery.digest(original), int(time.time()) + 1800)
        if phase.startswith("active"):
            execution.native_active.add(admission)
            if phase != "active":
                task["execution"]["state"] = "needs_verification"
                task["execution"]["checkpoint"]["operation"] = {
                    "id": "op-before-late-assignment", "state": "started" if phase == "active_started" else "unknown"}
                # A new execution still cannot enter on an unresolved effect.
                with pytest.raises(RecoveryConflict):
                    execution.verify(identity)
        elif phase == "stale":
            authority.cache["native_digest"] = "wrong"
        elif phase == "lost_descriptor_ack":
            remember = execution.sessions.remember
            writes = []
            def reject_first(*args):
                writes.append(True)
                if len(writes) == 1:
                    raise RecoveryConflict("descriptor CAS unavailable")
                return remember(*args)
            execution.sessions.remember = reject_first
        await runner.dispatch(snapshot)  # empty process-local waiting cache, no new Matrix event
        if phase.startswith("active"):
            assert not order
            ctx = SimpleNamespace(session_id=session, request=SimpleNamespace(channel_meta={"task_execution": identity}), mode_state={})
            if phase == "active_unknown":
                with pytest.raises(RecoveryConflict):
                    await execution.assignments.before_reasoning(ctx, agent)
                assert not order
                assert await store.get_session_state_dict(**address) == original
                channel.enqueue_recovered_task.assert_not_awaited()
                authority.claim.assert_not_called()
                model.assert_not_called()
                await client.close()
                return
            await execution.assignments.before_reasoning(ctx, agent)
        elif phase == "lost_descriptor_ack":
            assert not order
            await runner.dispatch(snapshot)
        if phase == "stale":
            assert not order
            assert await store.get_session_state_dict(**address) == original
        else:
            assert order == ["native fact", "read"]
            latest = await store.get_session_state_dict(**address)
            msgs = latest["agent"]["state"]["context"]
            observed = [m for m in msgs if m["id"] == "matrix:" + key]
            assert len(observed) == 1 and observed[0]["metadata"]["matrix_source"] == source
            assert observed[0]["content"][0]["text"] == canonical["text"]
            assert record["native_commit"]["native_digest"] == Recovery.digest(latest)
            assert msgs[0] == original["agent"]["state"]["context"][0]
            await runner.dispatch(snapshot)
            execution.matrix.native_committed.assert_called_once()
            channel._session_receipts.submit.assert_awaited_once_with(room, [event_id])
        channel.enqueue_recovered_task.assert_not_awaited()
        authority.claim.assert_not_called()
        assert task["execution"]["checkpoint"]["completed_steps"] == ["one"]
        model.assert_not_called()
        await client.close()
    asyncio.run(run())


@pytest.mark.parametrize("save_fails", [False, True])
def test_qwenpaw_verified_input_refresh_is_saved_before_read_and_inference(native_channel, save_fails):
    path = Path(native_channel.__file__).with_name("session_receipts.py")
    spec = importlib.util.spec_from_file_location("_refreshed_human_checkpoint", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)

    async def scenario():
        order = []
        state = {"state": {"middle_context": {}, "context": [
            {"role": "user", "metadata": {"agentteams.event.id": "$new"}, "content": "Updated Human request"}]}}
        agent = SimpleNamespace(state=SimpleNamespace(middle_context=state["state"]["middle_context"]), state_dict=lambda: state)
        async def save(**kwargs):
            snapshot = kwargs["agent"].state_dict()
            assert snapshot["state"]["context"][0]["role"] == "user"
            assert snapshot["state"]["context"][0]["metadata"]["agentteams.event.id"] == "$new"
            order.append("save")
            if save_fails:
                raise OSError("synthetic unavailable storage")
        delivery = SimpleNamespace(submit=AsyncMock(side_effect=lambda *_: order.append("read")))
        channel = SimpleNamespace(_session_receipts=delivery)
        ctx = SimpleNamespace(extras={}, session_id="fixture", mode_state={},
            request=SimpleNamespace(channel="agentteams_matrix", user_id="!r", channel_meta={"room_id": "!r", "session_source_event_ids": ["$original"]}),
            workspace=SimpleNamespace(session=SimpleNamespace(save_session_state=save),
                channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=channel))))
        module.session_receipt_middleware(ctx, None)
        async def infer():
            await ctx.extras["agentteams_checkpoint_inputs"](agent, [{"roomId": "!r", "eventId": "$new"}])
            order.append("model")
        if save_fails:
            with pytest.raises(OSError, match="unavailable storage"):
                await infer()
            assert order == ["save"]
            delivery.submit.assert_not_awaited()
        else:
            await infer()
            assert order == ["save", "read", "model"]
            delivery.submit.assert_awaited_once_with("!r", ["$original", "$new"], state["state"]["middle_context"][module.PENDING_KEY])
    asyncio.run(scenario())
