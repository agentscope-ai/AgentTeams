#!/usr/bin/env python3
"""Integration tests for AgentTeams Task-Trace correlation.

Uses a real OTel SDK (TracerProvider + InMemorySpanExporter) together with
the TeamHarness MCP server to verify that ``agentteams.task.id`` and
``agentteams.project.id`` appear on exported spans at every stage of the task
lifecycle:  delegate → ack → tool calls → submit → post-submit.

Prerequisites:
    pip install opentelemetry-api opentelemetry-sdk
"""

from __future__ import annotations

import importlib.util
import json
import os
from contextlib import contextmanager
from pathlib import Path
import sys
from typing import Any

import pytest

# ---------------------------------------------------------------------------
# OTel SDK imports — skip entire module when SDK is absent
# ---------------------------------------------------------------------------
otel_sdk = pytest.importorskip("opentelemetry.sdk")

from opentelemetry import trace as trace_api
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

REPO_ROOT = Path(__file__).resolve().parents[3]
TRACE_MODULE_PATH = (
    REPO_ROOT / "plugins" / "teamharness" / "adapters" / "qwenpaw" / "task_trace.py"
)
SERVER_MODULE_PATH = REPO_ROOT / "plugins" / "teamharness" / "mcp" / "server.py"


def _load_module(name: str, path: Path):
    module_dir = str(path.parent)
    inserted = module_dir not in sys.path
    if inserted:
        sys.path.insert(0, module_dir)
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(module)
    finally:
        if inserted:
            sys.path.remove(module_dir)
    return module


# ===================================================================
# Fixtures
# ===================================================================


@pytest.fixture()
def otel_env(tmp_path: Path):
    """Set up a fresh OTel TracerProvider + InMemorySpanExporter and
    install the AgentTeamsTaskSpanProcessor on it.

    Uses the provider directly (via provider.get_tracer) instead of the
    global set_tracer_provider — avoids the "already set" warning and keeps
    tests isolated.
    """
    workspace = tmp_path / "workspaces" / "default"
    workspace.mkdir(parents=True)
    old_worker_name = os.environ.get("AGENTTEAMS_WORKER_NAME")
    os.environ["AGENTTEAMS_WORKER_NAME"] = "worker-a"

    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))

    trace_mod = _load_module("teamharness_trace", TRACE_MODULE_PATH)

    processor = trace_mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)
    provider.add_span_processor(processor)

    tracer = provider.get_tracer("agentteams.test")

    class Env:
        pass

    env = Env()
    env.provider = provider
    env.exporter = exporter
    env.tracer = tracer
    env.workspace = workspace
    env.trace_mod = trace_mod
    env.processor = processor

    yield env

    if old_worker_name is None:
        os.environ.pop("AGENTTEAMS_WORKER_NAME", None)
    else:
        os.environ["AGENTTEAMS_WORKER_NAME"] = old_worker_name
    provider.shutdown()


@pytest.fixture()
def mcp_server(otel_env):
    """Load the TeamHarness MCP server module with env configured for the
    test workspace.  Patches _sync_task / _pull_task to avoid real MinIO calls.
    """
    old_env = {}
    env_overrides = {
        "QWENPAW_WORKING_DIR": str(otel_env.workspace.parent.parent),
        "AGENTTEAMS_AGENT_ROLE": "worker",
        "AGENTTEAMS_WORKER_NAME": "worker-a",
    }
    env_clear = [
        "COPAW_WORKING_DIR",
        "TEAMHARNESS_RUNTIME_CONFIG",
        "TEAMHARNESS_SHARED_DIR",
        "AGENTTEAMS_SHARED_STORAGE_PREFIX",
        "AGENTTEAMS_MATRIX_URL",
        "AGENTTEAMS_WORKER_MATRIX_TOKEN",
        "AGENTTEAMS_MATRIX_USER_ID",
    ]
    for k, v in env_overrides.items():
        old_env[k] = os.environ.get(k)
        os.environ[k] = v
    for k in env_clear:
        old_env[k] = os.environ.get(k)
        os.environ.pop(k, None)

    server = _load_module("teamharness_mcp_server_integ", SERVER_MODULE_PATH)

    # Stub out MinIO sync operations — we only care about local meta.json changes
    server._sync_task = lambda *a, **kw: True
    server._pull_task = lambda *a, **kw: True

    yield server

    for k, v in old_env.items():
        if v is None:
            os.environ.pop(k, None)
        else:
            os.environ[k] = v


