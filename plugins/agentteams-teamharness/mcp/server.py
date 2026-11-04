#!/usr/bin/env python3
"""TeamHarness MCP stdio server entry point."""

from __future__ import annotations

import json
import os
import subprocess  # noqa: F401 — kept as an attribute: tests patch server.subprocess.run
import sys
from pathlib import Path
from typing import Any

# W1-a3 (issue #1451 P6): the fragment groups are imported as the `mcp.*`
# package below, so the package root (the directory containing mcp/) must be
# on sys.path. plugin.yaml starts this file as a script (`python mcp/server.py`,
# whose sys.path[0] is mcp/ itself, not its parent) and the test harnesses
# exec it via spec_from_file_location — neither puts the package root on the
# path, so bootstrap it here, before any mcp.* import.
_PKG_ROOT = Path(__file__).resolve().parents[1]  # the directory containing mcp/
if str(_PKG_ROOT) not in sys.path:
    sys.path.insert(0, str(_PKG_ROOT))

# W1-a2 (issue #1451 P6): every tool-implementation group is now a real
# module; this file is assembly only. The real modules are registered with
# the refs layer so call-time lookups (and the PEP 562 __getattr__ below)
# can fall back to the historical flat namespace, and refs.bind keeps this
# module's globals as the primary resolution target so test monkeypatches
# of server-module attributes stay visible at every proxy call site.
from mcp.artifact import filesync, publish, sync

from mcp.common import config, constants, errors, identity, io, payload, paths, refs, tool_schemas

from mcp.lifecycle import signal_events, taskflow, transitions

from mcp.project import flow, state

from mcp.room import matrix, mention, notify, rooms

from mcp.transport import message, route

from mcp import message_tool

from mcp import roomflow_tool

refs.register_modules((
    constants, tool_schemas, paths, config, io, payload, errors, identity,
    transitions, signal_events, taskflow, state, flow, filesync, publish,
    sync, mention, matrix, rooms, notify, route, message, message_tool,
    roomflow_tool,
))

# Every test loads its own server instance (function-scoped) and the bind
# runs at that instance's import time, so during each test the resolution
# target is the instance under test; monkeypatches installed on that
# instance stay visible at every proxy call site.
refs.bind(globals())

from mcp.common.config import _load_runtime_config, _section
from mcp.common.constants import MESSAGE_TOOL_BLOCKED_ROLES, TOOL_NAMES
from mcp.common.tool_schemas import TOOL_SCHEMAS
from mcp.lifecycle.transitions import _append_transition_history, _assert_transition, _transition_actor
from mcp.artifact.publish import _artifact
from mcp.lifecycle.taskflow import _taskflow
from mcp.project.flow import _projectflow
from mcp.room.rooms import _roomflow
from mcp.transport.message import _message

# Monkeypatch-sensitive (15-name set): resolved at call time via refs so a
# test patch of this module's attribute stays visible at the call site.
_filesync = refs.proxy("_filesync")
_write_task = refs.proxy("_write_task")


# Pinned in server.py: test_transition_validation.py statically audits this
# file's source (together with project/flow.py) and requires the single task
# status-write entry point (_transition_task) to be physically present here,
# with every status write inside the whitelisted functions.
def _transition_task(
    arguments: dict[str, Any],
    task: dict[str, Any],
    task_id: str,
    new_status: str,
    action: str,
    note: str = "",
    from_status: str | None = None,
    **extra: Any,
) -> str:
    """Single entry point for task status changes: table validation ->
    history append -> status + extra fields merged -> ``_write_task``.

    Project-node mirroring and shared-storage sync stay at the call site
    (each site has a different sync shape: exclude / result_paths).
    Returns the previous task-meta status.
    """
    old_status = from_status if from_status is not None else str(task.get("status") or "")
    _assert_transition(old_status, new_status, action, arguments=arguments, task_id=task_id)
    if extra:
        task.update(extra)
    _append_transition_history(task, old_status, new_status, action, _transition_actor(arguments), note)
    task["status"] = new_status
    _write_task(arguments, task)
    return old_status


refs.define("_transition_task", _transition_task)


def _tool_schema(name: str) -> dict[str, Any]:
    schema = TOOL_SCHEMAS[name]
    return {
        "name": name,
        "description": schema["description"],
        "inputSchema": schema["inputSchema"],
    }

