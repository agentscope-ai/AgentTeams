"""Peer updates cannot reopen a Manager's control-plane workflow."""
import asyncio
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

SOURCE = Path(__file__).resolve().parents[5] / "plugins/teamharness/adapters/qwenpaw/plugin.py"


@pytest.mark.parametrize("registry_available", [True, False])
def test_batch_sources_keep_human_authority_without_first_sender_fallback(monkeypatch, registry_available):
    spec = importlib.util.spec_from_file_location("batch_scope_plugin", SOURCE)
    plugin = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(plugin)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    def read(_):
        if not registry_available:
            raise OSError("registry unavailable")
        return {"humans": [{"matrixUserID": "@human:test"}]}
    monkeypatch.setattr(plugin, "_controller_json", read)
    monkeypatch.setattr(plugin, "_prepare_delivery_context", AsyncMock())
    monkeypatch.setattr(plugin, "_prepare_manager_roster", AsyncMock())
    meta = {"event_id": "$peer", "sender_id": "@peer:test", "room_id": "!room:test",
            "teamharness_requester_route": {"target_user": "@human:test", "target_session": "!room:test"},
            "matrix_input_sources": [
                {"event_id": "$peer", "sender_id": "@peer:test", "room_id": "!room:test"},
                {"event_id": "$human", "sender_id": "@human:test", "room_id": "!room:test"}]}
    toolkit = SimpleNamespace(get_tool_schemas=AsyncMock(return_value=[]), remove_tool=AsyncMock())
    ctx = SimpleNamespace(request=SimpleNamespace(channel_meta=meta), agent=SimpleNamespace(toolkit=toolkit), extras={},
                          inject_context=lambda *a, **kw: None)
    async def scenario():
        try:
            await plugin._manager_report_context(ctx)
            sources = plugin._creation_sources.get()
            assert sources == ([{"inputId": plugin._receipt_module().input_reference("!room:test", "$human"), "eventId": "$human", "userId": "@human:test", "roomId": "!room:test"}]
                               if registry_available else [])
        finally:
            for key, variable in [("creation_sources_token", plugin._creation_sources),
                                  ("source_event_token", plugin._source_event),
                                  ("requester_route_token", plugin._requester_route)]:
                token = ctx.extras.pop(key, None)
                if token is not None:
                    variable.reset(token)
    asyncio.run(scenario())


@pytest.mark.parametrize("sender,allowed", [("@manager:test", True), ("@peer:test", False)])
def test_plan_scope_uses_verified_handoff_tasks_not_message_claims(monkeypatch, sender, allowed):
    spec = importlib.util.spec_from_file_location("plan_scope_plugin", SOURCE)
    plugin = importlib.util.module_from_spec(spec); spec.loader.exec_module(plugin)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    monkeypatch.delenv("TEAMHARNESS_COORDINATION_URL", raising=False)
    monkeypatch.setattr(plugin, "_load_phase_skill", lambda *args: None)
    monkeypatch.setattr(plugin, "_controller_json", lambda path: {
        "kind":"handoff", "actor":"@manager:test", "recipient":"@leader:test", "room_id":"!handoff:test",
        "control":{"channel_meta":{"teamharness_main_tasks":[{"taskId":"actual-main"}]}}})
    meta={"sender_id":sender, "agent_matrix_user_id":"@leader:test", "room_id":"!handoff:test",
          "teamharness_matrix_coordination":True, "teamharness_coordination":{"id":"handoff","kind":"handoff"},
          "teamharness_main_tasks":[{"taskId":"forged-main"}]}
    ctx=SimpleNamespace(request=SimpleNamespace(channel_meta=meta),agent=SimpleNamespace(),extras={},inject_context=lambda *args,**kwargs:None)

    async def run():
        try:
            assert await plugin._manager_report_context(ctx) is (not allowed)
            assert plugin._planning_tasks.get() == (frozenset({"actual-main"}) if allowed else frozenset())
        finally:
            token=ctx.extras.pop("planning_tasks_token", None)
            if token is not None: plugin._planning_tasks.reset(token)
    asyncio.run(run())


