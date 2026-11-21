"""QwenPaw 2 public plugin API contract tests for TeamHarness."""

import asyncio
import importlib.util
from pathlib import Path
import sys
import types

import pytest


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
    monkeypatch.setattr(module, "_register_coordination_hook",
                        lambda api: api.calls.append(("coordination", (), {})))
    api = FakeApi()
    module.plugin.register(api)
    names = [call[0] for call in api.calls]
    assert names == [
        "register_prompt_section",
        "register_skill_provider",
        "register_middleware",
        "trace",
        "coordination",
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


@pytest.mark.parametrize("synced,notified,stopped", [(True, True, True), (False, True, False), (True, False, False)])
def test_api_worker_ends_after_durable_submission(monkeypatch, synced, notified, stopped):
    module = load_plugin()
    import json
    monkeypatch.setitem(sys.modules, "agentscope.middleware", types.SimpleNamespace(MiddlewareBase=object))
    monkeypatch.setitem(sys.modules, "agentscope.message", types.SimpleNamespace(Msg=lambda **kw: types.SimpleNamespace(**kw)))
    module._execution_task.set("task-1")
    middleware = module._sanitizer_factory(None, None)
    saved = []
    agent = types.SimpleNamespace(name="worker", state=types.SimpleNamespace(append_context=lambda *a: saved.append(a)))
    async def tool(**kwargs):
        yield types.SimpleNamespace(content=[types.SimpleNamespace(text=json.dumps({
            "ok": True, "synced": synced, "task": {"task_id": "task-1", "status": "submitted"},
            "notification": {"sent": notified}}))])
    async def model(**kwargs):
        yield {"model_called": True}
    async def run():
        call = types.SimpleNamespace(name="teamharness__taskflow", input={"action": "submit_task"})
        async for _ in middleware.on_acting(agent, {"tool_call": call}, tool): pass
        return [x async for x in middleware.on_reasoning(agent, {}, model)]
    result = asyncio.run(run())
    assert bool(saved) is stopped
    assert (isinstance(result[0], dict) and result[0].get("model_called") is True) is (not stopped)


@pytest.mark.parametrize("change,stopped", [({}, True), ({"synced": False}, False),
    ({"accepted": False}, False), ({"nodeStatus": "revision"}, False),
    ({"submissionId": "old"}, False), ({"taskId": "other"}, False),
    ({"dispatch": {"ok": False}}, False), ({"dispatch": {}}, False)])
def test_api_review_ends_only_after_matching_durable_acceptance(monkeypatch, change, stopped):
    import json
    module = load_plugin()
    monkeypatch.setitem(sys.modules, "agentscope.middleware", types.SimpleNamespace(MiddlewareBase=object))
    monkeypatch.setitem(sys.modules, "agentscope.message", types.SimpleNamespace(Msg=lambda **kw: types.SimpleNamespace(**kw)))
    token = module._review_task.set(("t1", "s1"))
    middleware = module._sanitizer_factory(None, None)
    saved = []
    agent = types.SimpleNamespace(name="leader", state=types.SimpleNamespace(append_context=lambda *a: saved.append(a)))
    receipt = {"ok": True, "synced": True, "accepted": True, "nodeStatus": "completed",
        "taskId": "t1", "submissionId": "s1", "dispatch": {"ok": True}, **change}
    async def tool(**kwargs):
        yield types.SimpleNamespace(content=[{"type": "text", "text": json.dumps(receipt)}])
    async def model(**kwargs):
        yield {"model_called": True}
    async def run():
        call = types.SimpleNamespace(name="teamharness__projectflow", input=json.dumps({"action": "accept_task_result"}))
        async for _ in middleware.on_acting(agent, {"tool_call": call}, tool): pass
        return [x async for x in middleware.on_reasoning(agent, {}, model)]
    try:
        result = asyncio.run(run())
        assert bool(saved) is stopped
        assert (isinstance(result[0], dict) and result[0].get("model_called") is True) is (not stopped)
    finally:
        module._review_task.reset(token)


def test_api_approval_projection_tracks_the_real_resolution(monkeypatch):
    module = load_plugin()
    async def run():
        future = asyncio.get_running_loop().create_future()
        pending = types.SimpleNamespace(request_id="r1", session_id="coordination-task:t:d", future=future,
            tool_name="Bash", extra={"tool_call": {"input": {"command": "echo probe"}}},
            severity="HIGH", result_summary="Review the command", created_at=1, timeout_seconds=300)
        async def get_pending(agent_id):
            assert agent_id == "default"
            return [pending] if not future.done() else []
        service = types.SimpleNamespace(get_all_pending_by_agent=get_pending)
        monkeypatch.setitem(sys.modules, "qwenpaw.app.approvals", types.SimpleNamespace(get_approval_service=lambda: service))
        first = await module._coordination_approvals()
        assert first[0]["status"] == "pending" and first[0]["expires_at"] == 301000
        future.set_result("approved")
        await asyncio.sleep(0)
        assert (await module._coordination_approvals())[0]["status"] == "approved"
    asyncio.run(run())


@pytest.mark.parametrize("command,executed", [
    ('wc -l report.md && rm -rf "$TMPDIR/check"', False),
    ('CLEAN=$(mktemp -d); python test.py; rm -rf "$CLEAN"', False),
    ('rm -rf "$TMPDIR/check"', True),
    ("export QWENPAW_SESSION_ID='matrix:!room:test' && rm -rf \"$TMPDIR/check\"", True),
    ('rm -rf /shared/other-task', True),
    ('python test.py', True),
])
def test_private_cleanup_preflight_never_executes_ambiguous_scripts(monkeypatch, command, executed):
    module = load_plugin()
    monkeypatch.setitem(sys.modules, "agentscope.middleware", types.SimpleNamespace(MiddlewareBase=object))
    monkeypatch.setitem(sys.modules, "agentscope.tool", types.SimpleNamespace(ToolResponse=lambda **kwargs: kwargs))
    calls = []
    async def handler(**kwargs):
        calls.append(kwargs)
        yield {"state": "success"}
    async def run():
        middleware = module._sanitizer_factory(None, None)
        return [event async for event in middleware.on_acting(None, {"tool_call": types.SimpleNamespace(name="execute_shell_command", input={"command": command})}, handler)]
    result = asyncio.run(run())
    assert bool(calls) is executed
    if not executed:
        assert result[0]["state"] == "error"
        assert "No command was executed" in result[0]["content"][0]["text"]


def test_compound_scratch_cleanup_is_rejected_before_requesting_approval(monkeypatch):
    module = load_plugin()
    monkeypatch.setitem(sys.modules, "agentscope.middleware", types.SimpleNamespace(MiddlewareBase=object))
    monkeypatch.setitem(sys.modules, "agentscope.permission", types.SimpleNamespace(
        PermissionBehavior=types.SimpleNamespace(DENY="deny"), PermissionDecision=lambda **kw: kw))
    async def approval(**kwargs):
        raise AssertionError("unnecessary approval was requested")
    async def run():
        middleware = module._sanitizer_factory(None, None)
        return await middleware.on_check_permission(None, {
            "tool_call": types.SimpleNamespace(name="execute_shell_command"),
            "tool_input": {"command": 'D="$TMPDIR/test"; rm -rf "$D"; mkdir -p "$D"'},
        }, approval)
    assert asyncio.run(run())["behavior"] == "deny"


def test_team_prompt_reads_packaged_contract():
    module = load_plugin()
    assert "TeamHarness" in module._team_prompt(None)


def test_coordination_short_circuit_uses_structured_keyword_message(monkeypatch):
    module = load_plugin()
    from types import SimpleNamespace
    class Msg:
        def __init__(self, *, name, content, role):
            assert content == [{"type": "text", "text": "NO_REPLY"}]
            self.role = role
    monkeypatch.setitem(sys.modules, "agentscope.message", SimpleNamespace(Msg=Msg))
    monkeypatch.setitem(sys.modules, "qwenpaw.runtime.hooks", SimpleNamespace(
        HookBase=object, HookResult=lambda **kwargs: SimpleNamespace(**kwargs),
        HookAction=SimpleNamespace(SHORT_CIRCUIT="short_circuit")))
    monkeypatch.setitem(sys.modules, "qwenpaw.runtime.phases", SimpleNamespace(
        Phase=SimpleNamespace(PRE_EXECUTE="pre_execute", FINALLY="finally")))
    async def skip(_ctx): return True
    monkeypatch.setattr(module, "_manager_report_context", skip)
    api = FakeApi()
    module._register_coordination_hook(api)
    assert api.calls[0][1][0].phase == "pre_execute"
    result = asyncio.run(api.calls[0][1][0].run(object()))
    assert result.action == "short_circuit" and result.payload.role == "assistant"


def test_middleware_preserves_requester_route_in_json_tool_input(monkeypatch):
    import json
    module = load_plugin()
    monkeypatch.setitem(sys.modules, "agentscope.middleware", types.SimpleNamespace(MiddlewareBase=object))
    call = types.SimpleNamespace(name="teamharness__message", input=json.dumps({"message": "handoff"}))
    route = {"channel": "matrix", "target_session": "!human:test"}
    async def handler(**kwargs):
        assert json.loads(kwargs["tool_call"].input)["requesterReplyRoute"] == route
        yield {"state": "success"}
    async def run():
        token = module._requester_route.set(route)
        try:
            return [item async for item in module._sanitizer_factory(None, None).on_acting(
                None, {"tool_call": call}, handler)]
        finally:
            module._requester_route.reset(token)
    assert asyncio.run(run()) == [{"state": "success"}]


def test_plugin_does_not_patch_qwenpaw_private_runtime():
    source = PLUGIN_PATH.read_text(encoding="utf-8")
    assert "QwenPawAgent._acting" not in source
    assert "legacy_mcp_client_to_driver" not in source
    assert "save_agent_config" not in source


def test_planning_preload_keeps_start_contract_and_references_full_manual_skill():
    from types import SimpleNamespace
    module = load_plugin()
    texts = []
    module._load_phase_skill(SimpleNamespace(inject_context=lambda text, **kw: texts.append(text)), "project-management")
    assert len(texts) == 1
    assert "start_task" in texts[0] and "parent_task_id" in texts[0]
    assert "start_project" not in texts[0]
    assert "Manual and Loop reference" not in texts[0]
    assert "read the complete skill" in texts[0]
    assert len(texts[0]) < len((module.ASSET_DIR / "skills/team/project-management/SKILL.md").read_text()) / 2


def test_task_cli_receipt_is_injected_only_for_final_success_and_current_task(monkeypatch):
    import json
    module = load_plugin()
    monkeypatch.setitem(sys.modules, "agentscope.middleware", types.SimpleNamespace(MiddlewareBase=object))
    class ToolResponse:
        def __init__(self, content, state="success"):
            self.content, self.state = content, state
    class TextBlock:
        def __init__(self, text): self.text = text
    monkeypatch.setitem(sys.modules, "agentscope.tool", types.SimpleNamespace(ToolResponse=ToolResponse))
    monkeypatch.setitem(sys.modules, "agentscope.message", types.SimpleNamespace(TextBlock=TextBlock))
    middleware = module._sanitizer_factory(None, None)
    token = module._execution_task.set("t1")
    payload = {"task":{"id":"t1","status":"submitted","submissionId":"s1"},"notification":{"status":"queued","eventId":"e1","recipient":"leader-a"}}
    async def run(tool="execute_shell_command", command='agt task submit t1 --summary "Done"', state="success", final=True):
        block = TextBlock(json.dumps(payload))
        response = ToolResponse([block], state) if final else types.SimpleNamespace(content=[block], state=state)
        async def handler(**_kwargs): yield response
        result = [item async for item in middleware.on_acting(None, {"tool_call":types.SimpleNamespace(name=tool,input={"command":command})}, handler)]
        return result[0].content
    try:
        assert len(asyncio.run(run(state="error"))) == 1
        assert len(asyncio.run(run(final=False))) == 1
        assert len(asyncio.run(run(command="cat result.json"))) == 1
        assert len(asyncio.run(run(command="agt task submit other --summary Done"))) == 1
        content = asyncio.run(run())
        assert len(content) == 2 and "[Task system receipt]" in content[0].text
        assert "leader-a" in content[0].text and "Do not resend" in content[0].text
        assert json.loads(content[1].text) == payload
        assert len(asyncio.run(run())) == 1  # Same receipt, same current conversation.
    finally:
        module._execution_task.reset(token)


def test_manager_binds_the_shared_intake_contract_as_a_prompt_section(monkeypatch):
    module = load_plugin()
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE","manager")
    monkeypatch.setenv("TEAMHARNESS_COORDINATION_URL","http://fixture")
    contract=(ROOT/"plugins/teamharness/skills/team/task-management/SKILL.md").read_text().partition("## Plan as Leader")[0].strip()
    assert contract in module._team_prompt(None)


def test_roster_injection_cannot_replace_the_shared_intake_protocol(monkeypatch):
    from types import SimpleNamespace
    module = load_plugin()
    monkeypatch.setenv("TEAMHARNESS_COORDINATION_URL","http://fixture")
    def read(path):
        return ({"teams":[{"name":"team","leaderName":"lead"}]} if path=="teams" else
                {"workers":[{"name":"lead","matrixUserID":"@lead:test"}]})
    monkeypatch.setattr(module,"_controller_json",read)
    injected=[]
    ctx=SimpleNamespace(inject_context=lambda text,**kwargs:injected.append(text))
    asyncio.run(module._prepare_manager_roster(ctx))
    assert injected and '"leader": "lead"' in injected[0]
    assert "spec:<complete original" not in injected[0]
    assert "protocol and registered" not in injected[0]
