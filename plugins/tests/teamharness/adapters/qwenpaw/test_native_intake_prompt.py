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
            Path(directory, "MANAGER.md").write_text("You are the Manager.", encoding="utf-8")
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

        with TemporaryDirectory() as directory, patch.dict(os.environ, {
            "AGENTTEAMS_AGENT_ROLE": "manager", "AGENTTEAMS_CONTROLLER_URL": "http://controller:8090",
        }):
            asyncio.run(run(directory))