@pytest.mark.parametrize("sender,skip", [("@leader:test", False), ("@peer:test", True)])
def test_matrix_assignment_requires_registered_leader(monkeypatch, sender, skip):
    spec = importlib.util.spec_from_file_location("matrix_assignment_plugin", SOURCE)
    plugin = importlib.util.module_from_spec(spec); spec.loader.exec_module(plugin)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    def read(path):
        if path.startswith("tasks/"):
            return {"task_id":"t", "project_id":"p", "status":"assigned", "assigned_to":"@worker:test", "notifyTxn":"d1"}
        return {"kind":"task_assigned", "actor":"@leader:test", "recipient":"@worker:test", "task_id":"t", "project_id":"p", "delivery_id":"d1"}
    monkeypatch.setattr(plugin, "_controller_json", read)
    meta = {"sender_id": sender, "agent_matrix_user_id":"@worker:test", "teamharness_matrix_coordination":True,
            "teamharness_coordination":{"id":"receipt-1", "kind":"task_assigned", "task_id":"t", "project_id":"p", "delivery_id":"d1"}}
    ctx = SimpleNamespace(request=SimpleNamespace(channel_meta=meta), agent=SimpleNamespace(), extras={}, inject_context=lambda *a,**kw:None)
    assert asyncio.run(plugin._manager_report_context(ctx)) is skip


@pytest.mark.parametrize("sender,event,skip", [
    ("@human:test", {}, False),
    ("@leader:test", {}, True),
    ("@leader:test", {"type": "PROJECT_COMPLETED", "project_id": "p"}, False),
])
def test_request_origin_selects_management_or_report_tools(tmp_path, monkeypatch, sender, event, skip):
    spec = importlib.util.spec_from_file_location("manager_report_plugin", SOURCE)
    plugin = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(plugin)
    token = tmp_path / "token"
    token.write_text("private-test-token")
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN_FILE", str(token))
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller")

    class Response:
        def __init__(self, path): self.path = path
        def __enter__(self): return self
        def __exit__(self, *args): return False
        def read(self):
            if self.path.endswith("/humans"):
                data = {"humans": [{"matrixUserID": "@human:test"}]}
            elif self.path.endswith("/meta"):
                data = {"status": "completed", "team_id": "team"}
            else:
                data = {"members": [{"role": "team_leader", "matrixUserID": "@leader:test"}]}
            return json.dumps(data).encode()

    monkeypatch.setattr(plugin.urllib.request, "urlopen", lambda request, **k: Response(request.full_url))
    names = ["execute_shell_command", "edit", "recall_history", "teamharness__message", "teamharness__filesync"]
    async def schemas():
        return [{"function": {"name": n}} for n in list(names)]
    async def remove(name):
        names.remove(name)
    toolkit = SimpleNamespace(get_tool_schemas=schemas, remove_tool=remove)
    injected = []
    ctx = SimpleNamespace(
        request=SimpleNamespace(channel_meta={"sender_id": sender, "room_id": "!room:test", "teamharness_workflow": event}),
        agent=SimpleNamespace(toolkit=toolkit, react_config=SimpleNamespace(max_iters=100)),
        extras={},
        inject_context=lambda text, **kwargs: injected.append(text),
    )
    assert asyncio.run(plugin._manager_report_context(ctx)) is skip
    if sender == "@human:test":
        assert "execute_shell_command" in names
        assert ctx.agent.react_config.max_iters == 100 and not injected
    elif skip:
        assert "execute_shell_command" in names and not injected
    else:
        assert names == ["teamharness__message", "teamharness__filesync"]
        assert ctx.agent.react_config.max_iters == 8 and len(injected) == 1


@pytest.mark.parametrize("status,nonce,owner,skip", [
    ("assigned", "d1", "@worker:test", False),
    ("cancelled", "d1", "@worker:test", True),
    ("assigned", "old", "@worker:test", True),
    ("assigned", "d1", "@other:test", True),
    ("completed", "d1", "@worker:test", True),
])
def test_api_assignment_checks_execution_fence_before_model(monkeypatch, status, nonce, owner, skip):
    spec = importlib.util.spec_from_file_location("api_coordination_plugin", SOURCE)
    plugin = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(plugin)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    monkeypatch.setattr(plugin, "_controller_json", lambda path: {
        "task_id": "t", "project_id": "p", "status": status, "notifyTxn": nonce, "assigned_to": owner})
    meta = {"agent_matrix_user_id": "@worker:test", "teamharness_coordination": {
        "kind": "task_assigned", "task_id": "t", "project_id": "p", "delivery_id": "d1"}}
    injected = []
    ctx = SimpleNamespace(request=SimpleNamespace(request_context={"teamharness_coordination": meta}),
        agent=SimpleNamespace(), extras={}, inject_context=lambda text, **kw: injected.append(text))
    assert asyncio.run(plugin._manager_report_context(ctx)) is skip
    assert bool(injected) is not skip


