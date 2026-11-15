"""Exercise the packaged channel entrypoint against stage handoff packets."""
import asyncio
import importlib.util
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, patch


@unittest.skipUnless(importlib.util.find_spec("qwenpaw") and importlib.util.find_spec("nio"), "native channel dependencies required")
class StagePacketTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        root = Path(__file__).resolve().parents[3]
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

    def receive(self, full, body):
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
        with patch.object(channel, "_is_dm_room", AsyncMock(return_value=True)), \
             patch.object(channel, "_announce_session_receipts", AsyncMock()), \
             patch.object(channel, "_send_typing", AsyncMock()), \
             patch.object(channel, "_is_channel_disabled", return_value=False), \
             patch.object(channel, "_is_thread_event", return_value=False), \
             patch.object(channel, "_targeted_readiness_probe_reply", return_value=None), \
             patch.object(channel, "_strip_mention_prefix", side_effect=lambda text, room: text), \
             patch.object(channel, "_control_command_text", return_value=None):
            asyncio.run(channel._on_room_event(SimpleNamespace(room_id="!room:test"), event))
        return received[0]

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
        event = SimpleNamespace(source={"content": {"org.agentteams.progress_only": True}})
        asyncio.run(channel._on_room_event(SimpleNamespace(room_id="!room:test"), event))


if __name__ == "__main__":
    unittest.main()