# ===================================================================
# Helpers
# ===================================================================


def _write_task(workspace: Path, task_id: str, meta: dict, *, write_project: bool = True) -> None:
    meta = dict(meta)
    if (
        meta.get("status") in {"assigned", "in_progress"}
        and not meta.get("assigned_to")
        and not meta.get("assignee")
    ):
        meta["assigned_to"] = "worker-a"
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True, exist_ok=True)
    (task_dir / "meta.json").write_text(
        json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8",
    )
    project_id = str(meta.get("project_id") or "")
    if not write_project or not project_id:
        return
    project_path = workspace / "shared" / "projects" / project_id / "meta.json"
    project_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        project = json.loads(project_path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        project = {}
    if not isinstance(project, dict):
        project = {}
    project["project_id"] = project_id
    tasks = project.get("tasks")
    if not isinstance(tasks, list):
        tasks = []
    tasks = [item for item in tasks if not isinstance(item, dict) or item.get("task_id") != task_id]
    tasks.append({"task_id": task_id, "status": meta.get("status")})
    project["tasks"] = tasks
    project_path.write_text(
        json.dumps(project, ensure_ascii=False, indent=2), encoding="utf-8",
    )


def _write_project(workspace: Path, project_id: str, meta: dict) -> None:
    project_dir = workspace / "shared" / "projects" / project_id
    project_dir.mkdir(parents=True, exist_ok=True)
    (project_dir / "meta.json").write_text(
        json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8",
    )


def _spans_with_task(exporter: InMemorySpanExporter, task_id: str):
    """Return exported spans that carry the given agentteams.task.id."""
    return [
        s for s in exporter.get_finished_spans()
        if s.attributes and s.attributes.get("agentteams.task.id") == task_id
    ]


def _spans_without_task(exporter: InMemorySpanExporter):
    """Return exported spans that do NOT carry agentteams.task.id."""
    return [
        s for s in exporter.get_finished_spans()
        if not s.attributes or "agentteams.task.id" not in s.attributes
    ]


def _mcp_call(server, name: str, arguments: dict) -> dict:
    """Call a TeamHarness MCP tool and return the parsed JSON payload."""
    result = server.call_tool(name, arguments)
    text = result["content"][0]["text"]
    return json.loads(text)


@contextmanager
def _room(trace_mod: Any, room_id: str):
    token = trace_mod.set_current_room(room_id)
    try:
        yield
    finally:
        trace_mod.reset_current_room(token)


# ===================================================================
# Integration tests
# ===================================================================


class TestBeforeAck:
    """Only the task turn entry span carries task attributes."""

    def test_health_span_has_no_task_attrs(self, otel_env) -> None:
        with otel_env.tracer.start_as_current_span("health-check"):
            pass  # simulates an Agent turn with no active task

        spans = otel_env.exporter.get_finished_spans()
        assert len(spans) == 1
        assert "agentteams.task.id" not in (spans[0].attributes or {})

    def test_assigned_task_entry_span_is_picked_up(self, otel_env) -> None:
        _write_task(otel_env.workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "assigned",
        })

        with _room(otel_env.trace_mod, "!r:m.org"):
            with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
                with otel_env.tracer.start_as_current_span("llm-call"):
                    pass

        tagged = _spans_with_task(otel_env.exporter, "t1")
        assert len(tagged) == 1
        assert tagged[0].name == "enter_ai_application_system"

    def test_entry_span_started_before_room_context_is_backfilled(self, otel_env) -> None:
        """Covers OTel wrapping outside the QwenPaw TeamHarness wrapper."""
        _write_task(otel_env.workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "assigned",
        })

        with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
            with _room(otel_env.trace_mod, "!r:m.org"):
                result = otel_env.trace_mod.tag_current_entry_span(otel_env.workspace)
                assert result["ok"] is True

        tagged = _spans_with_task(otel_env.exporter, "t1")
        assert len(tagged) == 1
        assert tagged[0].name == "enter_ai_application_system"
        assert tagged[0].attributes["agentteams.project.id"] == "p1"


