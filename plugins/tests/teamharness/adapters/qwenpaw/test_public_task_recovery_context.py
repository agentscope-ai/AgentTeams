"""Startup preparation uses public Task operations only after execution fencing."""
import asyncio
import importlib.util
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest


@pytest.mark.parametrize("changed", [None, "process_id", "instance_id", "generation", "state", "missing"])
def test_recovered_managed_task_preparation_requires_current_execution(monkeypatch, changed):
    source = Path(__file__).resolve().parents[5] / "plugins/teamharness/adapters/qwenpaw/plugin.py"
    spec = importlib.util.spec_from_file_location("public_recovery_plugin", source)
    plugin = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(plugin)

    class Conflict(Exception): pass

    task = {"task_id":"execution-1", "task_plan_id":"plan", "main_task_id":"main", "notifyTxn":"assignment-1",
            "execution":{"process_id":"process-1", "instance_id":"instance-1", "generation":3, "state":"active"}}
    if changed and changed != "missing":
        task["execution"][changed] = "stale" if changed != "generation" else 2

    class Recovery:
        process = "process-1"
        def current(self, task_id, team):
            assert (task_id, team) == ("execution-1", "team")
            return None if changed == "missing" else task

    monkeypatch.setitem(sys.modules, "task_recovery", SimpleNamespace(Recovery=Recovery, RecoveryConflict=Conflict))
    prepare = AsyncMock()
    monkeypatch.setattr(plugin, "_prepare_public_task_event", prepare)
    monkeypatch.setattr(plugin, "_load_phase_skill", lambda *args: pytest.fail("managed recovery loaded legacy instructions"))
    ctx = SimpleNamespace(agent=SimpleNamespace(), extras={}, request=SimpleNamespace(channel_meta={
        "task_execution":{"task_id":"execution-1", "team_id":"team", "instance_id":"instance-1", "generation":3}}))
    if changed:
        with pytest.raises(Conflict): asyncio.run(plugin._manager_report_context(ctx))
        prepare.assert_not_awaited()
    else:
        assert asyncio.run(plugin._manager_report_context(ctx)) is False
        prepare.assert_awaited_once_with(ctx, {"kind":"task_assigned", "task_id":"execution-1", "main_task_id":"main", "project_id":"", "delivery_id":"assignment-1"})
    assert not ctx.extras
