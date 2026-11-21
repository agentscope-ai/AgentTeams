"""Native SDK preparation using common public Task operations, never model calls."""
import asyncio
import importlib.util
import json
import os
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, patch


def available():
    try:
        return bool(importlib.util.find_spec("agentscope.agent") and importlib.util.find_spec("qwenpaw.runtime"))
    except (ValueError, ModuleNotFoundError):
        return False


@unittest.skipUnless(available(), "native SDK required")
class NativePublicContextTest(unittest.TestCase):
    def test_public_begin_review_and_closure_prepare_without_model_or_legacy_tools(self):
        from agentscope.agent import Agent
        from agentscope.model import ChatModelBase
        from agentscope.tool import Toolkit, FunctionTool, MCPTool
        import mcp.types
        from agentscope.formatter import OpenAIChatFormatter
        from agentscope.permission import PermissionBehavior, PermissionDecision
        from pydantic import BaseModel

        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("public_context_plugin", root / "plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)
        plugin.ASSET_DIR = root / "plugins/teamharness"

        class Parameters(BaseModel): pass

        class Model(ChatModelBase):
            def __init__(self):
                super().__init__(None, "protocol-test", Parameters(), stream=False)
                self.formatter = OpenAIChatFormatter()
            async def __call__(self, *args, **kwargs):
                raise AssertionError("fixed preparation entered the model")

        async def run(kind):
            calls, texts = [], []
            event = {"id": "event-1", "kind": kind, "task_id": "t1" if kind != "main_task_ready" else "", "main_task_id": "main", "delivery_id": "d1", "submission_id": "s1"}
            task = {"id": "t1", "mainTaskId": "main", "status": "in_progress", "assignedTo": "canonical-cr", "assignmentId": "d1"}
            result = {"task": task, "taskDirectory": "/shared/tasks/t1", "outputDirectory": "/shared/tasks/t1/workspace", "scratchDirectory": "/shared/tasks/t1/workspace/scratch"}
            if kind == "task_submitted":
                task.update(status="submitted", submissionId="s1", candidate={"id": "s1", "artifacts": [], "reportFile": {"artifactId": "report-s1"}})
            if kind == "main_task_ready":
                result = {"task": {"id": "main", "kind": "main", "status": "in_progress", "nodes": [{"taskId": "t1", "status": "completed"}], "acceptedResults": [{"id": "t1", "kind": "execution", "mainTaskId": "main", "status": "completed", "submissionId": "s1", "candidate": {"id": "s1"}, "review": {"id": "r1", "submissionId": "s1", "accepted": True, "status": "completed"}}]}}
            class Session:
                async def call_tool(self, name, arguments, **kwargs):
                    calls.append((arguments["action"], arguments["taskId"]))
                    return mcp.types.CallToolResult(content=[mcp.types.TextContent(type="text", text=json.dumps(result))])
            async def forbidden():
                raise AssertionError("legacy or Shell tool invoked")
            toolkit = Toolkit()
            decision = PermissionDecision(behavior=PermissionBehavior.ALLOW, message="Isolated fixed preparation fixture")
            await toolkit.add_tool(MCPTool("teamharness", mcp.types.Tool(name="task", description="Common Task preparation", inputSchema={"type":"object","required":["action","taskId"],"properties":{"action":{"type":"string"},"taskId":{"type":"string"}}}), session=Session()))
            for name in ("teamharness__taskflow", "teamharness__projectflow", "teamharness__filesync", "execute_shell_command", "run_tool_batch", "workerflow__worker_agentflow", "read_file"):
                await toolkit.add_tool(FunctionTool(forbidden, name=name, permission=decision))
            agent = Agent("Worker", "Use public Tasks", Model(), toolkit=toolkit, middlewares=[plugin._sanitizer_factory(None, None)])
            receipt = {**event, "protocol": "task/v1", "actor": "@leader:test", "recipient": "@recipient:test", "room_id": "!task:test", "control": {"channel_meta": {"teamharness_main_tasks": [{"taskId": "main"}]}}}
            meta = {"event_id": "$current", "teamharness_coordination": event, "teamharness_matrix_coordination": True, "sender_id": receipt["actor"], "agent_matrix_user_id": receipt["recipient"], "room_id": receipt["room_id"]}
            ctx = SimpleNamespace(request=SimpleNamespace(channel_meta=meta), agent=agent, extras={}, inject_context=lambda text, **kw: texts.append(text))
            try:
                with patch.dict(os.environ, {"AGENTTEAMS_AGENT_ROLE": "worker" if kind == "task_assigned" else "leader"}), patch.object(plugin, "_controller_json", return_value=receipt) as backend, patch.object(plugin._phase_module(), "notify", new=AsyncMock(return_value={"active": True, "pending_task_ids": ["main"]})) as lifecycle:
                    self.assertFalse(await plugin._manager_report_context(ctx))
                self.assertEqual(backend.call_count, 1)
                if kind == "task_assigned": lifecycle.assert_not_awaited()
                else: lifecycle.assert_awaited_once_with("event-1", "$current", "begin")
                self.assertEqual(calls, [] if kind == "handoff" else [("begin", "t1")] if kind == "task_assigned" else [("get", "main" if kind == "main_task_ready" else "t1")])
                names = {tool["function"]["name"] for tool in await toolkit.get_tool_schemas()}
                self.assertIn("mcp__teamharness__task", names)
                self.assertNotIn("teamharness__taskflow", names)
                self.assertEqual("execute_shell_command" in names, kind == "task_assigned")
                for name in ("run_tool_batch", "workerflow__worker_agentflow", "read_file"):
                    self.assertEqual(name in names, kind == "task_assigned")
                self.assertTrue(any("# Task Collaboration" in text for text in texts))
                if kind == "task_assigned": self.assertEqual(plugin._ack_scope.get()[2], "canonical-cr")
                elif kind == "task_submitted": self.assertEqual(plugin._review_task.get(), ("t1", "s1"))
                elif kind == "main_task_ready": self.assertEqual(plugin._completion_task.get(), "main")
                else: self.assertEqual(plugin._planning_tasks.get(), frozenset({"main"}))
            finally:
                for key, var in (("execution_task_token", plugin._execution_task), ("ack_scope_token", plugin._ack_scope), ("review_task_token", plugin._review_task), ("completion_task_token", plugin._completion_task), ("source_event_token", plugin._source_event), ("planning_tasks_token", plugin._planning_tasks), ("phase_retry_token", plugin._phase_retry)):
                    if key in ctx.extras: var.reset(ctx.extras[key])
        for kind in ("handoff", "task_assigned", "task_submitted", "main_task_ready"):
            asyncio.run(run(kind))


if __name__ == "__main__": unittest.main()
