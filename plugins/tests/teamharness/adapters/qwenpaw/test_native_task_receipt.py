"""Verify receipt injection at the real SDK's next model-context boundary."""
import asyncio
import importlib.util
import json
from pathlib import Path
import unittest


def native_available():
    try:
        return importlib.util.find_spec("agentscope.agent") is not None
    except (ModuleNotFoundError, ValueError):
        return False


@unittest.skipUnless(native_available(), "native QwenPaw SDK required")
class NativeTaskReceiptTest(unittest.TestCase):
    def test_successful_task_cli_receipt_reaches_model_as_tool_context(self):
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
                    return ChatResponse(content=[ToolCallBlock(id="call-one", name="execute_shell_command", input=json.dumps({"command":"agt task submit t1 --summary Done"}))], is_last=True)
                testcase.assertEqual(self.calls, 2, "receipt triggered an extra model cycle")
                messages = kwargs.get("messages") or (args[0] if args else [])
                messages = await self.formatter.format(messages)
                tool_messages = [m for m in messages if m.get("role") == "tool"]
                text = json.dumps(tool_messages, ensure_ascii=False)
                testcase.assertIn("[Task system receipt]", text)
                testcase.assertIn("leader-a", text)
                testcase.assertIn("Do not resend", text)
                testcase.assertIn("not yet confirmed", text)
                testcase.assertIn("submissionId", text)  # Original result survives.
                for message in messages:
                    if message.get("role") == "assistant":
                        testcase.assertNotIn("[Task system receipt]", str(message.get("content")))
                return ChatResponse(content=[TextBlock(text="Submitted; awaiting review.")], is_last=True)

        async def execute(command: str):
            calls.append(command)
            return {"task":{"id":"t1","status":"submitted","submissionId":"s1"},
                    "notification":{"status":"queued","eventId":"e1","recipient":"leader-a"}}

        async def run():
            toolkit = Toolkit()
            await toolkit.add_tool(FunctionTool(execute, name="execute_shell_command", permission=PermissionDecision(behavior=PermissionBehavior.ALLOW, message="In-memory fixture; no shell or network access")))
            model = Model()
            agent = Agent("Worker", "Execute assigned work.", model, toolkit=toolkit, middlewares=[plugin._sanitizer_factory(None,None)])
            token = plugin._execution_task.set("t1")
            try:
                await agent.reply(Msg(name="Leader", role="user", content=[TextBlock(text="Submit the result.")]))
                testcase.assertEqual(model.calls, 2)
                testcase.assertEqual(calls, ["agt task submit t1 --summary Done"])
            finally:
                plugin._execution_task.reset(token)
        asyncio.run(run())


if __name__ == "__main__":
    unittest.main()
