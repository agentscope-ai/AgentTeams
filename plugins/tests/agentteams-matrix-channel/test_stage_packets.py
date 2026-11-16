"""Exercise the packaged channel entrypoint against stage handoff packets."""
import asyncio
import importlib.util
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, patch


def _native_channel_available():
    try:
        return bool(importlib.util.find_spec("qwenpaw.schemas") and importlib.util.find_spec("nio"))
    except (ModuleNotFoundError, ValueError):
        return False


@unittest.skipUnless(_native_channel_available(), "native channel dependencies required")
class StagePacketTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        root = Path(__file__).resolve().parents[3] if len(Path(__file__).resolve().parents) > 3 else Path("/nonexistent")
        cls.root = root
        path = root / "plugins/agentteams-matrix/adapters/qwenpaw/agentteams_matrix/channel.py"
        if not path.exists():
            path = Path("/opt/agentteams/qwenpaw-builtin/plugins/agentteams-matrix-channel/agentteams_matrix/channel.py")
            core_root = path.parents[1]
        else:
            core_root = root / "plugins/agentteams-matrix"
        with patch.object(sys, "path", [str(core_root), *sys.path]):
            spec = importlib.util.spec_from_file_location("stage_packet_channel", path)
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)
        cls.channel_type = module.AgentTeamsMatrixChannel

    def receive(self, full, body, extra=None):
        channel = self.channel_type(process=lambda *args, **kwargs: None)
        channel._user_id = "@receiver:test"
        channel._session_receipts = object()
        received = []
        channel._enqueue = received.append
        event = SimpleNamespace(sender="@leader:test", event_id="$event", body=body, source={"content": {
            "org.agentteams.report": {"text": full},
            "org.agentteams.workflow": {"type": "PROJECT_COMPLETED", "project_id": "p"},
            "org.agentteams.requester_route": {"channel": "matrix", "target_session": "!human:test"},
        }})
        event.source["content"].update(extra or {})
        with patch.object(channel, "_is_dm_room", AsyncMock(return_value=True)), \
             patch.object(channel, "_announce_session_receipts", AsyncMock()), \
             patch.object(channel, "_send_typing", AsyncMock()), \
             patch.object(channel, "_is_channel_disabled", return_value=False), \
             patch.object(channel, "_is_thread_event", return_value=False), \
             patch.object(channel, "_targeted_readiness_probe_reply", return_value=None), \
             patch.object(channel, "_strip_mention_prefix", side_effect=lambda text, room: text), \
             patch.object(channel, "_control_command_text", return_value=None):
            asyncio.run(channel._on_room_event(SimpleNamespace(room_id="!room:test"), event))
        return received[0] if received else None

    def assignment(self):
        return {"session_id": "coordination-task:t:dispatch-1", "input": [{"content": [{"text": "Read the bounded task spec and ACK t."}]}], "channel_meta": {
            "room_id": "!room:test", "sender_id": "@leader:test", "agent_matrix_user_id": "@receiver:test",
            "task_id": "t", "project_id": "p", "teamharness_coordination": {"kind": "task_assigned", "task_id": "t", "project_id": "p", "delivery_id": "dispatch-1"}}}

    def test_matrix_assignment_uses_bounded_context_and_isolated_session(self):
        packet = self.receive("", "@receiver 前置环节已验收，请继续任务。", {"org.agentteams.coordination": self.assignment()})
        self.assertEqual(packet["content_parts"][0].text, "Read the bounded task spec and ACK t.")
        self.assertEqual(packet["meta"]["event_id"], "$event")
        self.assertTrue(packet["meta"]["teamharness_matrix_coordination"])
        channel = self.channel_type(process=lambda *args, **kwargs: None)
        self.assertEqual(channel.get_debounce_key(packet), "coordination-task:t:dispatch-1")
        request = channel.build_agent_request_from_native(packet)
        self.assertEqual(request.session_id, "coordination-task:t:dispatch-1")
        self.assertEqual(channel.get_to_handle_from_request(request), "!room:test")

    def test_forged_sender_or_recipient_cannot_enter_control_context(self):
        for key, value in (("sender_id", "@impostor:test"), ("agent_matrix_user_id", "@other:test"), ("room_id", "!other:test")):
            control = self.assignment(); control["channel_meta"][key] = value
            self.assertIsNone(self.receive("", "visible message", {"org.agentteams.coordination": control}))

    def test_control_responses_remain_visible_without_waking_peers(self):
        channel = self.channel_type(process=lambda *args, **kwargs: None)
        content = channel._matrix_text_content("!room:test", "Result submitted", {"teamharness_matrix_coordination": True}, "m.text")
        self.assertTrue(content["org.agentteams.progress_only"])
        self.assertEqual(content["m.mentions"], {"user_ids": []})

    def test_control_edits_cannot_wake_peers_either(self):
        channel = self.channel_type(process=lambda *args, **kwargs: None)
        channel._client = object()
        channel._room_send_with_retry = AsyncMock()
        asyncio.run(channel._edit_matrix_event("!room:test", "$root", "Result submitted", msgtype="m.text", progress_only=True))
        content = channel._room_send_with_retry.call_args.args[2]
        self.assertTrue(content["org.agentteams.progress_only"])
        self.assertTrue(content["m.new_content"]["org.agentteams.progress_only"])

    def test_full_handoff_and_verified_event_metadata_reach_request(self):
        full = "\n".join(f"acceptance {n}" for n in range(8))
        packet = self.receive(full, "\n".join(full.splitlines()[:5]) + "\n…preview")
        self.assertEqual(packet["content_parts"][0].text, full)
        self.assertEqual(packet["meta"]["agent_matrix_user_id"], "@receiver:test")
        self.assertEqual(packet["meta"]["teamharness_workflow"]["project_id"], "p")
        self.assertEqual(packet["meta"]["teamharness_requester_route"]["target_session"], "!human:test")

    def test_mismatching_preview_cannot_replace_visible_request(self):
        packet = self.receive("different request", "visible request")
        self.assertEqual(packet["content_parts"][0].text, "visible request")

    def test_api_progress_projection_never_starts_another_agent_turn(self):
        channel = self.channel_type(process=lambda *args, **kwargs: None)
        channel._is_dm_room = AsyncMock(side_effect=AssertionError("projection reached routing"))
        event = SimpleNamespace(source={"content": {"org.agentteams.progress_only": True, "m.mentions": {"user_ids": ["@receiver:test"]}}})
        asyncio.run(channel._on_room_event(SimpleNamespace(room_id="!room:test"), event))


if __name__ == "__main__":
    unittest.main()