class TestAckTask:
    """After ack_task changes meta.json to in_progress, spans get attributes."""

    def test_spans_after_ack_carry_task_attrs(self, otel_env, mcp_server) -> None:
        ws = otel_env.workspace
        project_id = "proj-integ"
        task_id = "task-integ"

        _write_project(ws, project_id, {
            "project_id": project_id,
            "tasks": [{"task_id": task_id, "status": "assigned"}],
        })
        _write_task(ws, task_id, {
            "task_id": task_id,
            "project_id": project_id,
            "room_id": "!room:m.org",
            "status": "assigned",
            "spec_path": f"shared/tasks/{task_id}/spec.md",
        })
        spec_dir = ws / "shared" / "tasks" / task_id
        (spec_dir / "spec.md").write_text("do something", encoding="utf-8")

        # Simulate: Agent's Entry span starts
        with _room(otel_env.trace_mod, "!room:m.org"):
            with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
                # Agent calls ack_task via MCP — this changes meta.json to in_progress
                with otel_env.tracer.start_as_current_span("mcp-tool-call"):
                    payload = _mcp_call(mcp_server, "taskflow", {
                        "workspaceDir": str(ws),
                        "action": "ack_task",
                        "role": "worker",
                        "payload": {"taskId": task_id},
                    })
                    assert payload["ok"] is True
                    assert payload["task"]["status"] == "in_progress"

                # Subsequent work stays as child spans; only the entry span is tagged.
                with otel_env.tracer.start_as_current_span("llm-call-post-ack"):
                    pass
                with otel_env.tracer.start_as_current_span("tool-call-filesync"):
                    pass

        # Verify: the entry span carries the assigned task context.
        tagged = _spans_with_task(otel_env.exporter, task_id)
        assert len(tagged) == 1
        assert tagged[0].name == "enter_ai_application_system"
        assert tagged[0].attributes["agentteams.project.id"] == project_id


class TestDuringWork:
    """The per-turn entry span carries task attributes during work."""

    def test_multiple_turn_entries_tagged(self, otel_env) -> None:
        ws = otel_env.workspace
        _write_task(ws, "active-task", {
            "task_id": "active-task",
            "project_id": "active-proj",
            "room_id": "!r:m.org",
            "status": "in_progress",
        })

        # Simulate 3 Agent turns
        for i in range(3):
            with _room(otel_env.trace_mod, "!r:m.org"):
                with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
                    with otel_env.tracer.start_as_current_span(f"llm-{i}"):
                        pass
                    with otel_env.tracer.start_as_current_span(f"tool-{i}"):
                        pass

        all_spans = otel_env.exporter.get_finished_spans()
        assert len(all_spans) == 9  # 3 turns × (entry + llm + tool)

        tagged = _spans_with_task(otel_env.exporter, "active-task")
        assert len(tagged) == 3
        for span in tagged:
            assert span.name == "enter_ai_application_system"
            assert span.attributes["agentteams.task.id"] == "active-task"
            assert span.attributes["agentteams.project.id"] == "active-proj"

    def test_nested_spans_tag_entry_only(self, otel_env) -> None:
        ws = otel_env.workspace
        _write_task(ws, "deep-task", {
            "task_id": "deep-task",
            "project_id": "deep-proj",
            "room_id": "!r:m.org",
            "status": "in_progress",
        })

        with _room(otel_env.trace_mod, "!r:m.org"):
            with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
                with otel_env.tracer.start_as_current_span("llm-call"):
                    with otel_env.tracer.start_as_current_span("http-request"):
                        with otel_env.tracer.start_as_current_span("dns-resolve"):
                            pass

        all_spans = otel_env.exporter.get_finished_spans()
        assert len(all_spans) == 4
        tagged = _spans_with_task(otel_env.exporter, "deep-task")
        assert len(tagged) == 1
        assert tagged[0].name == "enter_ai_application_system"


