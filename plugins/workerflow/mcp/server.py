#!/usr/bin/env python3
"""WorkerFlow MCP stdio server."""

from __future__ import annotations

import html
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import sys
import time
from typing import Any
import urllib.error
import urllib.parse
import urllib.request
import uuid


TOOL_NAMES = ["worker_agentflow"]

TOOL_SCHEMAS: dict[str, dict[str, Any]] = {
    "worker_agentflow": {
        "description": (
            "Manage Worker-local QwenPaw agent lifecycle for internal workflow. "
            "Use only for list_agents, list_subagents, create_temp_agent, "
            "delete_temp_agent, cleanup_shared, workflow_run, and workflow_* actions. "
            "Temporary agents are not TeamHarness Workers."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": [
                        "list_agents",
                        "list_subagents",
                        "create_temp_agent",
                        "delete_temp_agent",
                        "cleanup_shared",
                        "workflow_run",
                        "workflow_start",
                        "workflow_update",
                        "workflow_finish",
                        "workflow_fail",
                    ],
                },
                "apiBaseUrl": {
                    "type": "string",
                    "description": "Optional QwenPaw API base URL. Defaults to QWENPAW_API_BASE_URL or http://127.0.0.1:8088/api.",
                },
                "agentId": {
                    "type": "string",
                    "description": "Temporary agent id. create_temp_agent generates tmp-workerflow-* when omitted.",
                },
                "name": {"type": "string"},
                "description": {"type": "string"},
                "workspaceDir": {
                    "type": "string",
                    "description": "Optional QwenPaw workspace directory for the temporary agent.",
                },
                "templatePath": {
                    "type": "string",
                    "description": "Optional external template directory containing AGENTS.md and skills/. Overrides subagent.",
                },
                "subagent": {
                    "type": "string",
                    "description": "Optional runtime subagent name under the default workspace subagents/<name> directory.",
                },
                "sharedRunId": {
                    "type": "string",
                    "description": "Optional safe run id for a shared directory under default workspace shared/workerflow/<run-id>.",
                },
                "sharedDir": {
                    "type": "string",
                    "description": "Optional explicit shared directory. Relative paths resolve from the default workspace.",
                },
                "runId": {
                    "type": "string",
                    "description": "Workflow run id. Defaults to sharedRunId when omitted.",
                },
                "roomId": {
                    "type": "string",
                    "description": "Required Matrix DM/current conversation room id for workflow_run and workflow_start.",
                },
                "eventId": {
                    "type": "string",
                    "description": "Matrix event id to edit for workflow_update, workflow_finish, and workflow_fail.",
                },
                "title": {"type": "string"},
                "input": {
                    "type": "string",
                    "description": "Original workflow input to include in generated subagent submit prompts.",
                },
                "status": {"type": "string"},
                "summary": {"type": "string"},
                "subagents": {
                    "type": "array",
                    "items": {"type": "object", "additionalProperties": True},
                    "description": "Visible subagent rows for the workflow card or workflow_run fan-out plan.",
                },
                "nodes": {
                    "type": "array",
                    "items": {"type": "object", "additionalProperties": True},
                    "description": "Optional DAG nodes for workflow_run. Each node may include id, subagent, task, and dependsOn.",
                },
                "steps": {
                    "type": "array",
                    "items": {"type": "object", "additionalProperties": True},
                    "description": "Visible workflow step rows for the workflow card.",
                },
                "language": {"type": "string"},
                "skillNames": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "Optional existing QwenPaw skill names for the created agent profile.",
                },
                "activeModel": {
                    "type": "object",
                    "additionalProperties": True,
                },
                "merge": {
                    "type": "object",
                    "additionalProperties": True,
                    "description": "Optional merge instruction stored with workflow_run and included in subagent prompts.",
                },
                "cleanupWorkspace": {
                    "type": "boolean",
                    "description": "For delete_temp_agent and workflow_finish/fail, remove workspaces only when safely inside QWENPAW_WORKING_DIR/workspaces.",
                },
                "dryRun": {
                    "type": "boolean",
                    "description": "Return the resolved operation without calling QwenPaw.",
                },
            },
            "required": ["action"],
            "additionalProperties": True,
        },
    },
}

SAFE_SUBAGENT_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
SAFE_RUN_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
SAFE_TEMP_AGENT_ID_RE = re.compile(r"^tmp-[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")


def _require_loopback_api_base(base: str) -> None:
    parsed = urllib.parse.urlparse(base)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        raise ValueError("apiBaseUrl must be an http(s) loopback URL")
    host = parsed.hostname.lower()
    if host == "localhost":
        return
    try:
        if ipaddress.ip_address(host).is_loopback:
            return
    except ValueError:
        pass
    raise ValueError("apiBaseUrl must be an http(s) loopback URL")


def _api_base(raw: str | None = None) -> str:
    explicit = bool((raw or "").strip())
    base = (
        (raw or "").strip()
        or os.getenv("QWENPAW_API_BASE_URL", "").strip()
        or os.getenv("QWENPAW_BASE_URL", "").strip()
        or "http://127.0.0.1:8088/api"
    ).rstrip("/")
    if not base.endswith("/api"):
        base = f"{base}/api"
    if explicit:
        _require_loopback_api_base(base)
    return base


def _json_request(method: str, base_url: str, path: str, payload: dict[str, Any] | None = None) -> dict[str, Any]:
    url = f"{base_url}{path}"
    body = None if payload is None else json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(url, data=body, method=method)
    request.add_header("Accept", "application/json")
    if body is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            text = response.read().decode("utf-8")
            return json.loads(text) if text else {}
    except urllib.error.HTTPError as exc:
        text = exc.read().decode("utf-8", errors="replace")
        try:
            detail = json.loads(text)
        except json.JSONDecodeError:
            detail = text
        raise RuntimeError(f"{method} {path} failed: HTTP {exc.code}: {detail}") from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(f"{method} {path} failed: {exc}") from exc


def _qwenpaw_working_dir() -> Path:
    raw = os.getenv("QWENPAW_WORKING_DIR", "").strip() or os.getenv("COPAW_WORKING_DIR", "").strip()
    if raw:
        return Path(raw).expanduser()
    home = os.getenv("HOME", "").strip()
    return Path(home).expanduser() / ".qwenpaw" if home else Path.cwd() / ".qwenpaw"


def _default_workspace(agent_id: str) -> Path:
    return _qwenpaw_working_dir() / "workspaces" / agent_id


def _default_agent_workspace() -> Path:
    raw = os.getenv("QWENPAW_DEFAULT_WORKSPACE_DIR", "").strip()
    if raw:
        return Path(raw).expanduser()
    return _default_workspace("default")


def _subagents_dir() -> Path:
    return _default_agent_workspace() / "subagents"


def _shared_root() -> Path:
    return _default_agent_workspace() / "shared" / "workerflow"


