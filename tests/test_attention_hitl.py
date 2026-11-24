import copy
import importlib.util
from pathlib import Path
import unittest
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "plugins/teamharness"))
spec = importlib.util.spec_from_file_location("hitl_answer", ROOT / "plugins/teamharness/mcp/hitl_answer.py")
hitl = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hitl)


class AttentionDecisions(unittest.TestCase):
    def setUp(self):
        self.task = {"task_id": "one", "room_id": "!room:s", "status": "in_progress", "attention": [{
            "kind": "decision", "confirmation_id": "attention:one:nonce", "requested_of": ["@human:s"],
            "resolved": False, "options": [{"id": "yes", "label": "Continue"}]}]}
        self.content = {"com.agentteams.answer": {"agent_id": "@worker:s", "confirmation_id": "attention:one:nonce",
            "responses": [{"id": "decision", "selected": ["yes"]}]}}

    def apply(self, **changes):
        arguments = dict(task=self.task, room="!room:s", event_id="$answer", sender="@human:s",
                         content=self.content, owner="@worker:s", now=10)
        return hitl.apply_answer(**{**arguments, **changes})

    def test_answer_is_durable_and_duplicate_events_do_not_change_decision(self):
        self.assertTrue(self.apply())
        restored = copy.deepcopy(self.task)
        self.assertTrue(self.apply(task=restored))
        self.assertFalse(self.apply(event_id="$different-answer"))
        self.assertEqual(restored["attention"][0]["answer"]["responses"], [{"id": "decision", "selected": ["yes"]}])
        self.assertEqual(restored["status"], "in_progress")

    def test_wrong_room_sender_owner_request_and_option_are_rejected(self):
        for changes in ({"room": "!other:s"}, {"sender": "@intruder:s"}, {"owner": "@other:s"}):
            self.assertFalse(self.apply(**changes))
        self.content["com.agentteams.answer"]["confirmation_id"] = "attention:one:old"
        self.assertFalse(self.apply())
        self.content["com.agentteams.answer"]["confirmation_id"] = "attention:one:nonce"
        self.content["com.agentteams.answer"]["responses"][0]["selected"] = ["forged"]
        self.assertFalse(self.apply())
        self.assertNotIn("answer", self.task["attention"][0])

    def test_timeout_cancellation_and_superseded_tasks_are_rejected(self):
        self.task["attention"][0]["timeoutAt"] = "1970-01-01T00:00:05Z"
        self.assertFalse(self.apply())
        del self.task["attention"][0]["timeoutAt"]
        for status in ("cancelled", "completed", "revision", "blocked"):
            self.task["status"] = status
            self.assertFalse(self.apply())

    def test_answer_cannot_grant_business_approval_or_unfreeze(self):
        self.task["attention"][0]["kind"] = "approval"
        self.task["decision_status"] = "frozen"
        self.assertTrue(self.apply())
        self.assertFalse(self.task["attention"][0]["resolved"])
        self.assertEqual(self.task["decision_status"], "frozen")

    def test_tool_decision_cannot_settle_a_question(self):
        self.content["com.agentteams.answer"]["kind"] = "tool_approval"
        self.assertFalse(self.apply())
        self.assertNotIn("answer", self.task["attention"][0])


if __name__ == "__main__":
    unittest.main()