class TestSubmitTask:
    """The submit turn is still tagged; subsequent turns are not."""

    def test_submit_turn_tagged_and_next_turn_clean(self, otel_env, mcp_server) -> None:
        ws = otel_env.workspace
        project_id = "proj-submit"
        task_id = "task-submit"

        _write_project(ws, project_id, {
            "project_id": project_id,
            "tasks": [{"task_id": task_id, "status": "in_progress"}],
        })
        _write_task(ws, task_id, {
            "task_id": task_id,
            "project_id": project_id,
            "room_id": "!room:m.org",
            "status": "in_progress",
            "spec_path": f"shared/tasks/{task_id}/spec.md",
        })

        # --- Submit turn ---
        with _room(otel_env.trace_mod, "!room:m.org"):
            with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
                # Entry span is created BEFORE submit — task is still in_progress
                with otel_env.tracer.start_as_current_span("mcp-submit"):
                    submit_result = _mcp_call(mcp_server, "taskflow", {
                        "workspaceDir": str(ws),
                        "action": "submit_task",
                        "role": "worker",
                        "payload": {
                            "taskId": task_id,
                            "summary": "done",
                            "status": "SUCCESS",
                            "deliverables": [f"shared/tasks/{task_id}/output.md"],
                        },
                    })
                    assert submit_result["ok"] is True

        # Verify: submit turn entry span is tagged
        submit_spans = _spans_with_task(otel_env.exporter, task_id)
        assert len(submit_spans) >= 1
        entry_names = {s.name for s in submit_spans}
        assert "enter_ai_application_system" in entry_names

        otel_env.exporter.clear()

        # --- Next turn (after submit) ---
        with _room(otel_env.trace_mod, "!room:m.org"):
            with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
                with otel_env.tracer.start_as_current_span("idle-work"):
                    pass

        post_spans = otel_env.exporter.get_finished_spans()
        for span in post_spans:
            assert "agentteams.task.id" not in (span.attributes or {}), \
                f"span '{span.name}' should NOT have task attributes after submit"