def _resolve_run_id(raw: Any, fallback: str) -> str:
    value = str(raw or "").strip() or fallback
    if not SAFE_RUN_ID_RE.match(value):
        raise ValueError("sharedRunId must be a safe single path component")
    return value


def _resolve_shared_dir(shared_dir: Any, shared_run_id: Any, fallback_run_id: str) -> Path:
    raw = str(shared_dir or "").strip()
    if raw:
        path = Path(raw).expanduser()
        if not path.is_absolute():
            path = _default_agent_workspace() / path
        return path
    return _shared_root() / _resolve_run_id(shared_run_id, fallback_run_id)


def _shared_plan(agent_id: str, workspace_dir: Path, shared_dir: Path) -> dict[str, Any]:
    return {
        "path": str(shared_dir),
        "inputs": str(shared_dir / "inputs"),
        "output": str(shared_dir / "outputs" / agent_id),
        "workspaceLink": str(workspace_dir / "shared"),
        "metadata": str(workspace_dir / ".workerflow" / "shared.json"),
    }


def _replace_shared_link(link_path: Path, shared_dir: Path) -> dict[str, Any]:
    if link_path.is_symlink():
        current = Path(os.readlink(link_path))
        if not current.is_absolute():
            current = (link_path.parent / current).resolve()
        if current.resolve() == shared_dir.resolve():
            return {"created": False, "path": str(link_path), "target": str(shared_dir), "reason": "already_linked"}
        link_path.unlink()
    elif link_path.exists():
        return {"created": False, "path": str(link_path), "target": str(shared_dir), "reason": "path_exists_not_symlink"}

    os.symlink(shared_dir, link_path)
    return {"created": True, "path": str(link_path), "target": str(shared_dir)}


