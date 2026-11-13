"""Direct-chat controls cancel the captured run before queuing its continuation."""

import asyncio
import importlib.util
import sys
import shlex
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import pytest

pytest.importorskip("nio")
pytest.importorskip("qwenpaw")

path = Path(__file__).resolve().parents[3] / "plugins/agentteams-matrix-channel/agentteams_matrix/channel.py"
spec = importlib.util.spec_from_file_location("direct_control_channel", path)
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)

ROOM, HUMAN, AGENT = "!direct:test", "@human:test", "@agent:test"
CONTROL, EXECUTION = "com.agentteams.control", "com.agentteams.execution"


def channel():
    ch = module.AgentTeamsMatrixChannel(process=lambda *_: None)
    ch._user_id = AGENT
    ch._client = SimpleNamespace(joined_members=AsyncMock(return_value=module.JoinedMembersResponse(
        [SimpleNamespace(user_id=AGENT), SimpleNamespace(user_id=HUMAN)], ROOM,
    )))
    ch._room_send_with_retry = AsyncMock()
    ch._send_plain_text = AsyncMock()
    ch._access_control_gate = AsyncMock(return_value=False)
    ch._on_consume_error = AsyncMock()
    ch._send_typing = AsyncMock()
    return ch


def payload(body="补充：只看杭州"):
    return {"sender_id": HUMAN, "content_parts": [module.TextContent(type="text", text=body)],
            "meta": {"room_id": ROOM, "is_dm": True, "sender_id": HUMAN, "event_id": "$next"}}


def event(action=None, run_id="$run"):
    content = {} if action is None else {CONTROL: {"action": action, "run_id": run_id, "agent_id": AGENT}}
    return SimpleNamespace(source={"content": content})


def test_supplement_waits_for_tool_cleanup_and_keeps_original_input():
    async def scenario():
        ch = channel()
        started, cleaned = asyncio.Event(), asyncio.Event()

        async def stream(_self, item):
            assert item["meta"]["event_id"] == "$run"
            started.set()
            try:
                await asyncio.Event().wait()
            finally:
                # A native runtime may persist state asynchronously on cancellation.
                await asyncio.sleep(0)
                cleaned.set()
            yield "unused"

        original = payload("查全国数据")
        original["meta"]["event_id"] = "$run"
        ch._active_thread_roots[ROOM] = "$partial-response"
        with patch.object(module.BaseChannel, "_stream_with_tracker", stream):
            async def consume():
                async for _ in ch._stream_with_tracker(original):
                    pass
            task = asyncio.create_task(consume())
            await started.wait()
            supplementary = payload()
            handled = await ch._handle_direct_control(supplementary, event("steer"))
            assert handled is False  # Caller queues the retained supplementary payload.
            assert cleaned.is_set() and task.done()
            assert original["content_parts"][0].text == "查全国数据"
            assert [part.text for part in supplementary["meta"]["direct_inputs"]] == ["查全国数据", "补充：只看杭州"]
            assert supplementary["meta"]["session_source_event_ids"] == ["$run", "$next"]
        states = [call.args[2][EXECUTION]["status"] for call in ch._room_send_with_retry.await_args_list]
        assert states == ["running", "stopping", "stopped"]
        assert ch._room_send_with_retry.await_args_list[-1].args[2][EXECUTION]["message_id"] == "$partial-response"
        ch._on_consume_error.assert_not_awaited()

    asyncio.run(scenario())


def test_overlapping_controls_do_not_cancel_native_cleanup_twice():
    async def scenario():
        ch = channel()
        started, cleaning, release, cleaned, both_requested = [asyncio.Event() for _ in range(5)]
        requests = 0

        async def send(_room, _type, content):
            nonlocal requests
            if content[EXECUTION]["status"] == "stopping":
                requests += 1
                if requests == 2:
                    both_requested.set()
        ch._room_send_with_retry.side_effect = send

        async def stream(_self, _item):
            started.set()
            try:
                await asyncio.Event().wait()
            finally:
                cleaning.set()
                await release.wait()
                cleaned.set()
            yield "unused"

        original = payload("原任务")
        original["meta"]["event_id"] = "$run"
        with patch.object(module.BaseChannel, "_stream_with_tracker", stream):
            async def consume():
                async for _ in ch._stream_with_tracker(original):
                    pass
            producer = asyncio.create_task(consume())
            await started.wait()
            first = asyncio.create_task(ch._handle_direct_control(payload("补充一"), event("steer")))
            await cleaning.wait()
            second = asyncio.create_task(ch._handle_direct_control(payload("补充二"), event("steer")))
            await asyncio.wait_for(both_requested.wait(), 1)
            assert producer.cancelling() == 1 and not producer.done()
            release.set()
            assert await asyncio.gather(first, second) == [False, False]
            assert cleaned.is_set() and producer.done()
    asyncio.run(scenario())


@pytest.mark.parametrize("action", ["stop", "steer"])
def test_stale_control_does_not_cancel_new_run(action):
    async def scenario():
        ch = channel()
        task = asyncio.create_task(asyncio.Event().wait())
        ch._direct_runs[ROOM] = ("$new-run", HUMAN, task, [])
        assert await ch._handle_direct_control(payload(), event(action)) is (action == "stop")
        assert not task.cancelling()
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
    asyncio.run(scenario())


