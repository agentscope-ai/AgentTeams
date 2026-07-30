"""QwenPaw 2 public plugin API contract tests for TeamHarness."""

import asyncio
import importlib.util
from pathlib import Path
import sys
import types


ROOT = Path(__file__).resolve().parents[5]
PLUGIN_PATH = ROOT / "plugins" / "teamharness" / "adapters" / "qwenpaw" / "plugin.py"


def load_plugin():
    spec = importlib.util.spec_from_file_location(
        "teamharness_qwenpaw_plugin_test",
        PLUGIN_PATH,
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class FakeApi:
    def __init__(self):
        self.calls = []

    def __getattr__(self, name):
        def call(*args, **kwargs):
            self.calls.append((name, args, kwargs))

        return call


def test_register_uses_qwenpaw_2_public_extension_points(monkeypatch):
    module = load_plugin()
    class Router:
        def get(self, _path):
            return lambda function: function

        def post(self, _path):
            return lambda function: function

    monkeypatch.setitem(
        sys.modules,
        "fastapi",
        types.SimpleNamespace(APIRouter=Router),
    )
    monkeypatch.setattr(
        module,
        "_register_trace_hooks",
        lambda api: api.calls.append(("trace", (), {})),
    )
    api = FakeApi()
    module.plugin.register(api)
    names = [call[0] for call in api.calls]
    assert names == [
        "register_prompt_section",
        "register_skill_provider",
        "register_middleware",
        "trace",
        "register_http_router",
    ]


def test_sanitizer_redacts_tool_output(monkeypatch):
    module = load_plugin()
    monkeypatch.setenv("AGENTTEAMS_OUTPUT_SANITIZE_KEYWORDS", "secret-value")
    value = {"content": [{"text": "token=secret-value"}]}
    module._sanitize(value)
    assert value["content"][0]["text"] == "token=[REDACTED]"


def test_sanitizer_accepts_qwenpaw_2_middleware_keywords(monkeypatch):
    module = load_plugin()
    middleware_module = types.ModuleType("agentscope.middleware")
    middleware_module.MiddlewareBase = object
    agentscope_module = types.ModuleType("agentscope")
    agentscope_module.middleware = middleware_module
    monkeypatch.setitem(sys.modules, "agentscope", agentscope_module)
    monkeypatch.setitem(sys.modules, "agentscope.middleware", middleware_module)
    middleware = module._sanitizer_factory(None, None)

    async def next_handler(**_kwargs):
        yield {"text": "ok"}

    async def collect():
        return [
            item
            async for item in middleware.on_acting(
                agent=object(),
                input_kwargs={"tool_call": object()},
                next_handler=next_handler,
            )
        ]

    assert asyncio.run(collect()) == [{"text": "ok"}]


def test_team_prompt_reads_packaged_contract():
    module = load_plugin()
    assert "TeamHarness" in module._team_prompt(None)


def test_plugin_does_not_patch_qwenpaw_private_runtime():
    source = PLUGIN_PATH.read_text(encoding="utf-8")
    assert "QwenPawAgent._acting" not in source
    assert "legacy_mcp_client_to_driver" not in source
    assert "save_agent_config" not in source