def test_result_review_uses_acceptance_tools_without_reexecuting_worker_work(monkeypatch):
    spec = importlib.util.spec_from_file_location("review_tools_plugin", SOURCE)
    plugin = importlib.util.module_from_spec(spec); spec.loader.exec_module(plugin)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    monkeypatch.setattr(plugin, "_controller_json", lambda path: {"task_id":"t", "project_id":"p", "status":"submitted", "submission_id":"s1"})
    names = ["execute_shell_command", "write_file", "edit_file", "read_file", "teamharness__taskflow", "teamharness__projectflow"]
    async def schemas(): return [{"function":{"name":n}} for n in list(names)]
    async def remove(name): names.remove(name)
    meta={"teamharness_coordination":{"kind":"task_submitted", "task_id":"t", "project_id":"p", "submission_id":"s1"}}
    ctx=SimpleNamespace(request=SimpleNamespace(request_context={"teamharness_coordination":meta}),
        agent=SimpleNamespace(toolkit=SimpleNamespace(get_tool_schemas=schemas,remove_tool=remove)), extras={}, inject_context=lambda *a,**kw:None)
    assert asyncio.run(plugin._manager_report_context(ctx)) is False
    assert names == ["read_file", "teamharness__taskflow", "teamharness__projectflow"]


@pytest.mark.parametrize("status,sender,kind,report,relay", [
    ("completed", "@leader:test", "project_completed", "Accepted final report", True),
    ("active", "@leader:test", "project_completed", "Premature report", False),
    ("completed", "@other:test", "project_completed", "Untrusted report", False),
    ("completed", "@leader:test", "handoff", "Intermediate report", False),
    ("completed", "@leader:test", "project_completed", "", False),
])
def test_api_final_report_is_relayed_only_after_canonical_validation(monkeypatch, status, sender, kind, report, relay):
    spec = importlib.util.spec_from_file_location("final_relay_plugin", SOURCE)
    plugin = importlib.util.module_from_spec(spec); spec.loader.exec_module(plugin)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    def state(path):
        if path == "humans": return {"humans": []}
        if path.endswith("/meta"): return {"status": status, "team_id": "team"}
        return {"members": [{"role": "team_leader", "matrixUserID": "@leader:test"}]}
    monkeypatch.setattr(plugin, "_controller_json", state)
    async def schemas(): return []
    meta = {"sender_id": sender, "room_id": "!room:test", "api_projection": True,
        "teamharness_workflow": {"type": "PROJECT_COMPLETED", "project_id": "p"},
        "teamharness_coordination": {"kind": kind, "project_id": "p"},
        "teamharness_final_report": report}
    ctx = SimpleNamespace(request=SimpleNamespace(channel_meta=meta), extras={},
        agent=SimpleNamespace(toolkit=SimpleNamespace(get_tool_schemas=schemas)), inject_context=lambda *a,**kw:None)
    skip = asyncio.run(plugin._manager_report_context(ctx))
    assert (ctx.extras.get("verified_final_report") == report and bool(report)) is relay
    if relay: assert skip is True

