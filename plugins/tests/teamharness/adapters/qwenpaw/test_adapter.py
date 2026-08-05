"""QwenPaw 2 public plugin API contract tests for TeamHarness."""

import asyncio
import importlib.util
import sys
import types
from pathlib import Path

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
        types.SimpleNamespace(APIRouter=Router, Request=object),
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


def test_codex_manager_middleware_is_opt_in(monkeypatch):
    module = load_plugin()
    broker = module._load_codex_broker_module()
    monkeypatch.delenv("AGENTTEAMS_CODEX_MANAGER_ENABLED", raising=False)
    monkeypatch.delenv("AGENTTEAMS_CODEX_BROKER_TOKEN", raising=False)
    assert broker.manager_middleware_factory(None, None) is None


def test_codex_manager_middleware_round_trip(monkeypatch):
    module = load_plugin()
    broker = module._load_codex_broker_module()
    broker.BROKER = broker.ExecutionBroker()
    monkeypatch.setenv("AGENTTEAMS_CODEX_MANAGER_ENABLED", "true")
    monkeypatch.setenv("AGENTTEAMS_CODEX_BROKER_TOKEN", "capability-token")

    class Event:
        def __init__(self, **kwargs):
            self.__dict__.update(kwargs)

    class TextBlock(Event):
        pass

    class Msg(Event):
        pass

    middleware_module = types.ModuleType("agentscope.middleware")
    middleware_module.MiddlewareBase = object
    event_module = types.ModuleType("agentscope.event")
    event_module.TextBlockStartEvent = Event
    event_module.TextBlockDeltaEvent = Event
    event_module.TextBlockEndEvent = Event
    message_module = types.ModuleType("agentscope.message")
    message_module.Msg = Msg
    message_module.TextBlock = TextBlock
    monkeypatch.setitem(sys.modules, "agentscope.middleware", middleware_module)
    monkeypatch.setitem(sys.modules, "agentscope.event", event_module)
    monkeypatch.setitem(sys.modules, "agentscope.message", message_module)

    middleware = broker.manager_middleware_factory(None, None)
    agent = types.SimpleNamespace(
        name="manager",
        state=types.SimpleNamespace(session_id="room-1", reply_id="reply-1", context=[]),
    )

    async def collect():
        async def next_handler(**_kwargs):
            yield "native-model-must-not-run"

        task = asyncio.create_task(
            _collect_async_generator(
                middleware.on_reply(
                    agent=agent,
                    input_kwargs={"message": "coordinate this task"},
                    next_handler=next_handler,
                )
            )
        )
        await asyncio.sleep(0.01)
        execution = broker.BROKER.lease()
        assert execution["role"] == "manager"
        assert execution["sandbox"] == "read-only"
        assert execution["prompt"] == "coordinate this task"
        broker.BROKER.complete(execution["executionId"], output="delegated by Codex")
        return await task

    items = asyncio.run(collect())
    assert len(items) == 4
    assert items[1].delta == "delegated by Codex"
    assert items[-1].content[0].text == "delegated by Codex"


def test_codex_broker_releases_expired_lease(monkeypatch):
    module = load_plugin()
    broker_module = module._load_codex_broker_module()
    broker = broker_module.ExecutionBroker()
    execution = broker.submit(session_key="room-1", prompt="coordinate")
    first = broker.lease()
    assert first["executionId"] == execution.execution_id
    monkeypatch.setenv("AGENTTEAMS_CODEX_BROKER_LEASE_TIMEOUT", "0")
    second = broker.lease()
    assert second["executionId"] == execution.execution_id


def test_codex_broker_bounds_prompts_and_results():
    module = load_plugin()
    broker_module = module._load_codex_broker_module()
    broker = broker_module.ExecutionBroker()
    execution = broker.submit(session_key="room", prompt="p" * 100_100)
    leased = broker.lease()
    assert len(leased["prompt"]) == 100_000
    assert broker.complete(execution.execution_id, output="o" * 20_100)
    assert len(broker.wait(execution, 0).output) == 20_000


async def _collect_async_generator(generator):
    return [item async for item in generator]