class TestFullLifecycle:
    """End-to-end: delegate → ack → work → submit → idle."""

    def test_delegate_ack_work_submit_idle(self, otel_env, mcp_server) -> None:
        ws = otel_env.workspace
        project_id = "proj-e2e"
        task_id = "task-e2e"

        # --- Phase 1: Manager delegates task (leader role) ---
        _write_project(ws, project_id, {
            "project_id": project_id,
            "source_room_id": "!team:m.org",
            "tasks": [{"task_id": task_id, "status": "ready"}],
        })

        with otel_env.tracer.start_as_current_span("manager-delegate"):
            delegate_result = _mcp_call(mcp_server, "taskflow", {
                "workspaceDir": str(ws),
                "action": "delegate_task",
                "role": "leader",
                "payload": {
                    "projectId": project_id,
                    "taskId": task_id,
                    "roomId": "!worker-room:m.org",
                    "assignedTo": "worker-a",
                    "spec": "Build the feature",
                },
            })
            assert delegate_result["ok"] is True

        # At this point task status is "assigned" — no spans should be tagged
        delegate_spans = otel_env.exporter.get_finished_spans()
        for span in delegate_spans:
            assert "agentteams.task.id" not in (span.attributes or {})

        otel_env.exporter.clear()

        # --- Phase 2: Worker acks task ---
        with _room(otel_env.trace_mod, "!worker-room:m.org"):
            with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
                _mcp_call(mcp_server, "taskflow", {
                    "workspaceDir": str(ws),
                    "action": "ack_task",
                    "role": "worker",
                    "payload": {"taskId": task_id},
                })

                # Post-ack work
                with otel_env.tracer.start_as_current_span("post-ack-work"):
                    pass

        ack_tagged = _spans_with_task(otel_env.exporter, task_id)
        assert len(ack_tagged) == 1
        otel_env.exporter.clear()

        # --- Phase 3: Worker performs multiple work turns ---
        for i in range(3):
            with _room(otel_env.trace_mod, "!worker-room:m.org"):
                with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
                    with otel_env.tracer.start_as_current_span(f"llm-{i}"):
                        pass

        work_spans = otel_env.exporter.get_finished_spans()
        assert len(work_spans) == 6
        tagged_work_spans = _spans_with_task(otel_env.exporter, task_id)
        assert len(tagged_work_spans) == 3
        for span in tagged_work_spans:
            assert span.name == "enter_ai_application_system"
            assert span.attributes["agentteams.task.id"] == task_id
            assert span.attributes["agentteams.project.id"] == project_id
        otel_env.exporter.clear()

        # --- Phase 4: Worker submits ---
        with _room(otel_env.trace_mod, "!worker-room:m.org"):
            with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
                submit_result = _mcp_call(mcp_server, "taskflow", {
                    "workspaceDir": str(ws),
                    "action": "submit_task",
                    "role": "worker",
                    "payload": {
                        "taskId": task_id,
                        "summary": "feature built",
                        "status": "SUCCESS",
                        "deliverables": [f"shared/tasks/{task_id}/feature.py"],
                    },
                })
                assert submit_result["ok"] is True

        submit_tagged = _spans_with_task(otel_env.exporter, task_id)
        assert any(s.name == "enter_ai_application_system" for s in submit_tagged)
        otel_env.exporter.clear()

        # --- Phase 5: Post-submit idle --- no task attributes
        with _room(otel_env.trace_mod, "!worker-room:m.org"):
            with otel_env.tracer.start_as_current_span("enter_ai_application_system"):
                with otel_env.tracer.start_as_current_span("health-check"):
                    pass

        idle_spans = otel_env.exporter.get_finished_spans()
        assert len(idle_spans) == 2
        for span in idle_spans:
            assert "agentteams.task.id" not in (span.attributes or {})


class TestRegisterViaRealProvider:
    """Verify register_task_trace_processor works with the real OTel SDK."""

    def test_register_and_verify_attributes(self, tmp_path: Path) -> None:
        workspace = tmp_path / "workspaces" / "default"
        workspace.mkdir(parents=True)

        exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(exporter))
        old_worker_name = os.environ.get("AGENTTEAMS_WORKER_NAME")
        os.environ["AGENTTEAMS_WORKER_NAME"] = "worker-a"

        # Temporarily set global provider so register_task_trace_processor() can find it
        trace_api._TRACER_PROVIDER_SET_ONCE = trace_api.Once()
        trace_api.set_tracer_provider(provider)

        try:
            trace_mod = _load_module("teamharness_trace_reg", TRACE_MODULE_PATH)
            result = trace_mod.register_task_trace_processor(workspace, cache_ttl=0)
            assert result["ok"] is True

            tracer = provider.get_tracer("test")

            # No task yet — span should be clean
            with tracer.start_as_current_span("no-task"):
                pass
            spans = exporter.get_finished_spans()
            assert len(spans) == 1
            assert "agentteams.task.id" not in (spans[0].attributes or {})

            exporter.clear()

            # Write active task — next span should be tagged
            _write_task(workspace, "reg-task", {
                "task_id": "reg-task",
                "project_id": "reg-proj",
                "room_id": "!r:m.org",
                "status": "in_progress",
            })

            with _room(trace_mod, "!r:m.org"):
                with tracer.start_as_current_span("enter_ai_application_system"):
                    pass
            spans = exporter.get_finished_spans()
            assert len(spans) == 1
            assert spans[0].attributes["agentteams.task.id"] == "reg-task"
            assert spans[0].attributes["agentteams.project.id"] == "reg-proj"
        finally:
            if old_worker_name is None:
                os.environ.pop("AGENTTEAMS_WORKER_NAME", None)
            else:
                os.environ["AGENTTEAMS_WORKER_NAME"] = old_worker_name
            trace_api._TRACER_PROVIDER_SET_ONCE = trace_api.Once()
            provider.shutdown()