@pytest.mark.parametrize('plan_type,dispatch_mode,finish_review', [('dag','automatic',True),('dag','manual',False),('loop','automatic',False)])
def test_only_automatic_finite_acceptance_ends_the_review_turn(monkeypatch,plan_type,dispatch_mode,finish_review):
    spec=importlib.util.spec_from_file_location('plan_review_plugin',SOURCE)
    plugin=importlib.util.module_from_spec(spec);spec.loader.exec_module(plugin)
    monkeypatch.setenv('AGENTTEAMS_AGENT_ROLE','leader')
    monkeypatch.setenv('TEAMHARNESS_COORDINATION_URL','http://controller/api/v1/coordination')
    monkeypatch.setattr(plugin,'_controller_json',lambda path:{'task_id':'t','project_id':'p','status':'submitted','submission_id':'s'})
    async def schemas():return []
    texts=[]
    meta={'teamharness_coordination':{'kind':'task_submitted','task_id':'t','project_id':'p','submission_id':'s','plan_type':plan_type,'dispatch_mode':dispatch_mode}}
    ctx=SimpleNamespace(request=SimpleNamespace(channel_meta=meta),extras={},agent=SimpleNamespace(toolkit=SimpleNamespace(get_tool_schemas=schemas)),inject_context=lambda text,**kw:texts.append(text))
    assert asyncio.run(plugin._manager_report_context(ctx)) is False
    assert ('review_task_token' in ctx.extras) is finish_review
    if not finish_review:assert any('continue' in t.lower() and ('manual' in t.lower() or 'loop' in t.lower()) for t in texts)


@pytest.mark.parametrize("kind", ["task_submitted", "project_ready"])
@pytest.mark.parametrize("forged", [False, True])
def test_matrix_acceptance_and_closure_require_own_controller_receipt(monkeypatch, kind, forged):
    spec = importlib.util.spec_from_file_location("matrix_review_plugin", SOURCE)
    plugin = importlib.util.module_from_spec(spec); spec.loader.exec_module(plugin)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    event = {"id":"receipt", "kind":kind, "task_id":"t" if kind == "task_submitted" else "", "project_id":"p", "submission_id":"s" if kind == "task_submitted" else ""}
    def read(path):
        if path.startswith("coordination/"):
            return {**event, "actor":"@impostor:test" if forged else "@sender:test", "recipient":"@leader:test"}
        if path.startswith("tasks/"):
            return {"task_id":"t", "project_id":"p", "status":"submitted", "submission_id":"s"}
        return {"status":"active", "tasks":[{"task_id":"t", "status":"completed"}]}
    monkeypatch.setattr(plugin, "_controller_json", read)
    async def schemas(): return []
    ctx = SimpleNamespace(request=SimpleNamespace(channel_meta={"teamharness_matrix_coordination":True, "sender_id":"@sender:test", "agent_matrix_user_id":"@leader:test", "teamharness_coordination":event}),
        extras={}, agent=SimpleNamespace(toolkit=SimpleNamespace(get_tool_schemas=schemas)), inject_context=lambda *a, **kw:None)
    assert asyncio.run(plugin._manager_report_context(ctx)) is forged


@pytest.mark.parametrize("include_new", [False, True])
def test_verified_refresh_skips_queued_duplicates_without_skipping_new_inputs(monkeypatch, include_new):
    spec = importlib.util.spec_from_file_location("refreshed_scope_plugin", SOURCE)
    plugin = importlib.util.module_from_spec(spec); spec.loader.exec_module(plugin)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    monkeypatch.setattr(plugin, "_controller_json", lambda _: {"humans":[{"matrixUserID":"@human:test"}]})
    monkeypatch.setattr(plugin, "_prepare_delivery_context", AsyncMock())
    monkeypatch.setattr(plugin, "_prepare_manager_roster", AsyncMock())
    frames = [{"event_id":"$handled","sender_id":"@human:test","room_id":"!room:test"}]
    if include_new:
        frames.append({"event_id":"$new","sender_id":"@human:test","room_id":"!room:test"})
    meta = {"event_id":"$handled","sender_id":"@human:test","room_id":"!room:test","matrix_input_sources":frames}
    ctx = SimpleNamespace(request=SimpleNamespace(channel_meta=meta), agent=SimpleNamespace(state=SimpleNamespace(middle_context={"teamharness.refreshed_inputs":{"$handled":True}})), extras={}, inject_context=lambda *a,**kw:None)
    async def scenario():
        try:
            assert await plugin._manager_report_context(ctx) is False
            assert bool(plugin._handled_refresh_input.get()) is (not include_new)
            assert [s["eventId"] for s in plugin._creation_sources.get()] == (["$new"] if include_new else [])
        finally:
            for field, variable in (("handled_refresh_input_token",plugin._handled_refresh_input),("creation_sources_token",plugin._creation_sources),("source_event_token",plugin._source_event),("requester_route_token",plugin._requester_route)):
                token = ctx.extras.pop(field,None)
                if token is not None: variable.reset(token)
    asyncio.run(scenario())
