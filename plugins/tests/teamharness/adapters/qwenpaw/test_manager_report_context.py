"""Peer updates cannot reopen a Manager's control-plane workflow."""
import asyncio
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace

import pytest

SOURCE = Path(__file__).resolve().parents[5] / "plugins/teamharness/adapters/qwenpaw/plugin.py"


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
