#!/usr/bin/env python3
"""Tests for AgentTeams Task-Trace correlation (SpanProcessor approach).

Test layers:
  1. find_active_task        — filesystem scanning + room/worker filtering
  2. AgentTeamsTaskSpanProcessor — span attribute injection + current meta state
  3. register integration    — TracerProvider registration
  4. QwenPaw adapter wiring  — install_task_trace_processor()
  5. End-to-end scenario     — ack → tool calls → submit lifecycle
  6. Session isolation       — multi-room concurrent task scenarios
  7. Debug logging control   — AGENTTEAMS_TASK_TRACE_DEBUG toggle
"""

from __future__ import annotations

import importlib.util
import asyncio
import json
import types
import sys
from pathlib import Path
from typing import Any
from unittest.mock import MagicMock, patch

import pytest


REPO_ROOT = Path(__file__).resolve().parents[3]
TRACE_MODULE_PATH = (
    REPO_ROOT / "plugins" / "teamharness" / "adapters" / "qwenpaw" / "task_trace.py"
)
ADAPTER_PATH = REPO_ROOT / "plugins" / "teamharness" / "adapters" / "qwenpaw" / "plugin.py"


def _load_trace_module():
    spec = importlib.util.spec_from_file_location("teamharness_qwenpaw_task_trace", TRACE_MODULE_PATH)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _load_adapter_module():
    spec = importlib.util.spec_from_file_location("teamharness_qwenpaw_adapter", ADAPTER_PATH)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@pytest.fixture()
def workspace(tmp_path: Path) -> Path:
    ws = tmp_path / "workspaces" / "default"
    ws.mkdir(parents=True)
    return ws


@pytest.fixture(autouse=True)
def _reset_room_context(monkeypatch):
    """Ensure each test starts with no room context and no pending span."""
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "worker-a")
    mod = _load_trace_module()
    token = mod.set_current_room("")
    mod.clear_pending_entry_span()
    yield
    mod.reset_current_room(token)
    mod.clear_pending_entry_span()


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
    (task_dir / "meta.json").write_text(json.dumps(meta), encoding="utf-8")
    project_id = str(meta.get("project_id") or "")
    if not write_project or not project_id:
        return

    project_dir = workspace / "shared" / "projects" / project_id
    project_dir.mkdir(parents=True, exist_ok=True)
    project_path = project_dir / "meta.json"
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
    node = {"task_id": task_id, "status": meta.get("status")}
    tasks = [item for item in tasks if not isinstance(item, dict) or item.get("task_id") != task_id]
    tasks.append(node)
    project["tasks"] = tasks
    project_path.write_text(json.dumps(project), encoding="utf-8")


def _mock_span(name: str, *, recording: bool = True) -> MagicMock:
    span = MagicMock()
    span.is_recording.return_value = recording
    span.name = name
    return span


def _entry_span(*, recording: bool = True) -> MagicMock:
    return _mock_span("enter_ai_application_system", recording=recording)


def _start_entry_in_room(mod: Any, processor: Any, room_id: str = "!r:m.org") -> MagicMock:
    token = mod.set_current_room(room_id)
    try:
        span = _entry_span()
        processor.on_start(span)
        return span
    finally:
        mod.reset_current_room(token)


# ===================================================================
# Layer 1: find_active_task
# ===================================================================


