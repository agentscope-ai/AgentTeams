"""Exercise installed CronExecutor delivery against the actual Matrix channel."""
import asyncio
import importlib.util
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch

from qwenpaw.app.crons import executor
from qwenpaw.schemas import MessageType, RunStatus, TextContent

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "matrix_cron_final_channel",
    ROOT / "plugins/agentteams-matrix-channel/agentteams_matrix/channel.py",
)
CHANNEL = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = CHANNEL
SPEC.loader.exec_module(CHANNEL)
ROOM = "!cron:test"


def message(text):
    return NS(object="message", status=RunStatus.Completed,
              type=MessageType.MESSAGE, content=[TextContent(text=text)])


def job(mode="final", silent=False, channel="agentteams_matrix"):
    return NS(id="test", name="test", task_type="agent",
              request=NS(model_dump=lambda **kw: {}),
              dispatch=NS(target=NS(user_id="@human:test", session_id=f"matrix:{ROOM}"),
                          channel=channel, meta={}, silent=silent, mode=mode),
              runtime=NS(tool_safety=True, share_session=True, timeout_seconds=5))


class CronFinalTest(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.channel = CHANNEL.AgentTeamsMatrixChannel(
            process=lambda *a, **kw: None, share_session_in_group=True,
        )
        self.sent = []
        self.delivered = []
        self.events = []
        self.channel._client = True
        self.channel._write_attachment_context = lambda *a: None
        self.channel._send_typing = AsyncMock()

        async def send(room, kind, content):
            self.sent.append((room, content))
            return NS(event_id=f"$event{len(self.sent)}")

        self.channel._room_send_with_retry = send

        async def stream_query(request):
            for event in self.events:
                yield event

        async def send_event(channel, **kwargs):
            self.delivered.append(kwargs["event"].object)
            if channel == "agentteams_matrix":
                await self.channel.send_event(**kwargs)

        self.runner = executor.CronExecutor(
            workspace=NS(stream_query=stream_query),
            channel_manager=NS(send_event=send_event),
        )
        for name in ("create_trace", "finalize_trace", "append_trace_from_session_delta"):
            ctx = patch.object(executor, name, AsyncMock())
            ctx.start()
            self.addCleanup(ctx.stop)
        ctx = patch.object(executor, "read_session_messages", AsyncMock(return_value=[]))
        ctx.start()
        self.addCleanup(ctx.stop)

    def replies(self):
        return [content.get("m.new_content", {}).get("body")
                for _, content in self.sent if "m.new_content" in content]

    async def test_final_edits_placeholder_and_clears_state_before_next_job(self):
        self.events = [message("intermediate"), message("ok final"),
                       NS(object="response", status=RunStatus.Completed)]
        result = await self.runner.execute(job())
        self.assertEqual(result["delivery_status"], "success")
        self.assertEqual(self.replies(), ["ok final"])
        self.assertEqual(self.delivered, ["message", "response"])
        self.assertEqual(self.channel._proactive_send_state, {})
        self.assertEqual(self.channel._active_thread_roots, {})
        self.events = [message("ok stream"), NS(object="response", status=RunStatus.Completed)]
        await self.runner.execute(job("stream"))
        self.assertEqual(self.replies(), ["ok final", "ok stream"])
        self.assertEqual(len(self.sent), 4)
        self.assertEqual(self.channel._proactive_send_state, {})

    async def test_stream_still_defers_last_message_until_response(self):
        self.events = [message("progress"), message("answer"),
                       NS(object="response", status=RunStatus.Completed)]
        await self.runner.execute(job("stream"))
        self.assertEqual(self.delivered, ["message", "message", "response"])
        self.assertEqual(self.replies(), ["answer"])
        self.assertEqual(self.channel._proactive_send_state, {})

    async def test_silent_job_does_not_send_or_create_state(self):
        self.events = [message("hidden"), NS(object="response", status=RunStatus.Completed)]
        await self.runner.execute(job(silent=True))
        self.assertEqual(self.delivered, [])
        self.assertEqual(self.sent, [])
        self.assertEqual(self.channel._proactive_send_state, {})

    async def test_empty_final_does_not_create_placeholder(self):
        self.events = [NS(object="response", status=RunStatus.Completed)]
        await self.runner.execute(job())
        self.assertEqual(self.delivered, [])
        self.assertEqual(self.sent, [])
        self.assertEqual(self.channel._proactive_send_state, {})

    async def test_other_channels_keep_their_final_delivery_contract(self):
        self.events = [message("answer"), NS(object="response", status=RunStatus.Completed)]
        await self.runner.execute(job(channel="console"))
        self.assertEqual(self.delivered, ["message"])


if __name__ == "__main__":
    unittest.main()