@pytest.mark.parametrize("denied,group", [(True, False), (False, True)])
def test_acl_and_current_membership_checked_before_cancellation(denied, group):
    async def scenario():
        ch = channel()
        ch._access_control_gate.return_value = denied
        if group:
            ch._client.joined_members.return_value.members.append(SimpleNamespace(user_id="@other:test"))
        task = asyncio.create_task(asyncio.Event().wait())
        ch._direct_runs[ROOM] = ("$run", HUMAN, task, [])
        assert await ch._handle_direct_control(payload(), event("stop")) is True
        assert not task.cancelling()
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
    asyncio.run(scenario())


def test_stop_cancels_only_captured_room_and_does_not_queue_a_prompt():
    async def scenario():
        ch = channel()
        task = asyncio.create_task(asyncio.Event().wait())
        other = asyncio.create_task(asyncio.Event().wait())
        ch._direct_runs[ROOM] = ("$run", HUMAN, task, [])
        ch._direct_runs["!other:test"] = ("$other", HUMAN, other, [])
        assert await ch._handle_direct_control(payload("停止执行"), event("stop")) is True
        assert task.done() and not other.cancelling()
        other.cancel()
        await asyncio.gather(other, return_exceptions=True)
    asyncio.run(scenario())


def test_group_messages_and_slash_commands_keep_existing_queue_behavior():
    async def scenario():
        ch = channel()
        item = payload("/status")
        assert await ch._handle_direct_control(item, event()) is False
        item["meta"]["is_dm"] = False
        assert await ch._handle_direct_control(item, event()) is False
        ch._access_control_gate.assert_not_awaited()
    asyncio.run(scenario())


def test_unmarked_input_does_not_interrupt_an_active_run():
    async def scenario():
        ch = channel()
        task = asyncio.create_task(asyncio.Event().wait())
        ch._direct_runs[ROOM] = ("$run", HUMAN, task, [])
        assert not await ch._handle_direct_control(payload(), event())
        assert not task.cancelling()
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
    asyncio.run(scenario())


def test_reload_channels_share_the_captured_run_registry():
    first, replacement = channel(), channel()
    workspace = SimpleNamespace(task_tracker=SimpleNamespace())
    first.set_workspace(workspace)
    replacement.set_workspace(workspace)
    assert first._direct_runs is replacement._direct_runs


def test_consecutive_supplements_retain_each_input_once():
    async def scenario():
        ch = channel()
        entered = asyncio.Queue()

        async def stream(_self, item):
            entered.put_nowait(item)
            await asyncio.Event().wait()
            yield "unused"

        async def consume(item):
            async for _ in ch._stream_with_tracker(item):
                pass

        with patch.object(module.BaseChannel, "_stream_with_tracker", stream):
            original = payload("查全国数据")
            original["meta"]["event_id"] = "$run"
            asyncio.create_task(consume(original))
            await entered.get()
            first = payload("只看杭州")
            assert not await ch._handle_direct_control(first, event("steer"))
            asyncio.create_task(consume(first))
            await entered.get()
            second = payload("排除测试账号")
            second["meta"]["event_id"] = "$last"
            assert not await ch._handle_direct_control(second, event("steer", "$next"))
            assert [part.text for part in second["meta"]["direct_inputs"]] == ["查全国数据", "只看杭州", "排除测试账号"]
            assert second["meta"]["session_source_event_ids"] == ["$run", "$next", "$last"]

    asyncio.run(scenario())


def test_native_tracker_and_shell_stop_the_process_before_continuation(tmp_path):
    from qwenpaw.app.task_tracker import TaskTracker
    from qwenpaw.agents.tools.shell import execute_shell_command

    async def scenario():
        ch = channel()
        tracker = TaskTracker()
        pid_file = tmp_path / "tool.pid"
        received = []

        async def process(request):
            received.append(request)
            if len(received) == 1:
                script = f"import os,time;from pathlib import Path;Path({str(pid_file)!r}).write_text(str(os.getpid()));time.sleep(60)"
                await execute_shell_command(f"{shlex.quote(sys.executable)} -c {shlex.quote(script)}", timeout=60)
                # The native Runtime also checks cancellation before advancing.
                if asyncio.current_task().cancelling():
                    raise asyncio.CancelledError
                pytest.fail("Old execution advanced after interruption")
            if False:
                yield None

        ch._process = process
        ch._before_consume_process = AsyncMock()
        ch._on_process_completed = AsyncMock()
        ch._finish_response_cycle = AsyncMock()
        original = payload("查全国数据")
        original["meta"]["event_id"] = "$run"
        _, started = await tracker.attach_or_start("chat", original, ch._stream_with_tracker)
        assert started
        for _ in range(200):
            if pid_file.exists():
                break
            await asyncio.sleep(0.01)
        assert pid_file.exists(), "Native shell did not start"
        child_pid = int(pid_file.read_text())
        next_input = payload()
        assert await ch._handle_direct_control(next_input, event("steer")) is False
        # Container PID 1 may retain an orphaned zombie after process-group
        # termination; it has exited and cannot perform further operations.
        proc_stat = Path(f"/proc/{child_pid}/stat")
        assert not proc_stat.exists() or proc_stat.read_text().split()[2] == "Z"
        assert await tracker.get_status("chat") != "running"
        _, started = await tracker.attach_or_start("chat", next_input, ch._stream_with_tracker)
        assert started, "Continuation attached to the cancelled producer"
        assert await tracker.wait_all_done(timeout=2)
        assert len(received) == 2
        combined = " ".join(part.text for part in received[1].input[0].content)
        assert "查全国数据" in combined and "只看杭州" in combined

    asyncio.run(asyncio.wait_for(scenario(), timeout=8))