class TestFindActiveTask:
    def test_returns_in_progress_task(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        result = mod.find_active_task(workspace, room_id="!r:m.org")
        assert result is not None
        assert result["task_id"] == "t1"
        assert result["project_id"] == "p1"

    def test_returns_assigned_task(self, workspace: Path) -> None:
        """ACK turn: assigned tasks are matched so entry span is tagged."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "assigned_to": "worker-a",
            "room_id": "!r:m.org", "status": "assigned",
        })
        result = mod.find_active_task(workspace, room_id="!r:m.org")
        assert result is not None
        assert result["task_id"] == "t1"
        assert result["status"] == "assigned"

    def test_ignores_submitted(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "submitted",
        })
        assert mod.find_active_task(workspace) is None

    def test_no_tasks_dir(self, workspace: Path) -> None:
        mod = _load_trace_module()
        assert mod.find_active_task(workspace) is None

    def test_empty_tasks_dir(self, workspace: Path) -> None:
        mod = _load_trace_module()
        (workspace / "shared" / "tasks").mkdir(parents=True)
        assert mod.find_active_task(workspace) is None

    def test_corrupt_json_skipped(self, workspace: Path) -> None:
        mod = _load_trace_module()
        task_dir = workspace / "shared" / "tasks" / "bad"
        task_dir.mkdir(parents=True)
        (task_dir / "meta.json").write_text("{invalid", encoding="utf-8")
        assert mod.find_active_task(workspace) is None

    def test_multiple_tasks_returns_matching_in_progress(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "submitted",
        })
        _write_task(workspace, "t2", {
            "task_id": "t2", "project_id": "p2",
            "room_id": "!r2:m.org", "status": "in_progress",
        })
        result = mod.find_active_task(workspace, room_id="!r2:m.org")
        assert result is not None
        assert result["task_id"] == "t2"

    def test_worker_name_without_room_context_does_not_guess(self, workspace: Path, monkeypatch) -> None:
        mod = _load_trace_module()
        monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "worker-a")
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "assigned_to": "worker-a",
            "room_id": "!room-A:m.org", "status": "in_progress",
        })
        _write_task(workspace, "t2", {
            "task_id": "t2", "project_id": "p2",
            "assigned_to": "worker-b",
            "room_id": "!room-B:m.org", "status": "in_progress",
        })
        result = mod.find_active_task(workspace)
        assert result is None

    def test_no_room_or_worker_match_does_not_fallback(self, workspace: Path, monkeypatch) -> None:
        mod = _load_trace_module()
        monkeypatch.delenv("AGENTTEAMS_WORKER_NAME", raising=False)
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "assigned_to": "worker-a",
            "room_id": "!room-A:m.org", "status": "in_progress",
        })
        assert mod.find_active_task(workspace) is None

    def test_room_id_exact_match_takes_priority(self, workspace: Path) -> None:
        """When room_id is specified, it wins over worker-name matching."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "assigned_to": "worker-a",
            "room_id": "!room-A:m.org", "status": "in_progress",
        })
        _write_task(workspace, "t2", {
            "task_id": "t2", "project_id": "p2",
            "assigned_to": "worker-a",
            "room_id": "!room-B:m.org", "status": "in_progress",
        })
        result = mod.find_active_task(workspace, room_id="!room-B:m.org")
        assert result is not None
        assert result["task_id"] == "t2"

    def test_room_id_prefixes_are_canonicalized(self, workspace: Path) -> None:
        """Trace context and task meta may use matrix:, room:, or bare room ids."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "assigned_to": "worker-a",
            "room_id": "room:!room-A:m.org", "status": "in_progress",
        })

        bare = mod.find_active_task(workspace, room_id="!room-A:m.org")
        matrix = mod.find_active_task(workspace, room_id="matrix:!room-A:m.org")

        assert bare is not None
        assert bare["task_id"] == "t1"
        assert matrix is not None
        assert matrix["task_id"] == "t1"

    def test_matrix_assignee_matches_worker_name(self, workspace: Path, monkeypatch) -> None:
        """Matrix ids such as @worker-a:server match AGENTTEAMS_WORKER_NAME."""
        mod = _load_trace_module()
        monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "worker-a")
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "assigned_to": "@worker-a:m.org",
            "room_id": "!room-A:m.org", "status": "in_progress",
        })

        result = mod.find_active_task(workspace, room_id="!room-A:m.org")

        assert result is not None
        assert result["task_id"] == "t1"

    def test_room_id_ignores_non_matching_tasks(self, workspace: Path) -> None:
        """With room_id set, tasks from other rooms are not matched."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!room-X:m.org", "status": "in_progress",
        })
        result = mod.find_active_task(workspace, room_id="!room-Y:m.org")
        assert result is None

    def test_room_id_with_no_active_in_room(self, workspace: Path) -> None:
        """When the specified room has no active task, no other room is used."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!room-A:m.org", "status": "submitted",
        })
        _write_task(workspace, "t2", {
            "task_id": "t2", "project_id": "p2",
            "room_id": "!room-B:m.org", "status": "in_progress",
        })
        result = mod.find_active_task(workspace, room_id="!room-A:m.org")
        assert result is None

    def test_project_graph_is_not_required_for_room_task_match(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "orphan", {
            "task_id": "orphan", "project_id": "p1",
            "room_id": "!room-A:m.org", "status": "in_progress",
        }, write_project=False)
        result = mod.find_active_task(workspace, room_id="!room-A:m.org")
        assert result is not None
        assert result["task_id"] == "orphan"


# ===================================================================
# Layer 2: AgentTeamsTaskSpanProcessor
# ===================================================================


class TestAgentTeamsTaskSpanProcessor:
    def test_sets_attributes_on_entry_span_only(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        entry = _start_entry_in_room(mod, processor, "!r:m.org")
        entry.set_attribute.assert_any_call("agentteams.task.id", "t1")
        entry.set_attribute.assert_any_call("agentteams.project.id", "p1")

        child = _mock_span("chat qwen3-max")
        processor.on_start(child)
        child.set_attribute.assert_not_called()

    def test_skips_non_recording_span(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)
        span = _entry_span(recording=False)

        processor.on_start(span)

        span.set_attribute.assert_not_called()

    def test_no_attributes_when_no_active_task(self, workspace: Path) -> None:
        mod = _load_trace_module()
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)
        span = _entry_span()

        # No room set → span stashed but not tagged
        processor.on_start(span)
        span.set_attribute.assert_not_called()

    def test_room_project_ambiguity_sets_no_attributes(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        _write_task(workspace, "t2", {
            "task_id": "t2", "project_id": "p2",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        span = _start_entry_in_room(mod, processor, "!r:m.org")

        span.set_attribute.assert_not_called()

    def test_multiple_active_tasks_in_project_sets_project_only(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        _write_task(workspace, "t2", {
            "task_id": "t2", "project_id": "p1",
            "room_id": "!r:m.org", "status": "assigned",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        span = _start_entry_in_room(mod, processor, "!r:m.org")

        span.set_attribute.assert_called_once_with("agentteams.project.id", "p1")

    def test_entry_span_rereads_current_meta(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace)

        span1 = _start_entry_in_room(mod, processor, "!r:m.org")
        assert span1.set_attribute.call_count == 2

        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "submitted",
        })

        span2 = _start_entry_in_room(mod, processor, "!r:m.org")
        span2.set_attribute.assert_called_once_with("agentteams.project.id", "p1")

    def test_invalidate_cache_is_noop_with_meta_reread(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace)

        span1 = _start_entry_in_room(mod, processor, "!r:m.org")
        assert span1.set_attribute.call_count == 2

        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "submitted",
        })
        processor.invalidate_cache()

        span2 = _start_entry_in_room(mod, processor, "!r:m.org")
        span2.set_attribute.assert_called_once_with("agentteams.project.id", "p1")

    def test_on_end_logs_entry_span_attributes(self, workspace: Path) -> None:
        mod = _load_trace_module()
        mod.set_trace_debug(True)
        try:
            processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)
            span = _entry_span()
            span.attributes = {
                "agentteams.task.id": "t1",
                "agentteams.project.id": "p1",
            }
            processor.on_end(span)  # should not raise
        finally:
            mod.set_trace_debug(False)

    def test_on_end_noop_when_debug_disabled(self, workspace: Path) -> None:
        """on_end skips attribute reading when debug is off."""
        mod = _load_trace_module()
        mod.set_trace_debug(False)
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)
        span = _entry_span()
        span.attributes = {"agentteams.task.id": "t1"}
        processor.on_end(span)  # should not raise, should not access attributes

    def test_force_flush_and_shutdown(self, workspace: Path) -> None:
        mod = _load_trace_module()
        processor = mod.AgentTeamsTaskSpanProcessor(workspace)
        assert processor.force_flush() is True
        processor.shutdown()  # should not raise

# ===================================================================
# Layer 3: register_task_trace_processor
# ===================================================================


class TestRegisterTaskTraceProcessor:
    def test_no_otel_sdk_returns_failure(self, workspace: Path, monkeypatch) -> None:
        mod = _load_trace_module()
        original_import = __builtins__.__import__ if hasattr(__builtins__, '__import__') else __import__

        def mock_import(name, *args, **kwargs):
            if name in ("opentelemetry", "opentelemetry.sdk.trace"):
                raise ImportError("mocked")
            return original_import(name, *args, **kwargs)

        with patch("builtins.__import__", side_effect=mock_import):
            result = mod.register_task_trace_processor(workspace)
        assert result["ok"] is False
        assert "unavailable" in result["reason"]

    def test_registers_on_real_tracer_provider(self, workspace: Path) -> None:
        mod = _load_trace_module()

        otel_sdk_trace = types.ModuleType("opentelemetry.sdk.trace")
        TracerProviderClass = type("TracerProvider", (), {"add_span_processor": MagicMock()})
        otel_sdk_trace.TracerProvider = TracerProviderClass

        provider = TracerProviderClass()
        provider._real_provider = None

        otel_trace = types.ModuleType("opentelemetry.trace")
        otel_trace.get_tracer_provider = lambda: provider

        with patch.dict("sys.modules", {
            "opentelemetry": types.ModuleType("opentelemetry"),
            "opentelemetry.trace": otel_trace,
            "opentelemetry.sdk": types.ModuleType("opentelemetry.sdk"),
            "opentelemetry.sdk.trace": otel_sdk_trace,
        }):
            result = mod.register_task_trace_processor(workspace)

        assert result["ok"] is True
        provider.add_span_processor.assert_called_once()
        processor = provider.add_span_processor.call_args[0][0]
        assert type(processor).__name__ == "AgentTeamsTaskSpanProcessor"


# ===================================================================
# Layer 4: QwenPaw adapter install_task_trace_processor
# ===================================================================


class TestAdapterInstallTaskTraceProcessor:
    def test_extracts_matrix_room_from_request_context(self) -> None:
        mod = _load_adapter_module()
        assert mod._room_id_from_request_context({
            "channel": "matrix",
            "session_id": "matrix:!room-A:m.org",
            "user_id": "!room-A:m.org",
        }) == "!room-A:m.org"

    def test_extracts_room_from_channel_meta_first(self) -> None:
        mod = _load_adapter_module()
        assert mod._room_id_from_request_context({
            "session_id": "matrix:!session-room:m.org",
            "channel_meta": {"room_id": "!meta-room:m.org"},
        }) == "!meta-room:m.org"

    def test_context_wrapper_does_not_wrap_acting(self, monkeypatch) -> None:
        mod = _load_adapter_module()

        class FakeQwenPawAgent:
            async def __call__(self):
                return "called"

            async def reply(self):
                return "replied"

            async def _acting(self, tool_call):
                return {"ok": True}

        qwenpaw = types.ModuleType("qwenpaw")
        agents = types.ModuleType("qwenpaw.agents")
        react_agent = types.ModuleType("qwenpaw.agents.react_agent")
        react_agent.QwenPawAgent = FakeQwenPawAgent
        monkeypatch.setitem(sys.modules, "qwenpaw", qwenpaw)
        monkeypatch.setitem(sys.modules, "qwenpaw.agents", agents)
        monkeypatch.setitem(sys.modules, "qwenpaw.agents.react_agent", react_agent)

        trace_module = types.SimpleNamespace(
            set_current_room=MagicMock(return_value=object()),
            reset_current_room=MagicMock(),
        )

        original_acting = FakeQwenPawAgent._acting
        result = mod.install_task_trace_context_wrapper(trace_module)
        assert result["ok"] is True
        assert FakeQwenPawAgent._acting is original_acting

    def test_context_wrapper_tags_pending_entry_span(self, tmp_path: Path, monkeypatch) -> None:
        """Wrapper prefers tag_pending_entry_span when a pending span exists."""
        qwenpaw_dir = tmp_path / ".qwenpaw"
        (qwenpaw_dir / "workspaces" / "default").mkdir(parents=True)
        monkeypatch.setenv("QWENPAW_WORKING_DIR", str(qwenpaw_dir))
        monkeypatch.delenv("COPAW_WORKING_DIR", raising=False)

        mod = _load_adapter_module()

        class FakeQwenPawAgent:
            def __init__(self):
                self._request_context = {"session_id": "matrix:!room-A:m.org"}

            async def __call__(self):
                return "called"

            async def reply(self):
                return "replied"

        qwenpaw = types.ModuleType("qwenpaw")
        agents = types.ModuleType("qwenpaw.agents")
        react_agent = types.ModuleType("qwenpaw.agents.react_agent")
        react_agent.QwenPawAgent = FakeQwenPawAgent
        monkeypatch.setitem(sys.modules, "qwenpaw", qwenpaw)
        monkeypatch.setitem(sys.modules, "qwenpaw.agents", agents)
        monkeypatch.setitem(sys.modules, "qwenpaw.agents.react_agent", react_agent)

        pending_span = _entry_span()
        trace_module = types.SimpleNamespace(
            set_current_room=MagicMock(return_value=object()),
            reset_current_room=MagicMock(),
            tag_current_entry_span=MagicMock(return_value={"ok": True}),
            tag_pending_entry_span=MagicMock(return_value={"ok": True}),
            get_pending_entry_span=MagicMock(return_value=pending_span),
            register_task_trace_processor=MagicMock(return_value={"ok": True}),
        )

        result = mod.install_task_trace_context_wrapper(trace_module)
        assert result["ok"] is True

        assert asyncio.run(FakeQwenPawAgent()()) == "called"

        trace_module.set_current_room.assert_called_once_with("!room-A:m.org")
        trace_module.tag_pending_entry_span.assert_called_once_with(
            qwenpaw_dir / "workspaces" / "default",
        )
        # tag_current_entry_span should NOT be called when pending was used
        trace_module.tag_current_entry_span.assert_not_called()
        trace_module.reset_current_room.assert_called_once()

    def test_context_wrapper_refreshes_tasks_before_tagging_pending_span(self, tmp_path: Path, monkeypatch) -> None:
        qwenpaw_dir = tmp_path / ".qwenpaw"
        shared_dir = tmp_path / "teams" / "devteam11" / "shared"
        (qwenpaw_dir / "workspaces" / "default").mkdir(parents=True)
        shared_dir.mkdir(parents=True)
        monkeypatch.setenv("QWENPAW_WORKING_DIR", str(qwenpaw_dir))
        monkeypatch.setenv("TEAMHARNESS_SHARED_DIR", str(shared_dir))
        monkeypatch.setenv("AGENTTEAMS_SHARED_STORAGE_PREFIX", "teams/devteam11/shared")
        monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "worker-a")
        monkeypatch.setenv("AGENTTEAMS_RUNTIME", "k8s")
        monkeypatch.delenv("COPAW_WORKING_DIR", raising=False)

        mod = _load_adapter_module()
        events: list[tuple[Any, ...]] = []

        class FakeFileSync:
            def __init__(self, **kwargs):
                events.append(("init", kwargs))

            def mirror_prefix(self, remote_prefix, local_dir):
                events.append(("mirror", remote_prefix, Path(local_dir)))

        qwenpaw_worker = types.ModuleType("qwenpaw_worker")
        sync_module = types.ModuleType("qwenpaw_worker.sync")
        sync_module.FileSync = FakeFileSync
        monkeypatch.setitem(sys.modules, "qwenpaw_worker", qwenpaw_worker)
        monkeypatch.setitem(sys.modules, "qwenpaw_worker.sync", sync_module)

        class FakeQwenPawAgent:
            def __init__(self):
                self._request_context = {"session_id": "matrix:!room-A:m.org"}

            async def __call__(self):
                events.append(("call",))
                return "called"

            async def reply(self):
                return "replied"

        qwenpaw = types.ModuleType("qwenpaw")
        agents = types.ModuleType("qwenpaw.agents")
        react_agent = types.ModuleType("qwenpaw.agents.react_agent")
        react_agent.QwenPawAgent = FakeQwenPawAgent
        monkeypatch.setitem(sys.modules, "qwenpaw", qwenpaw)
        monkeypatch.setitem(sys.modules, "qwenpaw.agents", agents)
        monkeypatch.setitem(sys.modules, "qwenpaw.agents.react_agent", react_agent)

        trace_module = types.SimpleNamespace(
            set_current_room=MagicMock(side_effect=lambda room: events.append(("set_room", room)) or object()),
            reset_current_room=MagicMock(side_effect=lambda token: events.append(("reset",))),
            tag_current_entry_span=MagicMock(return_value={"ok": True}),
            tag_pending_entry_span=MagicMock(side_effect=lambda workspace: events.append(("tag_pending", Path(workspace))) or {"ok": True}),
            get_pending_entry_span=MagicMock(return_value=_entry_span()),
            register_task_trace_processor=MagicMock(return_value={"ok": True}),
        )

        result = mod.install_task_trace_context_wrapper(trace_module)
        assert result["ok"] is True

        assert asyncio.run(FakeQwenPawAgent()()) == "called"

        assert events[:4] == [
            ("set_room", "!room-A:m.org"),
            ("init", events[1][1]),
            ("mirror", "teams/devteam11/shared/tasks", shared_dir / "tasks"),
            ("tag_pending", qwenpaw_dir / "workspaces" / "default"),
        ]
        assert events[-2:] == [("call",), ("reset",)]

    def test_context_wrapper_refresh_failure_does_not_block_turn(self, tmp_path: Path, monkeypatch) -> None:
        qwenpaw_dir = tmp_path / ".qwenpaw"
        shared_dir = tmp_path / "shared"
        (qwenpaw_dir / "workspaces" / "default").mkdir(parents=True)
        shared_dir.mkdir(parents=True)
        monkeypatch.setenv("QWENPAW_WORKING_DIR", str(qwenpaw_dir))
        monkeypatch.setenv("TEAMHARNESS_SHARED_DIR", str(shared_dir))
        monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "worker-a")
        monkeypatch.delenv("COPAW_WORKING_DIR", raising=False)

        mod = _load_adapter_module()

        class FakeFileSync:
            def __init__(self, **kwargs):
                pass

            def mirror_prefix(self, remote_prefix, local_dir):
                raise RuntimeError("storage unavailable")

        qwenpaw_worker = types.ModuleType("qwenpaw_worker")
        sync_module = types.ModuleType("qwenpaw_worker.sync")
        sync_module.FileSync = FakeFileSync
        monkeypatch.setitem(sys.modules, "qwenpaw_worker", qwenpaw_worker)
        monkeypatch.setitem(sys.modules, "qwenpaw_worker.sync", sync_module)

        class FakeQwenPawAgent:
            def __init__(self):
                self._request_context = {"session_id": "matrix:!room-A:m.org"}

            async def __call__(self):
                return "called"

            async def reply(self):
                return "replied"

        qwenpaw = types.ModuleType("qwenpaw")
        agents = types.ModuleType("qwenpaw.agents")
        react_agent = types.ModuleType("qwenpaw.agents.react_agent")
        react_agent.QwenPawAgent = FakeQwenPawAgent
        monkeypatch.setitem(sys.modules, "qwenpaw", qwenpaw)
        monkeypatch.setitem(sys.modules, "qwenpaw.agents", agents)
        monkeypatch.setitem(sys.modules, "qwenpaw.agents.react_agent", react_agent)

        trace_module = types.SimpleNamespace(
            set_current_room=MagicMock(return_value=object()),
            reset_current_room=MagicMock(),
            tag_current_entry_span=MagicMock(return_value={"ok": True}),
            tag_pending_entry_span=MagicMock(return_value={"ok": True}),
            get_pending_entry_span=MagicMock(return_value=_entry_span()),
            register_task_trace_processor=MagicMock(return_value={"ok": True}),
        )

        result = mod.install_task_trace_context_wrapper(trace_module)
        assert result["ok"] is True

        assert asyncio.run(FakeQwenPawAgent()()) == "called"
        trace_module.tag_pending_entry_span.assert_called_once_with(
            qwenpaw_dir / "workspaces" / "default",
        )

    def test_context_wrapper_falls_back_to_tag_current(self, tmp_path: Path, monkeypatch) -> None:
        """When no pending span exists, wrapper falls back to tag_current_entry_span."""
        qwenpaw_dir = tmp_path / ".qwenpaw"
        (qwenpaw_dir / "workspaces" / "default").mkdir(parents=True)
        monkeypatch.setenv("QWENPAW_WORKING_DIR", str(qwenpaw_dir))
        monkeypatch.delenv("COPAW_WORKING_DIR", raising=False)

        mod = _load_adapter_module()

        class FakeQwenPawAgent:
            def __init__(self):
                self._request_context = {"session_id": "matrix:!room-A:m.org"}

            async def __call__(self):
                return "called"

            async def reply(self):
                return "replied"

        qwenpaw = types.ModuleType("qwenpaw")
        agents = types.ModuleType("qwenpaw.agents")
        react_agent = types.ModuleType("qwenpaw.agents.react_agent")
        react_agent.QwenPawAgent = FakeQwenPawAgent
        monkeypatch.setitem(sys.modules, "qwenpaw", qwenpaw)
        monkeypatch.setitem(sys.modules, "qwenpaw.agents", agents)
        monkeypatch.setitem(sys.modules, "qwenpaw.agents.react_agent", react_agent)

        trace_module = types.SimpleNamespace(
            set_current_room=MagicMock(return_value=object()),
            reset_current_room=MagicMock(),
            tag_current_entry_span=MagicMock(return_value={"ok": True}),
            tag_pending_entry_span=MagicMock(return_value={"ok": True}),
            get_pending_entry_span=MagicMock(return_value=None),
            register_task_trace_processor=MagicMock(return_value={"ok": True}),
        )

        result = mod.install_task_trace_context_wrapper(trace_module)
        assert result["ok"] is True

        assert asyncio.run(FakeQwenPawAgent()()) == "called"

        trace_module.tag_pending_entry_span.assert_not_called()
        trace_module.tag_current_entry_span.assert_called_once_with(
            qwenpaw_dir / "workspaces" / "default",
        )

    def test_returns_failure_when_no_workspace(self, monkeypatch) -> None:
        monkeypatch.delenv("QWENPAW_WORKING_DIR", raising=False)
        monkeypatch.delenv("COPAW_WORKING_DIR", raising=False)
        monkeypatch.delenv("TEAMHARNESS_RUNTIME_CONFIG", raising=False)
        mod = _load_adapter_module()
        result = mod.install_task_trace_processor()
        assert result["ok"] is False

    def test_loads_trace_module_and_registers(self, tmp_path: Path, monkeypatch) -> None:
        qwenpaw_dir = tmp_path / ".qwenpaw"
        workspace = qwenpaw_dir / "workspaces" / "default"
        workspace.mkdir(parents=True)
        monkeypatch.setenv("QWENPAW_WORKING_DIR", str(qwenpaw_dir))
        monkeypatch.delenv("COPAW_WORKING_DIR", raising=False)
        monkeypatch.delenv("TEAMHARNESS_RUNTIME_CONFIG", raising=False)

        mod = _load_adapter_module()

        with patch.object(mod, "PLUGIN_DIR", REPO_ROOT / "plugins" / "teamharness" / "adapters" / "qwenpaw"):
            result = mod.install_task_trace_processor()

        assert isinstance(result, dict)
        assert "ok" in result


# ===================================================================
# Layer 5: End-to-end task lifecycle scenario
# ===================================================================


class TestEndToEndLifecycle:
    """Simulates ack → work → submit and verifies span attributes at each stage."""

    def test_full_lifecycle(self, workspace: Path) -> None:
        mod = _load_trace_module()
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        # --- No task on disk ---
        span_before = _entry_span()
        processor.on_start(span_before)
        span_before.set_attribute.assert_not_called()

        # --- in_progress: entry span tagged, child spans not ---
        _write_task(workspace, "task-e2e", {
            "task_id": "task-e2e",
            "project_id": "proj-e2e",
            "room_id": "!room:m.org",
            "status": "in_progress",
        })
        span_working = _start_entry_in_room(mod, processor, "!room:m.org")
        span_working.set_attribute.assert_any_call("agentteams.task.id", "task-e2e")
        span_working.set_attribute.assert_any_call("agentteams.project.id", "proj-e2e")

        for _ in range(3):
            span_tool = _mock_span("execute_tool taskflow")
            processor.on_start(span_tool)
            span_tool.set_attribute.assert_not_called()

        # --- After submit: no attributes on entry span ---
        _write_task(workspace, "task-e2e", {
            "task_id": "task-e2e",
            "project_id": "proj-e2e",
            "room_id": "!room:m.org",
            "status": "submitted",
        })
        span_after = _start_entry_in_room(mod, processor, "!room:m.org")
        span_after.set_attribute.assert_called_once_with("agentteams.project.id", "proj-e2e")

    def test_ack_turn_tags_entry_span_while_assigned(self, workspace: Path) -> None:
        """ACK turn: entry span is tagged while task is still assigned."""
        mod = _load_trace_module()
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        _write_task(workspace, "task-ack", {
            "task_id": "task-ack",
            "project_id": "proj-ack",
            "room_id": "!room:m.org",
            "status": "assigned",
        })

        span_ack = _start_entry_in_room(mod, processor, "!room:m.org")
        span_ack.set_attribute.assert_any_call("agentteams.task.id", "task-ack")
        span_ack.set_attribute.assert_any_call("agentteams.project.id", "proj-ack")

    def test_submit_turn_still_tagged(self, workspace: Path) -> None:
        """Submit turn entry span is tagged while task is still in_progress."""
        mod = _load_trace_module()
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        _write_task(workspace, "task-sub", {
            "task_id": "task-sub",
            "project_id": "proj-sub",
            "room_id": "!room:m.org",
            "status": "in_progress",
        })

        span_submit_turn = _start_entry_in_room(mod, processor, "!room:m.org")
        span_submit_turn.set_attribute.assert_any_call("agentteams.task.id", "task-sub")

        _write_task(workspace, "task-sub", {
            "task_id": "task-sub",
            "project_id": "proj-sub",
            "room_id": "!room:m.org",
            "status": "submitted",
        })
        span_next = _start_entry_in_room(mod, processor, "!room:m.org")
        span_next.set_attribute.assert_called_once_with("agentteams.project.id", "proj-sub")


# ===================================================================
# Layer 6: Session isolation (multi-room)
# ===================================================================


class TestSessionIsolation:
    """Verifies that set_current_room correctly routes tasks."""

    def test_room_context_routes_to_correct_task(self, workspace: Path) -> None:
        """Two tasks in different rooms; room context determines which is matched."""
        mod = _load_trace_module()
        _write_task(workspace, "task-room-a", {
            "task_id": "task-room-a", "project_id": "proj-1",
            "room_id": "!room-A:m.org", "status": "in_progress",
        })
        _write_task(workspace, "task-room-b", {
            "task_id": "task-room-b", "project_id": "proj-2",
            "room_id": "!room-B:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        # Turn in room A → should tag with task-room-a
        token_a = mod.set_current_room("!room-A:m.org")
        span_a = _entry_span()
        processor.on_start(span_a)
        span_a.set_attribute.assert_any_call("agentteams.task.id", "task-room-a")
        mod.reset_current_room(token_a)

        # Turn in room B → should tag with task-room-b
        token_b = mod.set_current_room("!room-B:m.org")
        span_b = _entry_span()
        processor.on_start(span_b)
        span_b.set_attribute.assert_any_call("agentteams.task.id", "task-room-b")
        mod.reset_current_room(token_b)

    def test_no_room_context_does_not_fallback_to_any_active(self, workspace: Path) -> None:
        """Without room context, processor stashes but does not tag."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!room:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        span = _entry_span()
        processor.on_start(span)
        span.set_attribute.assert_not_called()
        # Span is stashed for deferred tagging
        assert mod.get_pending_entry_span() is span

    def test_room_submitted_does_not_cross_tag(self, workspace: Path) -> None:
        """Room A task submitted; room B task active; room A turn should not tag B."""
        mod = _load_trace_module()
        _write_task(workspace, "task-a", {
            "task_id": "task-a", "project_id": "proj-1",
            "room_id": "!room-A:m.org", "status": "submitted",
        })
        _write_task(workspace, "task-b", {
            "task_id": "task-b", "project_id": "proj-2",
            "room_id": "!room-B:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        # Room A turn: task-a is submitted, so task-b from another room must
        # not be used.
        token = mod.set_current_room("!room-A:m.org")
        span = _entry_span()
        processor.on_start(span)
        span.set_attribute.assert_called_once_with("agentteams.project.id", "proj-1")
        mod.reset_current_room(token)

    def test_per_room_context_independence(self, workspace: Path) -> None:
        """Room context routes each turn independently."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!room-A:m.org", "status": "in_progress",
        })
        _write_task(workspace, "t2", {
            "task_id": "t2", "project_id": "p2",
            "room_id": "!room-B:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace)

        # Resolve room-A
        token_a = mod.set_current_room("!room-A:m.org")
        span_a = _entry_span()
        processor.on_start(span_a)
        span_a.set_attribute.assert_any_call("agentteams.task.id", "t1")
        mod.reset_current_room(token_a)

        # Room-B should resolve independently
        token_b = mod.set_current_room("!room-B:m.org")
        span_b = _entry_span()
        processor.on_start(span_b)
        span_b.set_attribute.assert_any_call("agentteams.task.id", "t2")
        mod.reset_current_room(token_b)

    def test_submitted_meta_stops_tagging_without_invalidation(self, workspace: Path) -> None:
        """Current meta status is authoritative on the next entry span."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!room-A:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace)

        # Resolve active task
        token = mod.set_current_room("!room-A:m.org")
        span = _entry_span()
        processor.on_start(span)
        span.set_attribute.assert_any_call("agentteams.task.id", "t1")
        mod.reset_current_room(token)

        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!room-A:m.org", "status": "submitted",
        })

        token = mod.set_current_room("!room-A:m.org")
        span2 = _entry_span()
        processor.on_start(span2)
        span2.set_attribute.assert_called_once_with("agentteams.project.id", "p1")
        mod.reset_current_room(token)