def list_tools() -> list[dict[str, Any]]:
    return [_tool_schema(name) for name in _visible_tool_names()]

def call_tool(name: str, arguments: dict[str, Any] | None = None) -> dict[str, Any]:
    args = arguments or {}
    if name not in TOOL_NAMES:
        payload = {"ok": False, "error": "unknown_tool", "tool": name}
    elif name == "message" and _message_tool_blocked_for_runtime_role():
        payload = {
            "ok": False,
            "error": "forbidden_tool",
            "tool": name,
            "message": "message tool is not available to worker roles",
        }
    elif name == "health":
        payload = {"ok": True, "tool": name, "status": "ok"}
    elif name == "message":
        payload = _message(args)
    elif name == "roomflow":
        payload = _roomflow(args)
    elif name == "filesync":
        payload = _filesync(args)
    elif name == "artifact":
        payload = _artifact(args)
    elif name == "projectflow":
        payload = _projectflow(args)
    elif name == "taskflow":
        payload = _taskflow(args)
    else:
        payload = {
            "ok": True,
            "tool": name,
            "implemented": False,
            "reason": "tool behavior is defined by later TeamHarness behavior slices",
            "arguments": args,
        }
    return {
        "content": [
            {
                "type": "text",
                "text": json.dumps(payload, ensure_ascii=False),
            }
        ]
    }

def _normalize_role(role: str) -> str:
    value = (role or "").strip().replace("_", "-").lower()
    return {
        "team-leader": "leader",
        "teamleader": "leader",
        "team-leader-agent": "leader",
        "remote": "remote-member",
        "remote-member-agent": "remote-member",
    }.get(value, value)

def _runtime_role() -> str:
    role = os.getenv("AGENTTEAMS_AGENT_ROLE", "").strip() or os.getenv("AGENTTEAMS_WORKER_ROLE", "").strip()
    if not role:
        role = str(_section(_load_runtime_config(), "member").get("role") or "").strip()
    return _normalize_role(role)

def _visible_tool_names() -> list[str]:
    if _message_tool_blocked_for_runtime_role():
        return [name for name in TOOL_NAMES if name != "message"]
    return list(TOOL_NAMES)

def _message_tool_blocked_for_runtime_role() -> bool:
    return _runtime_role() in MESSAGE_TOOL_BLOCKED_ROLES

def _role(arguments: dict[str, Any]) -> str:
    runtime_role = _runtime_role()
    if runtime_role:
        return runtime_role
    return _normalize_role(str(arguments.get("role") or "").strip())

refs.define("_role", _role)

def handle_request(request: dict[str, Any]) -> dict[str, Any] | None:
    method = request.get("method")
    request_id = request.get("id")
    if request_id is None and isinstance(method, str) and method.startswith("notifications/"):
        return None
    if method == "initialize":
        result = {
            "protocolVersion": "2024-11-05",
            "serverInfo": {"name": "teamharness", "version": "0.1.0"},
            "capabilities": {"tools": {}},
        }
    elif method == "tools/list":
        result = {"tools": list_tools()}
    elif method == "tools/call":
        params = request.get("params", {}) or {}
        result = call_tool(str(params.get("name", "")), params.get("arguments", {}) or {})
    else:
        result = {
            "content": [
                {
                    "type": "text",
                    "text": json.dumps({"ok": False, "error": "unknown_method", "method": method}, ensure_ascii=False),
                }
            ]
        }
    return {"jsonrpc": "2.0", "id": request_id, "result": result}

def main() -> int:
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            request = json.loads(line)
        except json.JSONDecodeError:
            response = {
                "jsonrpc": "2.0",
                "id": None,
                "error": {"code": -32700, "message": "Parse error"},
            }
            print(json.dumps(response, ensure_ascii=False), flush=True)
            continue
        response = handle_request(request)
        if response is not None:
            print(json.dumps(response, ensure_ascii=False), flush=True)
    return 0


def __getattr__(name: str) -> Any:
    """PEP 562: historical flat-namespace names (pre-W1-a2 re-exports)
    resolve through the refs layer instead of physical attributes."""
    return refs.resolve(name)


if __name__ == "__main__":
    raise SystemExit(main())
