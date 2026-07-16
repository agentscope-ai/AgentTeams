#!/usr/bin/env python3
"""QwenPaw adapter for WorkerFlow."""

from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import sys
from typing import Any


PLUGIN_DIR = Path(__file__).resolve().parent
ASSET_DIR = PLUGIN_DIR / "workerflow"
if not (ASSET_DIR / "plugin.yaml").exists():
    ASSET_DIR = PLUGIN_DIR.parent.parent

MCP_CLIENT_ID = "workerflow"


def _read_yaml(path: Path) -> dict[str, Any]:
    try:
        import yaml

        data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    except (FileNotFoundError, ImportError):
        return {}
    return data if isinstance(data, dict) else {}


def _section(data: dict[str, Any], key: str) -> dict[str, Any]:
    value = data.get(key) or {}
    return value if isinstance(value, dict) else {}


def _iter_qwenpaw_agents() -> list[tuple[str, Path]]:
    try:
        from qwenpaw.config.utils import load_config
    except ImportError:
        workspace = os.getenv("QWENPAW_WORKSPACE_DIR", "").strip()
        return [("default", Path(workspace))] if workspace else []

    root = load_config()
    profiles = getattr(getattr(root, "agents", None), "profiles", {}) or {}
    agents: list[tuple[str, Path]] = []
    for agent_id, ref in profiles.items():
        if getattr(ref, "enabled", True) is False:
            continue
        workspace_dir = Path(getattr(ref, "workspace_dir", "")).expanduser()
        if str(workspace_dir):
            agents.append((str(agent_id), workspace_dir))
    return agents


def _copytree_replace(source: Path, target: Path) -> None:
    if target.exists():
        shutil.rmtree(target)
    shutil.copytree(source, target, ignore=shutil.ignore_patterns("__pycache__", ".DS_Store", "*.pyc"))


def _skill_entries() -> list[dict[str, Any]]:
    manifest = _read_yaml(ASSET_DIR / "plugin.yaml")
    entries: list[dict[str, Any]] = []
    for group_entries in _section(manifest, "skills").values():
        if isinstance(group_entries, list):
            for entry in group_entries:
                if isinstance(entry, dict):
                    entries.append(entry)
    return entries


def _install_skills() -> dict[str, Any]:
    installed: list[str] = []
    try:
        from qwenpaw.agents.skill_system.registry import ensure_skill_pool_initialized, reconcile_pool_manifest
        from qwenpaw.agents.skill_system.store import get_skill_pool_dir
    except ImportError:
        return {"installed": installed, "skipped": "qwenpaw skill API unavailable"}

    ensure_skill_pool_initialized()
    pool_dir = get_skill_pool_dir()
    pool_dir.mkdir(parents=True, exist_ok=True)
    for entry in _skill_entries():
        skill_id = str(entry.get("id") or "").strip()
        source = ASSET_DIR / str(entry.get("path") or "")
        if not skill_id or not source.exists():
            continue
        target = pool_dir / f"workerflow-{skill_id}"
        _copytree_replace(source, target)
        installed.append(target.name)
    reconcile_pool_manifest()
    return {"installed": installed}


def _enable_workspace_skills(workspace_dir: Path) -> dict[str, Any]:
    installed: list[str] = []
    failed: list[dict[str, str]] = []
    try:
        from qwenpaw.agents.skill_system.pool_service import SkillPoolService
    except ImportError:
        return {"installed": installed, "skipped": "qwenpaw skill pool API unavailable"}

    service = SkillPoolService()
    for entry in _skill_entries():
        skill_id = str(entry.get("id") or "").strip()
        if not skill_id:
            continue
        skill_name = f"workerflow-{skill_id}"
        result = service.download_to_workspace(skill_name, workspace_dir, overwrite=True)
        if result.get("success"):
            installed.append(skill_name)
        else:
            failed.append({"name": skill_name, "reason": str(result.get("reason") or "unknown")})
    return {"installed": installed, "failed": failed}


