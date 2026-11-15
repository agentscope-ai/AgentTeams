"""An ACK authorizes cleanup only inside that mutable assignment's outputs."""
import asyncio
import importlib.util
import json
from pathlib import Path
import sys
from types import SimpleNamespace

import pytest

SOURCE = Path(__file__).resolve().parents[5] / "plugins/teamharness/adapters/qwenpaw/plugin.py"


@pytest.fixture
def cleanup(tmp_path, monkeypatch):
    pytest.importorskip("qwenpaw_worker.cleanup_policy")
    spec = importlib.util.spec_from_file_location("task_cleanup_plugin", SOURCE)
    plugin = importlib.util.module_from_spec(spec); spec.loader.exec_module(plugin)
    monkeypatch.setitem(sys.modules, "agentscope.middleware", SimpleNamespace(MiddlewareBase=object))
    monkeypatch.setitem(sys.modules, "agentscope.permission", SimpleNamespace(
        PermissionBehavior=SimpleNamespace(ALLOW="allow", DENY="deny"),
        PermissionDecision=lambda behavior,message: SimpleNamespace(behavior=behavior,message=message)))
    root = tmp_path / "tasks/t1"; root.mkdir(parents=True)
    state = {"task_id": "t1", "status": "in_progress", "assigned_to": "@worker:test", "notifyTxn": "d1"}
    monkeypatch.setattr(plugin, "_controller_json", lambda path: dict(state))
    middleware = plugin._sanitizer_factory(None, None)
    async def ack(**kw):
        yield SimpleNamespace(content=[{"type": "text", "text": json.dumps({
            "ok": True, "synced": True, "task": state,
            "taskDirectory": str(root)})}])
    async def run_ack():
        call = SimpleNamespace(name="teamharness__taskflow", input=json.dumps({"action":"ack_task"}))
        async for _ in middleware.on_acting(None, {"tool_call": call}, ack): pass
    asyncio.run(run_ack())
    async def decision(command):
        async def native(**kw): return SimpleNamespace(behavior="ask")
        return await middleware.on_check_permission(None, {"tool_call":SimpleNamespace(name="execute_shell_command"), "tool_input":{"command":command}}, native)
    return plugin, root, state, lambda cmd: asyncio.run(decision(cmd)).behavior


def test_ack_allows_exact_own_task_output_cleanup(cleanup):
    _, root, _, decision = cleanup
    (root / "cache").mkdir()
    assert decision('rm -rf "'+str(root / "cache")+'"') == "allow"


@pytest.mark.parametrize("target", ["meta.json", "spec.md", "result.md", "base", "inputs", "."])
def test_task_control_and_inputs_do_not_gain_cleanup_permission(cleanup, target):
    _, root, _, decision = cleanup
    assert decision("rm -rf "+str(root / target)) != "allow"


@pytest.mark.parametrize("change", ["cancelled", "new-owner", "new-delegation"])
def test_cleanup_rechecks_current_assignment(cleanup, change):
    _, root, state, decision = cleanup
    if change == "cancelled": state["status"] = "cancelled"
    elif change == "new-owner": state["assigned_to"] = "@other:test"
    else: state["notifyTxn"] = "d2"
    assert decision("rm -rf "+str(root / "cache")) != "allow"


def test_other_task_and_symlink_escape_do_not_gain_permission(cleanup):
    _, root, _, decision = cleanup
    other = root.parent / "t2"; other.mkdir()
    (root / "link").symlink_to(other, target_is_directory=True)
    assert decision("rm -rf "+str(other / "cache")) != "allow"
    assert decision("rm -rf "+str(root / "link/cache")) != "allow"


def test_tmp_wildcard_is_returned_to_agent_without_human_approval(cleanup):
    _, _, _, decision = cleanup
    assert decision('rm -rf "${TMPDIR:-/tmp}"/old_check_*') == "deny"


def test_exact_own_output_move_is_allowed_but_other_tasks_are_protected(cleanup):
    _, root, _, decision = cleanup
    assert decision(f'mv "{root}/output.new" "{root}/output.txt"') == "allow"
    assert decision(f'mv "{root}/output.new" "{root}/meta.json"') != "allow"
    assert decision(f'mv "{root}/output.new" "{root}"') != "allow"
    assert decision(f'mv "{root}/output.new" "{root.parent}/other/output.txt"') != "allow"
    assert decision(f'mv "{root}/output.new" "{root}/output.txt"; touch /tmp/outside') != "allow"


def test_compound_output_move_returns_guidance_instead_of_requesting_approval(cleanup):
    _, root, _, decision = cleanup
    assert decision(f'cd "{root}" && cat output.txt > output.new && mv output.new output.txt') == "deny"
