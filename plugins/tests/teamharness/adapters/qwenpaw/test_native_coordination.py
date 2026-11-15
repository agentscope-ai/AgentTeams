"""Run the coordination hook through the native SDK orchestration phases."""
import asyncio
import importlib.util
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, patch


def _native_sdk_available():
    try:
        return bool(importlib.util.find_spec("agentscope.agent") and importlib.util.find_spec("qwenpaw.runtime"))
    except (ModuleNotFoundError, ValueError):
        return False


@unittest.skipUnless(_native_sdk_available(), "native SDK required")
class NativeCoordinationTest(unittest.TestCase):
    def test_exact_output_move_permission_is_task_scoped(self):
        from agentscope.permission import PermissionBehavior, PermissionDecision
        import tempfile
        path = Path("/opt/agentteams/qwenpaw-builtin/plugins/teamharness/plugin.py")
        spec = importlib.util.spec_from_file_location("native_output_scope_plugin", path)
        plugin = importlib.util.module_from_spec(spec); spec.loader.exec_module(plugin)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "tasks/t1"; root.mkdir(parents=True)
            state = {"task_id":"t1", "status":"in_progress", "assigned_to":"worker", "notifyTxn":"d1"}
            plugin._controller_json = lambda _: state
            middleware = plugin._sanitizer_factory(None, None)
            middleware._task_scope = ("t1", root, "worker", "d1")
            async def decide(command):
                async def native(**kw):
                    return PermissionDecision(behavior=PermissionBehavior.ASK, message="native policy")
                return await middleware.on_check_permission(None, {"tool_call":SimpleNamespace(name="execute_shell_command"),
                    "tool_input":{"command":command}}, native)
            self.assertEqual(asyncio.run(decide(f'mv {root}/new {root}/output')).behavior, PermissionBehavior.ALLOW)
            self.assertEqual(asyncio.run(decide(f'mv {root}/new {root.parent}/other/output')).behavior, PermissionBehavior.ASK)
            self.assertEqual(asyncio.run(decide(f'cd {root} && mv new output')).behavior, PermissionBehavior.DENY)
            state["status"] = "cancelled"
            self.assertEqual(asyncio.run(decide(f'mv {root}/new {root}/output')).behavior, PermissionBehavior.ASK)

    def test_durable_submit_finishes_without_a_second_model_call(self):
        self._assert_durable_transition_finishes("submit_task", "teamharness__taskflow", {
            "ok": True, "synced": True, "task": {"task_id": "atomic-1", "status": "submitted"},
            "notification": {"sent": True}}, "_execution_task", "atomic-1", "result submitted")

    def test_accepted_review_finishes_without_a_second_model_call(self):
        self._assert_durable_transition_finishes("accept_task_result", "teamharness__projectflow", {
            "ok": True, "synced": True, "taskId": "atomic-1", "submissionId": "submission-1",
            "accepted": True, "nodeStatus": "completed", "dispatch": {"ok": True}},
            "_review_task", ("atomic-1", "submission-1"), "accepted; the task system")

    def _assert_durable_transition_finishes(self, action, tool_name, receipt, context_name, context_value, expected):
        from agentscope.agent import Agent
        from agentscope.model import ChatModelBase, ChatResponse
        from agentscope.message import Msg, ToolCallBlock
        from agentscope.tool import Toolkit, FunctionTool, ToolResponse
        from agentscope.formatter import OpenAIChatFormatter
        from agentscope.permission import PermissionBehavior, PermissionDecision
        from pydantic import BaseModel
        import json
        path = Path("/opt/agentteams/qwenpaw-builtin/plugins/teamharness/plugin.py")
        spec = importlib.util.spec_from_file_location("native_finish_plugin", path)
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)
        class Parameters(BaseModel):
            pass
        class Model(ChatModelBase):
            def __init__(self):
                super().__init__(None, "protocol-test", Parameters(), stream=False)
                self.formatter = OpenAIChatFormatter()
                self.calls = 0
            async def __call__(self, *args, **kwargs):
                self.calls += 1
                if self.calls > 1:
                    raise AssertionError("submission triggered another model call")
                return ChatResponse(content=[ToolCallBlock(id="call-1", name=tool_name, input=json.dumps({"action": action}))], is_last=True)
        async def submit(action: str):
            return receipt
        async def run():
            toolkit = Toolkit()
            await toolkit.add_tool(FunctionTool(submit, name=tool_name,
                permission=PermissionDecision(behavior=PermissionBehavior.ALLOW, message="Isolated test tool; no external effects")))
            model = Model()
            agent = Agent("Worker", "Execute the assignment.", model, toolkit=toolkit,
                middlewares=[plugin._sanitizer_factory(None,None)])
            context = getattr(plugin, context_name)
            token = context.set(context_value)
            try:
                result = await agent.reply(Msg(name="Leader",role="user",content=[{"type":"text","text":"Submit the result."}]))
                self.assertEqual(model.calls, 1)
                self.assertIn(expected, str(result.content))
            finally:
                context.reset(token)
        asyncio.run(run())

    def test_native_approval_future_exposes_its_resolution(self):
        from qwenpaw.app.approvals import get_approval_service, ApprovalRequestSummary
        from qwenpaw.security.tool_guard.approval import ApprovalDecision
        path = Path("/opt/agentteams/qwenpaw-builtin/plugins/teamharness/plugin.py")
        spec = importlib.util.spec_from_file_location("native_approval_plugin", path)
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)
        async def run():
            service = get_approval_service()
            pending = await service.create_pending_summary(session_id="coordination-task:probe:dispatch", root_session_id="coordination-task:probe:dispatch",
                owner_agent_id="default", user_id="@human:test", channel="console", agent_id="default",
                summary=ApprovalRequestSummary(source_type="test", name="Bash", result_summary="Review the test command"),
                extra={"tool_call":{"input":{"command":"echo probe"}}})
            records = await plugin._coordination_approvals()
            self.assertEqual(records[0]["status"], "pending")
            await service.resolve_request(pending.request_id, ApprovalDecision.APPROVED)
            await asyncio.sleep(0)
            self.assertEqual((await plugin._coordination_approvals())[0]["status"], "approved")
        asyncio.run(run())

    def test_peer_update_does_not_enter_model_executor(self):
        self._assert_model_executor_skipped()

    def test_verified_final_report_does_not_enter_model_executor(self):
        self._assert_model_executor_skipped("Verified accepted report")

    def _assert_model_executor_skipped(self, final_report=None):
        import qwenpaw.runtime.runtime as runtime_module
        from qwenpaw.runtime.hooks import HookResult
        from qwenpaw.runtime.phases import Phase
        path = Path("/opt/agentteams/qwenpaw-builtin/plugins/teamharness/plugin.py")
        if len(Path(__file__).resolve().parents) > 5:
            source = Path(__file__).resolve().parents[5] / "plugins/teamharness/adapters/qwenpaw/plugin.py"
            if source.exists():
                path = source
        spec = importlib.util.spec_from_file_location("native_coordination_plugin", path)
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)
        hooks = []
        plugin._register_coordination_hook(SimpleNamespace(register_runtime_hook=hooks.append))
        self.assertEqual(hooks[0].phase, Phase.PRE_EXECUTE)

        async def run_phase(phase, ctx):
            for hook in hooks:
                if hook.phase == phase:
                    return await hook.run(ctx)
            return HookResult()

        workspace = SimpleNamespace(plugins=SimpleNamespace(
            hook_registry=SimpleNamespace(run=run_phase), modes=[],
            slash_command_registry=SimpleNamespace(dispatch=AsyncMock(return_value=None))))
        runtime = runtime_module.Runtime(workspace=workspace, app_services=None)
        ctx = SimpleNamespace(session_id="matrix:!test:test", input_msgs=[],
            extras={"verified_final_report": final_report} if final_report else {}, agent=None)
        runtime._normalize = lambda request: request
        runtime._build_context = lambda request: ctx

        async def run():
            return [event async for event in runtime.run(SimpleNamespace())]

        with patch.object(plugin, "_manager_report_context", AsyncMock(return_value=True)), \
             patch.object(runtime_module, "AgentBuilder", return_value=SimpleNamespace(build=AsyncMock(return_value=SimpleNamespace()))), \
             patch.object(runtime_module, "AgentExecutor", side_effect=AssertionError("peer event started the model")):
            events = asyncio.run(run())
            self.assertTrue(events)
            if final_report:
                self.assertIn(final_report, str(events))


if __name__ == "__main__":
    unittest.main()
