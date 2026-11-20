"""Verify receipt injection at the real SDK's next model-context boundary."""
import asyncio
import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch
import os
import shlex


def native_available():
    try:
        return importlib.util.find_spec("agentscope.agent") is not None
    except (ModuleNotFoundError, ValueError):
        return False


@unittest.skipUnless(native_available(), "native QwenPaw SDK required")
class NativeTaskReceiptTest(unittest.TestCase):
    def test_successful_task_cli_receipt_reaches_model_as_tool_context(self):
        self._assert_receipt("submit")

    def test_completion_receipt_reaches_leader_context_without_a_send_tool(self):
        self._assert_receipt("complete")

    def test_creation_receipt_binds_native_source_without_model_route_arguments(self):
        self._assert_receipt("create")

    def test_acceptance_receipt_reaches_exact_candidate_review_context(self):
        self._assert_receipt("accept")

    def test_rejection_receipt_reaches_exact_candidate_review_context(self):
        self._assert_receipt("reject")

    def test_plan_receipt_reaches_assigned_main_context(self):
        self._assert_receipt("plan")

    def _assert_receipt(self, action):
        from agentscope.agent import Agent
        from agentscope.model import ChatModelBase, ChatResponse
        from agentscope.message import Msg, ToolCallBlock, TextBlock
        from agentscope.tool import Toolkit, FunctionTool
        from agentscope.formatter import OpenAIChatFormatter
        from agentscope.permission import PermissionBehavior, PermissionDecision
        from pydantic import BaseModel

        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("receipt_native_plugin", root / "plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)
        testcase = self
        calls = []
        command = f"agt task {action} t1 --" + ("summary" if action == "submit" else "report") + " Done"
        if action == "create": command = 'agt task create --leader leader-a --title "Request" --spec "Original criteria"'
        if action in {"accept", "reject"}: command = f'agt task {action} t1 --submission s1 --note "Business validation evidence"'
        if action == "plan": command = 'agt task plan t1 --json \'{"nodes":[{"id":"A","assignedTo":"worker-a","spec":"Implement"}]}\''
        identity_field = "sourceEventId" if action == "create" else ("submissionId" if action == "submit" else "completionId")

        class Parameters(BaseModel):
            pass

        class Model(ChatModelBase):
            def __init__(self):
                super().__init__(None, "isolated-receipt-test", Parameters(), stream=False)
                self.formatter = OpenAIChatFormatter()
                self.calls = 0

            async def __call__(self, *args, **kwargs):
                self.calls += 1
                if self.calls == 1:
                    return ChatResponse(content=[ToolCallBlock(id="call-one", name="execute_shell_command", input=json.dumps({"command":command}))], is_last=True)
                testcase.assertEqual(self.calls, 2, "receipt triggered an extra model cycle")
                messages = kwargs.get("messages") or (args[0] if args else [])
                messages = await self.formatter.format(messages)
                tool_messages = [m for m in messages if m.get("role") == "tool"]
                text = json.dumps(tool_messages, ensure_ascii=False)
                testcase.assertIn("[Task system receipt]", text)
                if action not in {"accept", "reject"}: testcase.assertIn("worker-a" if action == "plan" else "manager-a" if action == "complete" else "leader-a", text)
                if action == "complete": testcase.assertIn("original conversation", text)
                testcase.assertIn("Do not recreate" if action == "plan" else "Do not repeat" if action in {"accept", "reject"} else "Do not resend" if action == "submit" else "Do not send another", text)
                if action in {"accept", "reject"}:
                    testcase.assertIn("synchronization is pending", text)
                    testcase.assertIn("operation-one", text)
                else:
                    if action != "create": testcase.assertIn("not yet confirmed", text)
                    testcase.assertIn("planId" if action == "plan" else identity_field, text)  # Original result survives.
                for message in messages:
                    if message.get("role") == "assistant":
                        testcase.assertNotIn("[Task system receipt]", str(message.get("content")))
                return ChatResponse(content=[TextBlock(text="Submitted; awaiting review.")], is_last=True)

        async def execute(command: str):
            calls.append(command)
            if action == "create":
                return {"sourceEventId":"$request", "task":{"id":"t1","kind":"main","status":"planned"},"notification":{"status":"queued","eventId":"handoff","recipient":"leader-a"}}
            if action in {"accept", "reject"}:
                state = "completed" if action == "accept" else "revision"
                return {"task":{"id":"t1","submissionId":"s1","status":state},
                        "review":{"id":"operation-one","submissionId":"s1","accepted":action == "accept","status":state},
                        "mainProjection":"pending"}
            if action == "plan":
                return {"task":{"id":"t1","kind":"main","status":"assigned","planId":"plan-one"},"progress":"ready",
                        "assignments":[{"taskId":"execution-a","status":"queued","eventId":"assignment-one","recipient":"worker-a"}]}
            return {"task":{"id":"t1","status":"submitted" if action == "submit" else "completed",identity_field:"operation-one"},
                    "notification":{"status":"queued","eventId":"e1","recipient":"manager-a" if action == "complete" else "leader-a"}}

        async def run():
            toolkit = Toolkit()
            await toolkit.add_tool(FunctionTool(execute, name="execute_shell_command", permission=PermissionDecision(behavior=PermissionBehavior.ALLOW, message="In-memory fixture; no shell or network access")))
            model = Model()
            agent = Agent("Worker", "Execute assigned work.", model, toolkit=toolkit, middlewares=[plugin._sanitizer_factory(None,None)])
            context = (plugin._source_event if action == "create" else plugin._execution_task if action == "submit"
                       else plugin._planning_tasks if action == "plan" else plugin._review_task if action in {"accept", "reject"} else plugin._completion_task)
            token = context.set("$request" if action == "create" else frozenset({"t1"}) if action == "plan" else ("t1","s1") if action in {"accept", "reject"} else "t1")
            route_token = plugin._requester_route.set({"target_user":"@human:test","target_session":"!source:test"}) if action == "create" else None
            try:
                await agent.reply(Msg(name="Leader", role="user", content=[TextBlock(text="Submit the result.")]))
                testcase.assertEqual(model.calls, 2)
                if action == "create":
                    testcase.assertEqual(len(calls),1)
                    words=shlex.split(calls[0])
                    testcase.assertEqual(words[:4],["env","AGENTTEAMS_ORIGIN_EVENT_ID=$request","AGENTTEAMS_REQUESTER_USER_ID=@human:test","AGENTTEAMS_REQUESTER_ROOM_ID=!source:test"])
                    testcase.assertEqual(words[4:],shlex.split(command))
                else: testcase.assertEqual(calls, [command])
            finally:
                context.reset(token)
                if route_token is not None: plugin._requester_route.reset(route_token)
        with patch.dict(os.environ,{"AGENTTEAMS_AGENT_ROLE":"manager" if action == "create" else "worker"}):
            asyncio.run(run())


if __name__ == "__main__":
    unittest.main()
