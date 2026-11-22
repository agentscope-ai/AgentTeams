"""The public intake contract must reach the native model, not just a registry."""
import asyncio
import importlib.util
import json
import os
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
import unittest
from unittest.mock import patch


def available():
    try:
        return bool(importlib.util.find_spec("agentscope.agent"))
    except (ValueError, ModuleNotFoundError):
        return False


@unittest.skipUnless(available(), "native SDK required")
class NativeIntakePromptTest(unittest.TestCase):
    def test_shared_intake_contract_reaches_actual_model_with_sdk_prompt_builder(self):
        from agentscope.agent import Agent
        from agentscope.formatter import OpenAIChatFormatter
        from agentscope.message import Msg, TextBlock
        from agentscope.model import ChatModelBase, ChatResponse
        from pydantic import BaseModel
        from qwenpaw.config.config import AgentProfileConfig
        from qwenpaw.runtime.builder import AgentBuilder
        from qwenpaw.runtime.prompt_contributors import build_default_prompt_manager

        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("native_intake_prompt", root / "plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)

        class Parameters(BaseModel): pass
        class Model(ChatModelBase):
            def __init__(self):
                super().__init__(None, "intake-test", Parameters(), stream=False)
                self.formatter = OpenAIChatFormatter()
                self.seen = []
            async def __call__(self, *args, **kwargs):
                self.seen.append(json.dumps([args, kwargs], default=str))
                return ChatResponse(content=[TextBlock(text="Understood")], is_last=True)

        async def run(directory):
            manager_prompt = root / "plugins/teamharness/prompts/manager/AGENTS.md"
            Path(directory, "MANAGER.md").write_text(manager_prompt.read_text(encoding="utf-8"), encoding="utf-8")
            ctx = SimpleNamespace(workspace_dir=Path(directory), agent_id="fixture", extras={},
                workspace=SimpleNamespace(plugins=SimpleNamespace(prompt_manager=build_default_prompt_manager())))
            builder = AgentBuilder()
            builder._build_env_context = lambda *_: ""
            builder._get_driver_prompt_hints = lambda *_: []
            host_prompt = builder.build_prompt(ctx, AgentProfileConfig(id="fixture", name="Manager", system_prompt_files=["MANAGER.md"]))
            model = Model()
            agent = Agent("Manager", host_prompt, model,
                middlewares=[plugin._sanitizer_factory(None, None)])
            await agent.reply(Msg(name="Human", role="user", content=[TextBlock(text="Please answer normally.")]))
            await agent.reply(Msg(name="Human", role="user", content=[TextBlock(text="Another ordinary question.")]))
            self.assertEqual(len(model.seen), 2)
            for request in model.seen:
                self.assertEqual(request.count("Different supplied input IDs are distinct Human message events"), 1)
                self.assertIn("relatedInputIds", request)
                self.assertIn("Use the common Task create action for managed Team work", request)
                self.assertIn("Handle AgentTeams administration directly", request)
                self.assertIn("you have verified that no suitable registered Team can take the work", request)
                self.assertIn("Choose the best fit for the request", request)
                self.assertIn("Leave decomposition and execution assignments to that Leader", request)
                self.assertNotIn("Delegate using", request)
                self.assertNotIn("GOAL AND ACCEPTANCE", request)

        with TemporaryDirectory() as directory, patch.dict(os.environ, {
            "AGENTTEAMS_AGENT_ROLE": "manager", "AGENTTEAMS_CONTROLLER_URL": "http://controller:8090",
        }):
            asyncio.run(run(directory))

    def test_native_model_call_records_metadata_and_forwards_inputs_unchanged(self):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock
        try:
            from opentelemetry.sdk.trace import TracerProvider
        except ImportError:
            self.skipTest("optional tracing SDK required")
        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("native_intake_metadata", root / "plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)

        async def run():
            contract = plugin._team_prompt(None)
            hidden = "synthetic private content must not be exported"
            source = {"inputId": "input-synthetic", "eventId": "$synthetic"}
            messages = [SimpleNamespace(role="system", content=[{"text": contract}]),
                        SimpleNamespace(role="user", content=[{"text": "[Message $synthetic]\ninput-synthetic\n" + hidden}])]
            arguments = {"messages": messages, "tools": [], "tool_choice": "auto"}
            result = object()
            forward = AsyncMock(return_value=result)
            middleware = plugin._sanitizer_factory(None, None)
            provider = TracerProvider()
            token = plugin._creation_sources.set([source])
            try:
                with provider.get_tracer("intake-test").start_as_current_span("native-turn") as span:
                    self.assertIs(await middleware.on_model_call(None, arguments, forward), result)
                    encoded = span.attributes["agentteams.task.intake_checks"]
                    check = json.loads(encoded)[0]
                    self.assertTrue(check["currentSystemContractPresent"])
                    self.assertEqual(check["systemMessages"], 1)
                    self.assertEqual(check["providedInputs"], check["userSourceMarkers"])
                    self.assertEqual(check["providedInputs"], check["visibleInputReferences"])
                    self.assertNotIn(hidden, encoded)
                    self.assertNotIn("$synthetic", encoded)
                    self.assertEqual(list(span.attributes["agentteams.input.event_ids"]), ["$synthetic"])
                    # A registered input without its actual model marker/ref
                    # must not be claimed as admitted by this model call.
                    extra = plugin._creation_sources.set([source, {"inputId": "input-absent", "eventId": "$absent"}])
                    try:
                        await middleware.on_model_call(None, arguments, forward)
                        self.assertEqual(list(span.attributes["agentteams.input.event_ids"]), ["$synthetic"])
                    finally:
                        plugin._creation_sources.reset(extra)
                self.assertEqual(forward.await_count, 2)
                forward.assert_awaited_with(**arguments)
            finally:
                plugin._creation_sources.reset(token)
                provider.shutdown()

        with patch.dict(os.environ, {"AGENTTEAMS_AGENT_ROLE": "manager", "AGENTTEAMS_CONTROLLER_URL": "http://controller:8090", "AGENTTEAMS_OTEL_ENABLED": "true"}):
            asyncio.run(run())