def _setup_shared_dir(agent_id: str, workspace_dir: Path, shared_dir: Path) -> dict[str, Any]:
    shared_dir = shared_dir.expanduser().resolve()
    inputs_dir = shared_dir / "inputs"
    output_dir = shared_dir / "outputs" / agent_id
    inputs_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    workspace_dir.mkdir(parents=True, exist_ok=True)
    metadata_dir = workspace_dir / ".workerflow"
    metadata_dir.mkdir(parents=True, exist_ok=True)
    metadata = {
        "agentId": agent_id,
        "sharedPath": str(shared_dir),
        "inputsPath": str(inputs_dir),
        "outputPath": str(output_dir),
    }
    metadata_path = metadata_dir / "shared.json"
    metadata_path.write_text(json.dumps(metadata, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    link = _replace_shared_link(workspace_dir / "shared", shared_dir)
    return {
        "path": str(shared_dir),
        "inputs": str(inputs_dir),
        "output": str(output_dir),
        "metadata": str(metadata_path),
        "link": link,
    }


def _safe_cleanup_shared(shared_dir: Path) -> dict[str, Any]:
    root = _shared_root().resolve()
    target = shared_dir.expanduser().resolve()
    try:
        target.relative_to(root)
    except ValueError:
        return {"removed": False, "reason": "shared_dir_outside_workerflow_root", "path": str(target)}
    if target == root:
        return {"removed": False, "reason": "refuse_to_remove_shared_root", "path": str(target)}
    if target.exists():
        shutil.rmtree(target)
        return {"removed": True, "path": str(target)}
    return {"removed": False, "reason": "shared_dir_missing", "path": str(target)}


def _write_json(path: Path, data: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f".{path.name}.tmp")
    tmp.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    tmp.replace(path)


def _read_json(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        return {}
    return data if isinstance(data, dict) else {}


def _load_runtime_config() -> dict[str, Any]:
    runtime_config = os.getenv("TEAMHARNESS_RUNTIME_CONFIG", "").strip()
    if not runtime_config:
        runtime_config = os.getenv("AGENTTEAMS_MEMBER_RUNTIME_CONFIG", "").strip()
    if not runtime_config:
        return {}
    path = Path(runtime_config).expanduser()
    if not path.exists():
        return {}
    text = path.read_text(encoding="utf-8")
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        data = None
    if isinstance(data, dict):
        return data
    try:
        import yaml

        data = yaml.safe_load(text) or {}
    except Exception:
        data = _simple_yaml_sections(text)
    return data if isinstance(data, dict) else {}


def _simple_yaml_sections(text: str) -> dict[str, Any]:
    data: dict[str, Any] = {}
    section: str | None = None
    nested_section: str | None = None
    for line in text.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        top = re.match(r"^([A-Za-z0-9_]+):\s*(.*)$", line)
        if top:
            key, value = top.group(1), top.group(2).strip()
            if value:
                data[key] = _yaml_scalar(value)
                section = None
                nested_section = None
            else:
                data[key] = {}
                section = key
                nested_section = None
            continue
        nested = re.match(r"^\s{2}([A-Za-z0-9_]+):\s*(.*)$", line)
        if nested and section and isinstance(data.get(section), dict):
            key, value = nested.group(1), nested.group(2).strip()
            if value:
                data[section][key] = _yaml_scalar(value)
                nested_section = None
            else:
                data[section][key] = {}
                nested_section = key
            continue
        deep = re.match(r"^\s{4}([A-Za-z0-9_]+):\s*(.*)$", line)
        if deep and section and nested_section and isinstance(data.get(section), dict):
            parent = data[section].get(nested_section)
            if isinstance(parent, dict):
                parent[deep.group(1)] = _yaml_scalar(deep.group(2).strip())
    return data


def _yaml_scalar(value: str) -> Any:
    if value in {"", "null", "Null", "NULL", "~"}:
        return ""
    if value in {"true", "True", "TRUE"}:
        return True
    if value in {"false", "False", "FALSE"}:
        return False
    if (value.startswith("'") and value.endswith("'")) or (value.startswith('"') and value.endswith('"')):
        return value[1:-1]
    return value


def _section(data: dict[str, Any], name: str) -> dict[str, Any]:
    value = data.get(name)
    return value if isinstance(value, dict) else {}


def _matrix_user_id() -> str:
    explicit = os.getenv("AGENTTEAMS_MATRIX_USER_ID", "").strip()
    if explicit:
        return explicit
    member = _section(_load_runtime_config(), "member")
    return str(member.get("matrixUserId") or member.get("matrix_user_id") or "").strip()


def _workflow_run_id(arguments: dict[str, Any], action: str) -> str:
    raw = arguments.get("runId") or arguments.get("run_id") or arguments.get("sharedRunId")
    if raw:
        return _resolve_run_id(raw, "")
    shared_dir = str(arguments.get("sharedDir") or "").strip()
    if shared_dir:
        return _resolve_run_id(Path(shared_dir).expanduser().name, "")
    if action in {"workflow_start", "workflow_run"}:
        return f"run-{uuid.uuid4().hex[:8]}"
    raise ValueError("runId or sharedRunId is required")


def _workflow_state_path(shared_dir: Path) -> Path:
    return shared_dir / "workflow.json"


def _workflow_rows(value: Any) -> list[dict[str, Any]]:
    if not isinstance(value, list):
        return []
    rows: list[dict[str, Any]] = []
    for item in value:
        if isinstance(item, dict):
            rows.append(dict(item))
        elif item is not None:
            rows.append({"name": str(item)})
    return rows


def _workflow_row_keys(row: dict[str, Any]) -> set[str]:
    keys: set[str] = set()
    for key in ("id", "agentId", "agent_id", "name"):
        value = str(row.get(key) or "").strip()
        if value:
            keys.add(value)
    return keys


def _sync_subagent_rows(subagents: list[dict[str, Any]], steps: list[dict[str, Any]]) -> list[dict[str, Any]]:
    if not subagents or not steps:
        return subagents
    synced = [dict(row) for row in subagents]
    by_key: dict[str, int] = {}
    for index, row in enumerate(synced):
        for key in _workflow_row_keys(row):
            by_key[key] = index
    for step in steps:
        for key in _workflow_row_keys(step):
            index = by_key.get(key)
            if index is None:
                continue
            if "status" in step:
                synced[index]["status"] = step.get("status")
            if "summary" in step:
                synced[index]["summary"] = step.get("summary")
            break
    return synced


def _workflow_done(status: Any) -> bool:
    return str(status or "").strip().lower() in {"done", "success", "succeeded", "completed"}


def _workflow_completed_keys(state: dict[str, Any]) -> set[str]:
    completed: set[str] = set()
    for row in _workflow_rows(state.get("steps")) + _workflow_rows(state.get("subagents")) + _workflow_rows(state.get("nodes")):
        if _workflow_done(row.get("status")):
            completed.update(_workflow_row_keys(row))
    return completed


def _workflow_rows_by_key(rows: list[dict[str, Any]]) -> dict[str, dict[str, Any]]:
    by_key: dict[str, dict[str, Any]] = {}
    for row in rows:
        for key in _workflow_row_keys(row):
            by_key.setdefault(key, row)
    return by_key


def _with_upstream_summaries(prompt: str, depends_on: list[str], state: dict[str, Any]) -> str:
    by_key = _workflow_rows_by_key(_workflow_rows(state.get("steps")) + _workflow_rows(state.get("subagents")))
    lines: list[str] = []
    for dep_id in depends_on:
        row = by_key.get(dep_id) or {}
        summary = str(row.get("summary") or "").strip()
        status = str(row.get("status") or "").strip()
        if summary:
            lines.append(f"- {dep_id}: {summary}")
        elif status:
            lines.append(f"- {dep_id}: {status}")
    if not lines:
        return prompt
    return f"{prompt.rstrip()}\n\nUpstream summaries:\n" + "\n".join(lines)


def _mark_workflow_rows_ready(rows: list[dict[str, Any]], ready_ids: set[str]) -> list[dict[str, Any]]:
    marked: list[dict[str, Any]] = []
    for row in rows:
        item = dict(row)
        if str(item.get("id") or "").strip() in ready_ids:
            item["status"] = "ready"
            item["summary"] = "dependencies done; ready for submit"
        marked.append(item)
    return marked


def _advance_workflow_state(state: dict[str, Any]) -> list[dict[str, Any]]:
    waiting = _workflow_rows(state.get("waitingInstructions"))
    if not waiting:
        state["readyInstructions"] = []
        return []

    completed = _workflow_completed_keys(state)
    ready: list[dict[str, Any]] = []
    still_waiting: list[dict[str, Any]] = []
    for instruction in waiting:
        depends_on = [str(item or "").strip() for item in instruction.get("dependsOn") or []]
        depends_on = [item for item in depends_on if item]
        if depends_on and all(dep_id in completed for dep_id in depends_on):
            ready_instruction = dict(instruction)
            ready_instruction["submitPrompt"] = _with_upstream_summaries(
                str(ready_instruction.get("submitPrompt") or ""),
                depends_on,
                state,
            )
            ready.append(ready_instruction)
        else:
            still_waiting.append(instruction)

    state["readyInstructions"] = ready
    if not ready:
        return []

    state["waitingInstructions"] = still_waiting
    submit_instructions = _workflow_rows(state.get("submitInstructions"))
    submitted_ids = {str(item.get("id") or "").strip() for item in submit_instructions}
    for instruction in ready:
        instruction_id = str(instruction.get("id") or "").strip()
        if instruction_id and instruction_id not in submitted_ids:
            submit_instructions.append(instruction)
            submitted_ids.add(instruction_id)
    state["submitInstructions"] = submit_instructions

    ready_ids = {str(instruction.get("id") or "").strip() for instruction in ready}
    state["nodes"] = _mark_workflow_rows_ready(_workflow_rows(state.get("nodes")), ready_ids)
    state["subagents"] = _mark_workflow_rows_ready(_workflow_rows(state.get("subagents")), ready_ids)
    return ready


def _workflow_temp_agent_ids(state: dict[str, Any]) -> list[str]:
    agent_ids: list[str] = []
    seen: set[str] = set()
    for row in _workflow_rows(state.get("subagents")) + _workflow_rows(state.get("nodes")):
        agent_id = str(row.get("agentId") or row.get("agent_id") or "").strip()
        if not agent_id.startswith("tmp-workerflow-") or not SAFE_TEMP_AGENT_ID_RE.match(agent_id):
            continue
        if agent_id not in seen:
            agent_ids.append(agent_id)
            seen.add(agent_id)
    return agent_ids


def _cleanup_workflow_temp_agents(state: dict[str, Any], arguments: dict[str, Any]) -> dict[str, Any]:
    results: list[dict[str, Any]] = []
    dry_run = bool(arguments.get("dryRun"))
    for agent_id in _workflow_temp_agent_ids(state):
        try:
            deleted = _agentflow(
                {
                    "action": "delete_temp_agent",
                    "apiBaseUrl": arguments.get("apiBaseUrl"),
                    "agentId": agent_id,
                    "cleanupWorkspace": bool(arguments.get("cleanupWorkspace")),
                    "dryRun": dry_run,
                },
            )
            item = {
                "agentId": agent_id,
                "ok": True,
                "deleted": False if dry_run else deleted.get("deleted", True),
            }
            if "cleanup" in deleted:
                item["cleanup"] = deleted["cleanup"]
            if dry_run:
                item["planned"] = True
            results.append(item)
        except RuntimeError as exc:
            message = str(exc)
            if "HTTP 404" in message:
                results.append({"agentId": agent_id, "ok": True, "deleted": False, "reason": "agent_missing"})
            else:
                results.append({"agentId": agent_id, "ok": False, "error": message})
        except Exception as exc:
            results.append({"agentId": agent_id, "ok": False, "error": str(exc)})
    return {
        "agents": results,
        "deleted": sum(1 for item in results if item.get("ok") and item.get("deleted") is not False),
        "missing": sum(1 for item in results if item.get("reason") == "agent_missing"),
        "failed": sum(1 for item in results if not item.get("ok")),
    }


def _workflow_status(action: str, arguments: dict[str, Any], previous: dict[str, Any]) -> str:
    explicit = str(arguments.get("status") or "").strip()
    if explicit:
        return explicit
    if action == "workflow_start":
        return "running"
    if action == "workflow_finish":
        return "done"
    if action == "workflow_fail":
        return "failed"
    return str(previous.get("status") or "running")


def _merge_workflow_state(action: str, arguments: dict[str, Any]) -> dict[str, Any]:
    run_id = _workflow_run_id(arguments, action)
    shared_dir = _resolve_shared_dir(arguments.get("sharedDir"), arguments.get("sharedRunId") or run_id, run_id).expanduser().resolve()
    previous = _read_json(_workflow_state_path(shared_dir))
    room_id = str(arguments.get("roomId") or arguments.get("room_id") or previous.get("roomId") or "").strip()
    event_id = str(arguments.get("eventId") or arguments.get("event_id") or previous.get("eventId") or "").strip()
    title = str(arguments.get("title") or previous.get("title") or "WorkerFlow").strip()
    summary = str(arguments.get("summary") or previous.get("summary") or "").strip()
    subagents = _workflow_rows(arguments.get("subagents")) if "subagents" in arguments else _workflow_rows(previous.get("subagents"))
    steps = _workflow_rows(arguments.get("steps")) if "steps" in arguments else _workflow_rows(previous.get("steps"))
    subagents = _sync_subagent_rows(subagents, steps)
    nodes = _sync_subagent_rows(_workflow_rows(previous.get("nodes")), steps)
    state = {
        "runId": run_id,
        "title": title,
        "status": _workflow_status(action, arguments, previous),
        "summary": summary,
        "roomId": room_id,
        "eventId": event_id,
        "coordinator": _matrix_user_id(),
        "sharedPath": str(shared_dir),
        "subagents": subagents,
        "steps": steps,
        "updatedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    for key in ("phase", "input", "merge", "workflowMode", "submitInstructions", "waitingInstructions"):
        if key in previous:
            state[key] = previous[key]
    if nodes:
        state["nodes"] = nodes
    if previous.get("createdAt"):
        state["createdAt"] = previous["createdAt"]
    else:
        state["createdAt"] = state["updatedAt"]
    return state


def _html_cell(value: Any) -> str:
    return html.escape(str(value or ""))


def _workflow_cell_value(row: dict[str, Any], key: str) -> Any:
    if key == "name":
        return row.get("name") or row.get("id") or row.get("agentId") or row.get("agent_id")
    if key == "agentId":
        return row.get("agentId") or row.get("agent_id") or row.get("id") or row.get("name")
    return row.get(key)


def _workflow_table(rows: list[dict[str, Any]], columns: list[tuple[str, str]]) -> str:
    if not rows:
        return ""
    header = "".join(f"<th>{html.escape(label)}</th>" for _, label in columns)
    body_rows = []
    for row in rows:
        cells = "".join(f"<td>{_html_cell(_workflow_cell_value(row, key))}</td>" for key, _ in columns)
        body_rows.append(f"<tr>{cells}</tr>")
    return f"<table><thead><tr>{header}</tr></thead><tbody>{''.join(body_rows)}</tbody></table>"


def _workflow_content(state: dict[str, Any]) -> dict[str, Any]:
    title = str(state.get("title") or "WorkerFlow")
    run_id = str(state.get("runId") or "")
    status = str(state.get("status") or "")
    summary = str(state.get("summary") or "")
    coordinator = str(state.get("coordinator") or "")
    shared_path = str(state.get("sharedPath") or "")
    subagents = _workflow_rows(state.get("subagents"))
    steps = _workflow_rows(state.get("steps"))

    lines = [f"[WorkerFlow] {run_id}: {status}"]
    if summary:
        lines.append(f"Summary: {summary}")
    if coordinator:
        lines.append(f"Coordinator: {coordinator}")
    if shared_path:
        lines.append(f"Shared: {shared_path}")
    if subagents:
        lines.append("Subagents:")
        for subagent in subagents:
            label = subagent.get("name") or subagent.get("role") or subagent.get("agentId") or subagent.get("id") or "subagent"
            subagent_status = subagent.get("status") or ""
            subagent_summary = subagent.get("summary") or ""
            lines.append(f"- {label}: {subagent_status} {subagent_summary}".rstrip())
    if steps:
        lines.append("Steps:")
        for step in steps:
            label = step.get("name") or step.get("id") or "step"
            step_status = step.get("status") or ""
            lines.append(f"- {label}: {step_status}".rstrip())

    subagent_table = _workflow_table(
        subagents,
        [("name", "Subagent"), ("role", "Role"), ("status", "Status"), ("summary", "Summary")],
    )
    step_table = _workflow_table(
        steps,
        [("name", "Step"), ("status", "Status"), ("summary", "Summary")],
    )
    html_parts = [
        f"<h3>{html.escape(title)} · {html.escape(run_id)}</h3>",
        f"<p><strong>Status:</strong> {html.escape(status)}</p>",
    ]
    if summary:
        html_parts.append(f"<p><strong>Summary:</strong> {html.escape(summary)}</p>")
    if coordinator:
        html_parts.append(f"<p><strong>Coordinator:</strong> {html.escape(coordinator)}</p>")
    if shared_path:
        html_parts.append(f"<p><strong>Shared:</strong> <code>{html.escape(shared_path)}</code></p>")
    if subagent_table:
        html_parts.append("<h4>Subagents</h4>")
        html_parts.append(subagent_table)
    if step_table:
        html_parts.append("<h4>Steps</h4>")
        html_parts.append(step_table)

    return {
        "msgtype": "m.notice",
        "body": "\n".join(lines),
        "format": "org.matrix.custom.html",
        "formatted_body": "\n".join(html_parts),
        "hiclaw.workflow": {
            "type": "workerflow",
            "runId": run_id,
            "status": status,
            "title": title,
            "summary": summary,
            "ownerRole": "worker",
            "ownerAgentId": "default",
            "coordinator": coordinator,
            "sharedPath": shared_path,
            "subagents": subagents,
            "steps": steps,
        },
    }


def _workflow_matrix_content(state: dict[str, Any], event_id: str = "") -> dict[str, Any]:
    content = _workflow_content(state)
    if not event_id:
        return content
    edit_content = dict(content)
    edit_content["body"] = f"* {content['body']}"
    edit_content["m.new_content"] = content
    edit_content["m.relates_to"] = {
        "rel_type": "m.replace",
        "event_id": event_id,
    }
    return edit_content


def _matrix_homeserver() -> str:
    return str(
        os.getenv("AGENTTEAMS_MATRIX_URL")
        or os.getenv("AGENTTEAMS_MATRIX_SERVER")
        or os.getenv("AGENTTEAMS_MATRIX_HOMESERVER")
        or ""
    ).rstrip("/")


def _matrix_token() -> str:
    credentials = _section(_load_runtime_config(), "credentials")
    token_env = str(credentials.get("matrixTokenEnv") or "AGENTTEAMS_WORKER_MATRIX_TOKEN").strip()
    for name in (token_env, "AGENTTEAMS_WORKER_MATRIX_TOKEN", "AGENTTEAMS_MATRIX_TOKEN"):
        if not name:
            continue
        value = os.getenv(name, "").strip()
        if value:
            return value
    return ""


def _matrix_send_content(room_id: str, content: dict[str, Any]) -> str:
    homeserver = _matrix_homeserver()
    token = _matrix_token()
    if not homeserver or not token:
        raise ValueError(
            "Matrix homeserver and token are required "
            "(AGENTTEAMS_MATRIX_URL or alias; runtime credentials.matrixTokenEnv, "
            "AGENTTEAMS_WORKER_MATRIX_TOKEN, or AGENTTEAMS_MATRIX_TOKEN)"
        )
    encoded_room = urllib.parse.quote(room_id, safe="")
    txn_id = f"workerflow-{os.getpid()}-{int(time.time() * 1000)}"
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/send/m.room.message/{txn_id}",
        data=json.dumps(content).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")[:200]
        raise RuntimeError(f"Matrix API error: HTTP {exc.code}: {body}") from exc
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        raise RuntimeError(f"Matrix API error: {exc}") from exc
    event_id = str(data.get("event_id") or "")
    if not event_id:
        raise RuntimeError("Matrix send response missing event_id")
    return event_id


def _workflow(arguments: dict[str, Any], action: str) -> dict[str, Any]:
    dry_run = bool(arguments.get("dryRun"))
    state = _merge_workflow_state(action, arguments)
    ready_instructions = _advance_workflow_state(state) if action == "workflow_update" else []
    if not state.get("roomId"):
        raise ValueError("roomId is required; pass the current Matrix DM/conversation room explicitly")

    edit_event_id = "" if action == "workflow_start" else str(state.get("eventId") or "")
    if action != "workflow_start" and not edit_event_id:
        raise ValueError("eventId is required for workflow updates unless workflow.json already records it")
    if action in {"workflow_finish", "workflow_fail"}:
        state["cleanupTempAgents"] = _cleanup_workflow_temp_agents(state, arguments)
    content = _workflow_matrix_content(state, edit_event_id)
    result = {
        "ok": True,
        "tool": "worker_agentflow",
        "action": action,
        "runId": state["runId"],
        "roomId": state["roomId"],
        "eventId": state.get("eventId") or "",
        "statePath": str(_workflow_state_path(Path(str(state["sharedPath"])))),
        "content": content,
    }
    for key in ("nodes", "submitInstructions", "waitingInstructions", "readyInstructions", "workflowMode"):
        if key in state:
            result[key] = state[key]
    if "cleanupTempAgents" in state:
        result["cleanupTempAgents"] = state["cleanupTempAgents"]
    if ready_instructions:
        result["readyInstructions"] = ready_instructions
    if dry_run:
        result["dryRun"] = True
        return result

    event_id = _matrix_send_content(str(state["roomId"]), content)
    if action == "workflow_start":
        state["eventId"] = event_id
    else:
        state["lastEditEventId"] = event_id
    _write_json(_workflow_state_path(Path(str(state["sharedPath"]))), state)
    result["eventId"] = str(state.get("eventId") or event_id)
    if action != "workflow_start":
        result["editEventId"] = event_id
    return result


def _workflow_plan_subagents(value: Any) -> list[dict[str, Any]]:
    if not isinstance(value, list) or not value:
        raise ValueError("workflow_run requires at least one subagent")
    subagents: list[dict[str, Any]] = []
    for index, item in enumerate(value):
        if not isinstance(item, dict):
            raise ValueError("workflow_run subagents must be objects")
        subagent_plan = dict(item)
        subagent_id = str(subagent_plan.get("id") or f"subagent-{index + 1}").strip()
        if not SAFE_SUBAGENT_NAME_RE.match(subagent_id):
            raise ValueError("subagent id must be a safe single path component")
        subagent = str(subagent_plan.get("subagent") or "").strip()
        if not subagent:
            raise ValueError("workflow_run subagent requires subagent")
        task = str(subagent_plan.get("task") or "").strip()
        if not task:
            raise ValueError("workflow_run subagent requires task")
        subagent_plan["id"] = subagent_id
        subagent_plan["subagent"] = subagent
        subagent_plan["task"] = task
        subagents.append(subagent_plan)
    return subagents


def _workflow_plan_nodes(arguments: dict[str, Any]) -> tuple[list[dict[str, Any]], bool]:
    raw_nodes = arguments.get("nodes")
    is_dag = isinstance(raw_nodes, list) and bool(raw_nodes)
    nodes = _workflow_plan_subagents(raw_nodes if is_dag else arguments.get("subagents"))
    seen: set[str] = set()
    node_ids = [node["id"] for node in nodes]
    for node in nodes:
        if node["id"] in seen:
            raise ValueError("workflow_run node ids must be unique")
        seen.add(node["id"])
        raw_depends = node.get("dependsOn")
        if raw_depends is None:
            raw_depends = node.get("depends_on")
        depends_on = raw_depends if isinstance(raw_depends, list) else []
        normalized: list[str] = []
        for dep in depends_on:
            dep_id = str(dep or "").strip()
            if not SAFE_SUBAGENT_NAME_RE.match(dep_id):
                raise ValueError("workflow_run dependsOn entries must be safe node ids")
            if dep_id not in node_ids:
                raise ValueError(f"workflow_run node depends on unknown node: {dep_id}")
            if dep_id not in normalized:
                normalized.append(dep_id)
        node["dependsOn"] = normalized
    _validate_workflow_dag(nodes)
    return nodes, is_dag


def _validate_workflow_dag(nodes: list[dict[str, Any]]) -> None:
    by_id = {node["id"]: node for node in nodes}
    visiting: set[str] = set()
    visited: set[str] = set()

    def visit(node_id: str) -> None:
        if node_id in visited:
            return
        if node_id in visiting:
            raise ValueError("workflow_run nodes must not contain dependency cycles")
        visiting.add(node_id)
        for dep in by_id[node_id].get("dependsOn") or []:
            visit(dep)
        visiting.remove(node_id)
        visited.add(node_id)

    for node in nodes:
        visit(node["id"])


def _workflow_subagent_agent_id(run_id: str, subagent: dict[str, Any]) -> str:
    explicit = str(subagent.get("agentId") or subagent.get("agent_id") or "").strip()
    agent_id = explicit or f"tmp-workerflow-{run_id}-{subagent['id']}"
    if not SAFE_TEMP_AGENT_ID_RE.match(agent_id):
        raise ValueError("workflow_run subagent agentId must be a safe tmp-* id")
    return agent_id


def _workflow_submit_prompt(
    title: str,
    input_text: str,
    subagent: dict[str, Any],
    shared: dict[str, Any],
    merge: dict[str, Any],
    dependency_outputs: list[dict[str, str]] | None = None,
) -> str:
    lines = [
        "WorkerFlow subagent task.",
        "",
        f"Workflow: {title}",
        f"Subagent id: {subagent['id']}",
        f"Subagent: {subagent['subagent']}",
        f"Task: {subagent['task']}",
        "",
        "Input:",
        input_text,
        "",
        "Shared files:",
        f"- inputs: {shared.get('inputs') or ''}",
        f"- output: {shared.get('output') or ''}",
        "",
        "Write any files only under the output path above. Return a concise structured result.",
    ]
    if dependency_outputs:
        lines.extend(["", "Upstream outputs:"])
        for item in dependency_outputs:
            lines.append(f"- {item['id']}: {item['output']}")
    merge_instruction = str(merge.get("instruction") or "").strip()
    if merge_instruction:
        lines.extend(["", f"Merge expectation: {merge_instruction}"])
    return "\n".join(lines).strip()


def _workflow_dependency_outputs(node: dict[str, Any], output_paths: dict[str, str]) -> list[dict[str, str]]:
    return [{"id": dep_id, "output": output_paths[dep_id]} for dep_id in node.get("dependsOn") or []]


def _fail_workflow_run_spawn(
    arguments: dict[str, Any],
    run_id: str,
    start: dict[str, Any],
    node_rows: list[dict[str, Any]],
    exc: Exception,
) -> None:
    if bool(arguments.get("dryRun")):
        return
    try:
        _workflow(
            {
                **arguments,
                "runId": run_id,
                "roomId": start.get("roomId"),
                "eventId": start.get("eventId"),
                "status": "failed",
                "summary": f"failed to create subagents: {exc}",
                "subagents": node_rows,
                "steps": [
                    {"name": "Start workflow", "status": "done"},
                    {"name": "Create subagents", "status": "failed", "summary": str(exc)},
                    {"name": "Submit subagent tasks", "status": "pending"},
                    {"name": "Merge results", "status": "pending"},
                ],
            },
            "workflow_fail",
        )
    except Exception as cleanup_exc:
        logger.warning("workflow_run failure compensation failed: %s", cleanup_exc, exc_info=True)


def _workflow_run(arguments: dict[str, Any]) -> dict[str, Any]:
    run_id = _workflow_run_id(arguments, "workflow_run")
    title = str(arguments.get("title") or "WorkerFlow").strip()
    input_text = str(arguments.get("input") or "").strip()
    nodes, is_dag = _workflow_plan_nodes(arguments)
    merge = arguments.get("merge") if isinstance(arguments.get("merge"), dict) else {}
    dry_run = bool(arguments.get("dryRun"))

    start = _workflow(
        {
            **arguments,
            "runId": run_id,
            "status": "spawning",
            "summary": str(arguments.get("summary") or "creating subagents"),
            "subagents": [],
            "steps": [
                {"name": "Start workflow", "status": "done"},
                {"name": "Create subagents", "status": "running"},
                {"name": "Submit subagent tasks", "status": "pending"},
                {"name": "Merge results", "status": "pending"},
            ],
        },
        "workflow_start",
    )

    shared_dir = _resolve_shared_dir(arguments.get("sharedDir"), arguments.get("sharedRunId") or run_id, run_id)
    agent_ids = {node["id"]: _workflow_subagent_agent_id(run_id, node) for node in nodes}
    output_paths = {
        node_id: str((shared_dir / "outputs" / agent_id).expanduser())
        for node_id, agent_id in agent_ids.items()
    }
    node_rows: list[dict[str, Any]] = []
    cleanup_rows: list[dict[str, Any]] = []
    submit_instructions: list[dict[str, Any]] = []
    waiting_instructions: list[dict[str, Any]] = []
    try:
        for node in nodes:
            agent_id = agent_ids[node["id"]]
            display_name = str(node.get("name") or node.get("title") or node["id"])
            display_role = str(node.get("role") or node.get("title") or node["subagent"])

            def record_created_temp_agent(_agent_id: str, _created: dict[str, Any]) -> None:
                cleanup_rows.append(
                    {
                        "id": node["id"],
                        "name": display_name,
                        "agentId": _agent_id,
                        "role": display_role,
                        "subagent": node["subagent"],
                        "task": node["task"],
                        "status": "failed",
                        "summary": "created before workflow_run failed",
                    },
                )

            create_args: dict[str, Any] = {
                "action": "create_temp_agent",
                "apiBaseUrl": arguments.get("apiBaseUrl"),
                "agentId": agent_id,
                "name": display_name,
                "description": str(node.get("description") or f"WorkerFlow subagent {display_name}"),
                "subagent": node["subagent"],
                "sharedRunId": run_id,
                "sharedDir": arguments.get("sharedDir"),
                "language": node.get("language") or arguments.get("language") or "zh",
                "dryRun": dry_run,
                "_createdTempAgent": record_created_temp_agent,
            }
            if isinstance(node.get("skillNames"), list):
                create_args["skillNames"] = node["skillNames"]
            if isinstance(node.get("activeModel"), dict):
                create_args["activeModel"] = node["activeModel"]
            elif isinstance(arguments.get("activeModel"), dict):
                create_args["activeModel"] = arguments["activeModel"]

            created = _agentflow(create_args)
            shared = created.get("shared") if isinstance(created.get("shared"), dict) else {}
            workspace = str(created.get("workspace") or _default_workspace(agent_id))
            dependency_outputs = _workflow_dependency_outputs(node, output_paths)
            submit_prompt = _workflow_submit_prompt(title, input_text, node, shared, merge, dependency_outputs)
            is_ready = not node.get("dependsOn")
            status = "ready" if is_ready else "waiting"
            row = {
                "id": node["id"],
                "name": display_name,
                "agentId": agent_id,
                "role": display_role,
                "subagent": node["subagent"],
                "task": node["task"],
                "dependsOn": node.get("dependsOn") or [],
                "status": status,
                "summary": "ready for submit" if is_ready else f"waiting for {', '.join(node['dependsOn'])}",
                "workspace": workspace,
                "shared": shared,
                "submitPrompt": submit_prompt,
            }
            node_rows.append(row)
            for index, cleanup_row in enumerate(cleanup_rows):
                if cleanup_row.get("agentId") == agent_id:
                    cleanup_rows[index] = row
                    break
            else:
                if not dry_run:
                    cleanup_rows.append(row)
            instruction = {
                "id": node["id"],
                "agentId": agent_id,
                "submitPrompt": submit_prompt,
                "shared": shared,
            }
            if is_ready:
                submit_instructions.append(instruction)
            else:
                instruction["dependsOn"] = node.get("dependsOn") or []
                waiting_instructions.append(instruction)
    except Exception as exc:
        _fail_workflow_run_spawn(arguments, run_id, start, cleanup_rows, exc)
        raise

    if dry_run:
        state_path = Path(str(start["statePath"]))
        return {
            "ok": True,
            "tool": "worker_agentflow",
            "action": "workflow_run",
            "runId": run_id,
            "roomId": start["roomId"],
            "eventId": "",
            "statePath": str(state_path),
            "shared": {"path": str(state_path.parent)},
            "subagents": node_rows,
            "nodes": node_rows,
            "submitInstructions": submit_instructions,
            "waitingInstructions": waiting_instructions,
            "workflowMode": "dag" if is_dag else "fanout",
            "dryRun": True,
        }

    updated = _workflow(
        {
            **arguments,
            "runId": run_id,
            "status": "ready",
            "summary": "subagents ready; submit tasks to agents",
            "subagents": node_rows,
            "steps": [
                {"name": "Start workflow", "status": "done"},
                {"name": "Create subagents", "status": "done"},
                {"name": "Submit subagent tasks", "status": "ready"},
                {"name": "Merge results", "status": "pending"},
            ],
        },
        "workflow_update",
    )

    state_path = Path(str(updated["statePath"]))
    state = _read_json(state_path)
    state["phase"] = "ready"
    state["input"] = input_text
    state["merge"] = merge
    state["workflowMode"] = "dag" if is_dag else "fanout"
    state["nodes"] = node_rows
    state["submitInstructions"] = submit_instructions
    state["waitingInstructions"] = waiting_instructions
    _write_json(state_path, state)

    return {
        "ok": True,
        "tool": "worker_agentflow",
        "action": "workflow_run",
        "runId": run_id,
        "roomId": updated["roomId"],
        "eventId": updated["eventId"],
        "statePath": str(state_path),
        "shared": {"path": str(Path(str(state_path)).parent)},
        "subagents": node_rows,
        "nodes": node_rows,
        "submitInstructions": submit_instructions,
        "waitingInstructions": waiting_instructions,
        "workflowMode": "dag" if is_dag else "fanout",
        "dryRun": False,
    }


def _list_subagents() -> list[dict[str, Any]]:
    root = _subagents_dir()
    if not root.is_dir():
        return []
    result = []
    for path in sorted(root.iterdir()):
        if not path.is_dir() or not SAFE_SUBAGENT_NAME_RE.match(path.name):
            continue
        skills_dir = path / "skills"
        skills = []
        if skills_dir.is_dir():
            skills = sorted(
                skill.name
                for skill in skills_dir.iterdir()
                if skill.is_dir() and (skill / "SKILL.md").is_file()
            )
        result.append({
            "name": path.name,
            "path": str(path),
            "hasAgentsMd": (path / "AGENTS.md").is_file(),
            "skills": skills,
        })
    return result


def _resolve_subagent_template(name: str) -> Path:
    value = name.strip()
    if not SAFE_SUBAGENT_NAME_RE.match(value):
        raise ValueError("subagent must be a safe single path component")
    path = (_subagents_dir() / value).resolve()
    root = _subagents_dir().resolve()
    try:
        path.relative_to(root)
    except ValueError as exc:
        raise ValueError("subagent path escapes default workspace subagents directory") from exc
    if not path.is_dir():
        raise ValueError(f"subagent not found: {value}")
    return path


def _copy_replace(source: Path, target: Path) -> None:
    if target.exists():
        shutil.rmtree(target)
    shutil.copytree(source, target, ignore=shutil.ignore_patterns("__pycache__", ".DS_Store", "*.pyc"))


def _copy_template(template_path: str, workspace_dir: Path) -> dict[str, Any]:
    if not template_path:
        return {"copied": [], "skills": [], "enabled": []}
    template = Path(template_path).expanduser().resolve()
    if not template.is_dir():
        raise ValueError(f"templatePath is not a directory: {template}")
    workspace_dir.mkdir(parents=True, exist_ok=True)

    copied: list[str] = []
    for name in ("AGENTS.md", "PROFILE.md", "SOUL.md"):
        source = template / name
        if source.is_file():
            shutil.copy2(source, workspace_dir / name)
            copied.append(name)

    copied_skills: list[str] = []
    source_skills = template / "skills"
    target_skills = workspace_dir / "skills"
    if source_skills.is_dir():
        target_skills.mkdir(parents=True, exist_ok=True)
        for source in sorted(source_skills.iterdir()):
            if source.is_dir() and (source / "SKILL.md").is_file():
                _copy_replace(source, target_skills / source.name)
                copied_skills.append(source.name)

    enabled = _reconcile_and_enable_skills(workspace_dir, copied_skills)
    return {"copied": copied, "skills": copied_skills, "enabled": enabled}


def _reconcile_and_enable_skills(workspace_dir: Path, skill_names: list[str]) -> list[str]:
    if not skill_names:
        return []
    try:
        from qwenpaw.agents.skill_system.registry import reconcile_workspace_manifest
        from qwenpaw.agents.skill_system.workspace_service import SkillService
    except ImportError:
        return []

    reconcile_workspace_manifest(workspace_dir)
    service = SkillService(workspace_dir)
    enabled: list[str] = []
    for name in skill_names:
        result = service.enable_skill(name)
        if result.get("success"):
            enabled.append(name)
    return enabled


def _safe_cleanup_workspace(agent_id: str, workspace_dir: str) -> dict[str, Any]:
    if not workspace_dir:
        return {"removed": False, "reason": "missing_workspace"}
    if not agent_id.startswith("tmp-"):
        return {"removed": False, "reason": "agent_id_not_temporary"}
    root = (_qwenpaw_working_dir() / "workspaces").resolve()
    target = Path(workspace_dir).expanduser().resolve()
    try:
        target.relative_to(root)
    except ValueError:
        return {"removed": False, "reason": "workspace_outside_qwenpaw_workspaces", "path": str(target)}
    if target.name != agent_id:
        return {"removed": False, "reason": "workspace_name_mismatch", "path": str(target)}
    if target.exists():
        shutil.rmtree(target)
        return {"removed": True, "path": str(target)}
    return {"removed": False, "reason": "workspace_missing", "path": str(target)}


def _agentflow(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "").strip()
    base_url = _api_base(arguments.get("apiBaseUrl"))
    dry_run = bool(arguments.get("dryRun"))

    if action == "list_agents":
        if dry_run:
            return {"ok": True, "tool": "worker_agentflow", "action": action, "apiBaseUrl": base_url}
        return {"ok": True, "tool": "worker_agentflow", "action": action, "agents": _json_request("GET", base_url, "/agents")}

    if action == "list_subagents":
        return {"ok": True, "tool": "worker_agentflow", "action": action, "subagents": _list_subagents()}

    if action == "create_temp_agent":
        agent_id = str(arguments.get("agentId") or f"tmp-workerflow-{uuid.uuid4().hex[:8]}").strip()
        if not agent_id.startswith("tmp-"):
            raise ValueError("temporary agent id must start with tmp-")
        workspace_dir = Path(str(arguments.get("workspaceDir") or _default_workspace(agent_id))).expanduser()
        template_path = str(arguments.get("templatePath") or "").strip()
        subagent = str(arguments.get("subagent") or "").strip()
        if template_path and subagent:
            raise ValueError("use either templatePath or subagent, not both")
        if subagent:
            template_path = str(_resolve_subagent_template(subagent))
        payload: dict[str, Any] = {
            "id": agent_id,
            "name": str(arguments.get("name") or agent_id),
            "description": str(arguments.get("description") or "Temporary WorkerFlow agent"),
            "workspace_dir": str(workspace_dir),
            "language": str(arguments.get("language") or "zh"),
            "skill_names": arguments.get("skillNames") if isinstance(arguments.get("skillNames"), list) else [],
        }
        active_model = arguments.get("activeModel")
        if isinstance(active_model, dict):
            payload["active_model"] = active_model
        shared_dir = _resolve_shared_dir(arguments.get("sharedDir"), arguments.get("sharedRunId"), agent_id)
        if dry_run:
            return {
                "ok": True,
                "tool": "worker_agentflow",
                "action": action,
                "apiBaseUrl": base_url,
                "payload": payload,
                "subagent": subagent,
                "templatePath": template_path,
                "shared": _shared_plan(agent_id, workspace_dir, shared_dir),
            }
        created = _json_request("POST", base_url, "/agents", payload)
        created_recorder = arguments.get("_createdTempAgent")
        if callable(created_recorder):
            created_recorder(agent_id, created)
        resolved_workspace = Path(str(created.get("workspace_dir") or workspace_dir)).expanduser()
        template = _copy_template(template_path, resolved_workspace)
        shared = _setup_shared_dir(agent_id, resolved_workspace, shared_dir)
        return {
            "ok": True,
            "tool": "worker_agentflow",
            "action": action,
            "agentId": agent_id,
            "created": created,
            "workspace": str(resolved_workspace),
            "subagent": subagent,
            "template": template,
            "shared": shared,
        }

    if action == "delete_temp_agent":
        agent_id = str(arguments.get("agentId") or "").strip()
        if not agent_id:
            raise ValueError("agentId is required")
        if not agent_id.startswith("tmp-"):
            raise ValueError("worker_agentflow only deletes tmp-* temporary agents")
        workspace_dir = str(arguments.get("workspaceDir") or "").strip()
        if bool(arguments.get("cleanupWorkspace")) and not workspace_dir:
            try:
                profile = _json_request("GET", base_url, f"/agents/{urllib.parse.quote(agent_id)}")
                workspace_dir = str(profile.get("workspace_dir") or "")
            except RuntimeError:
                workspace_dir = ""
        if dry_run:
            return {
                "ok": True,
                "tool": "worker_agentflow",
                "action": action,
                "apiBaseUrl": base_url,
                "agentId": agent_id,
                "workspace": workspace_dir,
            }
        deleted = _json_request("DELETE", base_url, f"/agents/{urllib.parse.quote(agent_id)}")
        cleanup = (
            _safe_cleanup_workspace(agent_id, workspace_dir)
            if bool(arguments.get("cleanupWorkspace"))
            else {"removed": False, "reason": "cleanup_disabled"}
        )
        return {"ok": True, "tool": "worker_agentflow", "action": action, "agentId": agent_id, "deleted": deleted, "cleanup": cleanup}

    if action == "cleanup_shared":
        if not str(arguments.get("sharedDir") or "").strip() and not str(arguments.get("sharedRunId") or "").strip():
            raise ValueError("sharedRunId or sharedDir is required")
        shared_dir = _resolve_shared_dir(arguments.get("sharedDir"), arguments.get("sharedRunId"), "")
        if dry_run:
            return {
                "ok": True,
                "tool": "worker_agentflow",
                "action": action,
                "shared": {"path": str(shared_dir)},
            }
        return {
            "ok": True,
            "tool": "worker_agentflow",
            "action": action,
            "shared": _safe_cleanup_shared(shared_dir),
        }

    if action == "workflow_run":
        return _workflow_run(arguments)

    if action in {"workflow_start", "workflow_update", "workflow_finish", "workflow_fail"}:
        return _workflow(arguments, action)

    raise ValueError(f"unsupported action: {action}")


def _tool_schema(name: str) -> dict[str, Any]:
    schema = TOOL_SCHEMAS[name]
    return {"name": name, "description": schema["description"], "inputSchema": schema["inputSchema"]}


def _call_tool(name: str, arguments: dict[str, Any] | None = None) -> dict[str, Any]:
    if name not in TOOL_NAMES:
        raise ValueError(f"unknown tool: {name}")
    args = arguments or {}
    try:
        result = _agentflow(args)
    except Exception as exc:  # pylint: disable=broad-except
        result = {"ok": False, "tool": name, "error": str(exc)}
    return {"content": [{"type": "text", "text": json.dumps(result, ensure_ascii=False, indent=2)}]}


def _handle(request: dict[str, Any]) -> dict[str, Any] | None:
    method = request.get("method")
    request_id = request.get("id")
    if request_id is None:
        return None
    try:
        if method == "initialize":
            result = {
                "protocolVersion": "2024-11-05",
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "workerflow", "version": "0.1.0"},
            }
        elif method == "tools/list":
            result = {"tools": [_tool_schema(name) for name in TOOL_NAMES]}
        elif method == "tools/call":
            params = request.get("params") or {}
            result = _call_tool(str(params.get("name") or ""), params.get("arguments") or {})
        else:
            return {"jsonrpc": "2.0", "id": request_id, "error": {"code": -32601, "message": f"method not found: {method}"}}
        return {"jsonrpc": "2.0", "id": request_id, "result": result}
    except Exception as exc:  # pylint: disable=broad-except
        return {"jsonrpc": "2.0", "id": request_id, "error": {"code": -32000, "message": str(exc)}}


def main() -> None:
    for line in sys.stdin:
        if not line.strip():
            continue
        response = _handle(json.loads(line))
        if response is not None:
            sys.stdout.write(json.dumps(response, ensure_ascii=False) + "\n")
            sys.stdout.flush()


if __name__ == "__main__":
    main()
