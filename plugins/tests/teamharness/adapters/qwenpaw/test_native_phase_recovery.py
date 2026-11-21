"""Retryable Task errors stop native reasoning; framework owns resumption."""
import asyncio
import importlib.util
import json
from pathlib import Path
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch


def available():
    try:
        return bool(importlib.util.find_spec("agentscope.agent"))
    except (ValueError, ModuleNotFoundError):
        return False


@unittest.skipUnless(available(), "native SDK required")
class NativePhaseRecoveryTest(unittest.TestCase):
    def test_retryable_error_stops_after_first_model_call_and_preserves_identity(self):
        self._assert_retryable_operation("accept")

    def test_retryable_artifact_read_also_stops_without_an_extra_model_call(self):
        self._assert_retryable_operation("files_read")

    def _assert_retryable_operation(self, operation):
        from agentscope.agent import Agent
        from agentscope.model import ChatModelBase, ChatResponse
        from agentscope.message import Msg, ToolCallBlock, TextBlock
        from agentscope.tool import Toolkit, FunctionTool
        from agentscope.formatter import OpenAIChatFormatter
        from agentscope.permission import PermissionBehavior, PermissionDecision
        from pydantic import BaseModel

        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("native_retry_plugin", root / "plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)
        calls = []
        class Parameters(BaseModel): pass
        class Model(ChatModelBase):
            def __init__(self):
                super().__init__(None, "retry-test", Parameters(), stream=False)
                self.formatter = OpenAIChatFormatter()
                self.calls = 0
            async def __call__(self, *args, **kwargs):
                self.calls += 1
                if self.calls != 1:
                    raise AssertionError("retryable backend failure entered another model call")
                return ChatResponse(content=[ToolCallBlock(id="accept-one", name="mcp__teamharness__task",
                    input=json.dumps({"action": operation, "taskId": "t1", "submissionId": "s1", "note": "Verified"}))], is_last=True)
        async def task(action: str, taskId: str, submissionId: str, note: str):
            calls.append((action, taskId, submissionId))
            return {"ok": False, "error": "task_operation_failed", "retryable": True, "message": "HTTP 503: unavailable"}
        async def run():
            toolkit = Toolkit()
            await toolkit.add_tool(FunctionTool(task, name="mcp__teamharness__task",
                permission=PermissionDecision(behavior=PermissionBehavior.ALLOW, message="Isolated Task fixture")))
            model = Model()
            agent = Agent("Leader", "Review exact candidate.", model, toolkit=toolkit,
                          middlewares=[plugin._sanitizer_factory(None, None)])
            phase = {"id": "original-event", "native_id": "$review", "failed": False}
            phase_token = plugin._phase_retry.set(phase)
            scope_token = plugin._review_task.set(("t1", "s1"))
            try:
                await agent.reply(Msg(name="Worker", role="user", content=[TextBlock(text="Review candidate s1")]))
                self.assertEqual(model.calls, 1)
                self.assertEqual(calls, [(operation, "t1", "s1")])
                self.assertEqual(phase, {"id": "original-event", "native_id": "$review", "failed": True})
                formatted = await model.formatter.format(agent.state.context)
                text = json.dumps(formatted)
                self.assertIn("outcome awaits verification", text)
                self.assertNotIn("[Task system receipt]", text)
            finally:
                plugin._review_task.reset(scope_token)
                plugin._phase_retry.reset(phase_token)
        asyncio.run(run())

    def test_finally_reports_retry_outside_the_native_turn(self):
        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("native_retry_exit_plugin", root / "plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)
        hooks = []
        plugin._register_coordination_hook(SimpleNamespace(register_runtime_hook=hooks.append))
        final = next(hook for hook in hooks if hook.name == "teamharness_coordination_exit")
        async def run():
            token = plugin._phase_retry.set({"id": "original", "native_id": "$attempt", "failed": True})
            ctx = SimpleNamespace(extras={"phase_retry_token": token})
            with patch.object(plugin._phase_module(), "notify", new=AsyncMock(return_value={"retry_queued": True})) as callback:
                await final.run(ctx)
                self.assertIsNone(plugin._phase_retry.get())
                await asyncio.gather(*plugin._phase_jobs)
                callback.assert_awaited_once_with("original", "$attempt", "retry")
                self.assertFalse(plugin._phase_jobs)
        asyncio.run(run())