# ===================================================================
# Layer 7: Deferred entry span tagging (pending span mechanism)
# ===================================================================


class TestDeferredEntrySpanTagging:
    """Verifies the pending-entry-span mechanism that fixes the room
    context timing issue.

    Real-world flow:
      1. Instrumentation creates ``enter_ai_application_system`` span
         → on_start fires, room is NOT set yet → span is stashed
      2. QwenPawAgent.__call__ wrapper sets room context
      3. Wrapper calls tag_pending_entry_span() → stashed span gets tagged
    """

    def test_on_start_stashes_entry_span_when_room_not_set(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)
        span = _entry_span()

        # Room NOT set — span should be stashed, not tagged
        processor.on_start(span)
        span.set_attribute.assert_not_called()
        assert mod.get_pending_entry_span() is span

    def test_tag_pending_entry_span_tags_and_clears(self, workspace: Path) -> None:
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)
        span = _entry_span()

        # on_start without room → stash
        processor.on_start(span)
        assert mod.get_pending_entry_span() is span

        # Now set room and tag the pending span
        token = mod.set_current_room("!r:m.org")
        try:
            result = mod.tag_pending_entry_span(workspace, room_id="!r:m.org")
        finally:
            mod.reset_current_room(token)

        assert result["ok"] is True
        span.set_attribute.assert_any_call("agentteams.task.id", "t1")
        span.set_attribute.assert_any_call("agentteams.project.id", "p1")
        # Pending span should be cleared
        assert mod.get_pending_entry_span() is None

    def test_tag_current_entry_span_prefers_pending(self, workspace: Path) -> None:
        """tag_current_entry_span should find and tag the pending span
        even when get_current_span() would return a different (non-entry) span."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)
        entry = _entry_span()

        # on_start without room → stash
        processor.on_start(entry)

        # Set room, then call tag_current_entry_span (the wrapper's path)
        token = mod.set_current_room("!r:m.org")
        try:
            result = mod.tag_current_entry_span(workspace, room_id="!r:m.org")
        finally:
            mod.reset_current_room(token)

        assert result["ok"] is True
        entry.set_attribute.assert_any_call("agentteams.task.id", "t1")
        assert mod.get_pending_entry_span() is None

    def test_no_pending_span_returns_failure(self, workspace: Path) -> None:
        mod = _load_trace_module()
        result = mod.tag_pending_entry_span(workspace)
        assert result["ok"] is False
        assert "no pending" in result["reason"]

    def test_clear_pending_entry_span(self, workspace: Path) -> None:
        mod = _load_trace_module()
        processor = mod.AgentTeamsTaskSpanProcessor(workspace)
        span = _entry_span()
        processor.on_start(span)
        assert mod.get_pending_entry_span() is span
        mod.clear_pending_entry_span()
        assert mod.get_pending_entry_span() is None

    def test_on_start_with_room_set_does_not_stash(self, workspace: Path) -> None:
        """When room is already known, on_start tags immediately (no stash)."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        token = mod.set_current_room("!r:m.org")
        try:
            span = _entry_span()
            processor.on_start(span)
        finally:
            mod.reset_current_room(token)

        span.set_attribute.assert_any_call("agentteams.task.id", "t1")
        assert mod.get_pending_entry_span() is None

    def test_full_deferred_lifecycle(self, workspace: Path) -> None:
        """Simulate the real QwenPaw call flow end-to-end."""
        mod = _load_trace_module()
        _write_task(workspace, "t1", {
            "task_id": "t1", "project_id": "p1",
            "room_id": "!r:m.org", "status": "in_progress",
        })
        processor = mod.AgentTeamsTaskSpanProcessor(workspace, cache_ttl=0)

        # Step 1: Instrumentation creates entry span (no room yet)
        entry = _entry_span()
        processor.on_start(entry)
        entry.set_attribute.assert_not_called()
        assert mod.get_pending_entry_span() is entry

        # Step 2: Inner spans are created (agent_step, etc.)
        inner = _mock_span("agent_step")
        processor.on_start(inner)
        inner.set_attribute.assert_not_called()

        # Step 3: Wrapper sets room and tags pending span
        token = mod.set_current_room("!r:m.org")
        try:
            result = mod.tag_pending_entry_span(workspace, room_id="!r:m.org")
        finally:
            mod.reset_current_room(token)

        assert result["ok"] is True
        entry.set_attribute.assert_any_call("agentteams.task.id", "t1")
        entry.set_attribute.assert_any_call("agentteams.project.id", "p1")


# ===================================================================
# Layer 8: Debug logging control
# ===================================================================


class TestDebugLogging:
    def test_debug_disabled_by_default(self) -> None:
        mod = _load_trace_module()
        # Module level _DEBUG_ENABLED should respect env var at import time
        # For tests, just verify set_trace_debug works
        mod.set_trace_debug(False)
        assert mod._DEBUG_ENABLED is False

    def test_set_trace_debug_enables_logging(self) -> None:
        mod = _load_trace_module()
        mod.set_trace_debug(True)
        assert mod._DEBUG_ENABLED is True
        mod.set_trace_debug(False)

    def test_trace_log_noop_when_disabled(self, capsys, workspace: Path) -> None:
        mod = _load_trace_module()
        mod.set_trace_debug(False)
        mod._trace_log("this should not appear %s", "hello")
        captured = capsys.readouterr()
        assert "this should not appear" not in captured.err

    def test_trace_log_emits_when_enabled(self, capsys, workspace: Path) -> None:
        mod = _load_trace_module()
        mod.set_trace_debug(True)
        try:
            mod._trace_log("visible message %s", "world")
            captured = capsys.readouterr()
            assert "visible message world" in captured.err
        finally:
            mod.set_trace_debug(False)