def _mcp_client_env() -> dict[str, str]:
    env: dict[str, str] = {}
    for name in (
        "TEAMHARNESS_RUNTIME_CONFIG",
        "AGENTTEAMS_MEMBER_RUNTIME_CONFIG",
        "AGENTTEAMS_MATRIX_URL",
        "AGENTTEAMS_MATRIX_SERVER",
        "AGENTTEAMS_MATRIX_HOMESERVER",
        "AGENTTEAMS_WORKER_MATRIX_TOKEN",
        "AGENTTEAMS_MATRIX_TOKEN",
        "AGENTTEAMS_MATRIX_USER_ID",
        "AGENTTEAMS_MATRIX_DOMAIN",
        "AGENTTEAMS_WORKER_ROLE",
        "AGENTTEAMS_AGENT_ROLE",
        "AGENTTEAMS_WORKER_NAME",
        "QWENPAW_API_BASE_URL",
        "QWENPAW_BASE_URL",
        "QWENPAW_WORKING_DIR",
        "QWENPAW_WORKSPACE_DIR",
        "COPAW_WORKING_DIR",
    ):
        value = os.getenv(name, "").strip()
        if value:
            env[name] = value
    return env


def _ensure_mcp_client(agent_id: str, workspace_dir: Path) -> dict[str, Any]:
    try:
        from qwenpaw.config.config import MCPClientConfig, MCPConfig, load_agent_config, save_agent_config
    except ImportError:
        marker = workspace_dir / ".workerflow" / "mcp-client.json"
        marker.parent.mkdir(parents=True, exist_ok=True)
        marker.write_text(json.dumps({"id": MCP_CLIENT_ID}, indent=2), encoding="utf-8")
        return {"agent": agent_id, "action": "marker", "path": str(marker)}

    agent_config = load_agent_config(agent_id)
    if agent_config.mcp is None:
        agent_config.mcp = MCPConfig()
    server_path = ASSET_DIR / "mcp" / "server.py"
    client = MCPClientConfig(
        name=MCP_CLIENT_ID,
        description="Worker-local agent workflow MCP server",
        enabled=True,
        transport="stdio",
        command=sys.executable,
        args=[str(server_path)],
        cwd=str(ASSET_DIR),
        env=_mcp_client_env(),
    )
    agent_config.mcp.clients[MCP_CLIENT_ID] = client
    save_agent_config(agent_id, agent_config)
    return {"agent": agent_id, "action": "configured"}


def apply_workerflow() -> dict[str, Any]:
    skills = _install_skills()
    agents = []
    mcp = []
    for agent_id, workspace_dir in _iter_qwenpaw_agents():
        workspace_dir.mkdir(parents=True, exist_ok=True)
        agents.append({"agent": agent_id, "workspace": str(workspace_dir), "skills": _enable_workspace_skills(workspace_dir)})
        mcp.append(_ensure_mcp_client(agent_id, workspace_dir))
    return {"ok": True, "agents": agents, "skills": skills, "mcp": mcp}


class WorkerFlowPlugin:
    def __init__(self) -> None:
        self.last_apply_result: dict[str, Any] = {}

    def register(self, api: Any) -> None:
        def sync() -> dict[str, Any]:
            self.last_apply_result = apply_workerflow()
            return self.last_apply_result

        api.register_startup_hook("workerflow_sync", sync, priority=45)
        self._register_http(api)

    def _register_http(self, api: Any) -> None:
        try:
            from fastapi import APIRouter
        except Exception:
            return
        router = APIRouter()

        @router.get("/health")
        def health() -> dict[str, Any]:
            return {"ok": True, "plugin": "workerflow", "adapter": "qwenpaw"}

        @router.get("/status")
        def status() -> dict[str, Any]:
            return {"ok": True, "lastApply": self.last_apply_result}

        @router.post("/sync")
        def sync_endpoint() -> dict[str, Any]:
            self.last_apply_result = apply_workerflow()
            return self.last_apply_result

        api.register_http_router(router, prefix="/workerflow", tags=["workerflow"])


plugin = WorkerFlowPlugin()
