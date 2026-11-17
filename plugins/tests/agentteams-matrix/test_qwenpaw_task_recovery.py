"""Startup recovery uses authority before native admission, without Matrix input."""
import asyncio
import copy
import importlib.util
import json
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "shared/lib"))
from task_recovery import Recovery, RecoveryConflict, RecoveryUnavailable


@pytest.fixture
def adapter(monkeypatch, tmp_path):
    path = ROOT / "plugins/agentteams-matrix-channel/agentteams_matrix/task_execution.py"
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
    task["execution"]["checkpoint"].update(delivery="pending", reply_txn="stable-transaction")
    claim = authority.claim
    authority.claim = lambda *args: {**claim(*args), "saved_result": "saved answer"}
    delivered = []
    authority.delivered = lambda *args: (delivered.append(args), copy.deepcopy(task))[1]
    asyncio.run(recovery.recover())
    channel.send_task_reply.assert_awaited_once_with("!task:hs", "saved answer", "stable-transaction")
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
    channel.send_task_reply = AsyncMock(return_value="$reply")
    recovery = type(recovery)(channel, authority)
    channel._task_execution = recovery
    cp = task["execution"]["checkpoint"]
    attempts = []
    def prepared(*args):
        task["status"] = "submitted"
        cp.update(result={"key": "saved-report"}, delivery="pending", reply_txn="stable-txn")
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
        channel.send_task_reply.assert_awaited_with("!task:hs", "saved answer", "stable-txn")
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
    cp.update(delivery='pending', reply_txn='saved-report')
    prepared = []
    authority.prepare_reply = lambda *args: (prepared.append(args), ({'execution': {'checkpoint': cp}, 'room_id': '!task:hs'}, 'authoritative report'))[1]
    authority.delivered = lambda *args: copy.deepcopy(task)
    async def scenario():
        if allowed:
            await recovery.finish({'task_id': 'T', 'team_id': 'team', 'generation': 2}, 'session', '')
            assert prepared == [('T', 'team', '')]
            channel.send_task_reply.assert_awaited_once_with('!task:hs', 'authoritative report', 'saved-report')
        else:
            with pytest.raises(RuntimeError, match='without a durable result'):
                await recovery.finish({'task_id': 'T', 'team_id': 'team', 'generation': 2}, 'session', '')
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


@pytest.fixture(params=["legacy", "bundle"])
def native_channel(monkeypatch, request):
    pytest.importorskip("qwenpaw.app.channels.base")
    path = ROOT / ("plugins/agentteams-matrix-channel/agentteams_matrix/channel.py" if request.param == "legacy"
                   else "plugins/agentteams-matrix/adapters/qwenpaw/agentteams_matrix/channel.py")
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
    channel.send_task_reply = AsyncMock(return_value="$reply")
    recovery = type(recovery)(channel, authority)
    channel._task_execution = recovery
    identity = {"task_id": "T", "team_id": "team", **task["execution"]}
    session = "matrix:task:" + recovery.key("team", "T") + ":native"
    task["execution"]["checkpoint"].update(delivery="pending", reply_txn="result")
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
        assert await channel.send_task_reply("!room", "saved result", "stable-txn") == "$persisted"
        channel._room_send_with_retry.assert_awaited_once_with("!room", "m.room.message", {
            "msgtype": "m.text", "body": "saved result", "org.agentteams.progress_only": True,
        }, tx_id="stable-txn")
        channel._room_send_with_retry.return_value = SimpleNamespace()
        with pytest.raises(RuntimeError, match="not acknowledged"):
            await channel.send_task_reply("!room", "saved result", "stable-txn")
    asyncio.run(scenario())


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
    task["execution"]["checkpoint"].update(delivery="pending", reply_txn="saved-txn")
    authority.prepare_reply = lambda *args: ({"task": task, "execution": task["execution"]}, "saved authoritative answer")
    delivered = []
    authority.delivered = lambda *args: (delivered.append(args), copy.deepcopy(task))[1]
    asyncio.run(recovery.finish({"task_id": "T", "team_id": "team", "generation": 2}, "native-session", "stale native answer"))
    channel.send_task_reply.assert_awaited_once_with("!task:hs", "saved authoritative answer", "saved-txn")
    assert delivered == [("T", "team", "$reply")]


@pytest.mark.parametrize("bundle", ["teamharness", "agentteams-teamharness"])
@pytest.mark.parametrize("state", ["active", "needs_verification"])
def test_recovered_native_reasoning_is_fenced_before_task_tools(adapter, monkeypatch, bundle, state):
    import task_recovery
    recovery, authority, task, channel = adapter
    task["execution"]["state"] = state
    path = ROOT / "plugins" / bundle / "adapters/qwenpaw/plugin.py"
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
