#!/usr/bin/env python3
"""QwenPaw adapter for TeamHarness.

The adapter owns QwenPaw-specific install/reconcile behavior. It does not poll
storage or runtime config by itself; the qwenpaw-worker triggers those loops.
"""

from __future__ import annotations

import json
import logging
import os
from pathlib import Path
import re
import shutil
import sys
from typing import Any, Dict, List, Optional, Tuple

logger = logging.getLogger(__name__)

PLUGIN_DIR = Path(__file__).resolve().parent
ASSET_DIR = PLUGIN_DIR / "teamharness"
if not (ASSET_DIR / "plugin.yaml").exists():
    ASSET_DIR = PLUGIN_DIR.parent.parent

TEAMS_PROMPT_FILE = "TEAMS.md"
MCP_CLIENT_ID = "teamharness"
REDACTION = "[REDACTED]"
SENSITIVE_FILE_AUTO_DENY_RULE = "SENSITIVE_FILE_BLOCK"
TEAMS_CONTEXT_START = "<!-- BEGIN HICLAW RUNTIME TEAM CONTEXT -->"
TEAMS_CONTEXT_END = "<!-- END HICLAW RUNTIME TEAM CONTEXT -->"
_TASK_TRACE_MODULE: Any = None
_TASK_TRACE_REGISTERED: bool = False

_BUILTIN_SANITIZER_PATTERNS: List[tuple[re.Pattern[str], str]] = [
    (re.compile(r"\b(LTAI)[A-Za-z0-9]{12,}"), r"\1****"),
    (re.compile(r"\b(AKIA)[A-Za-z0-9]{12,}"), r"\1****"),
    (re.compile(r"\b(AKID)[A-Za-z0-9]{12,}"), r"\1****"),
    (
        re.compile(
            r"(?i)((?:access_?key_?secret|secret_?access_?key|accesskeysecret"
            r"|TENCENTCLOUD_SECRET_KEY|aws_secret_access_key"
            r"|credentials\.secret)"
            r"""["']?[\s]*[=:]\s*["']?)([A-Za-z0-9/+=]{16,})"""
        ),
        r"\1********",
    ),
    (
        re.compile(
            r"(?i)((?:security_?token|session_?token|sts_?token)"
            r"""["']?[\s]*[=:]\s*["']?)([A-Za-z0-9/+=]{100,})"""
        ),
        r"\1********",
    ),
    (
        re.compile(
            r"(?i)((?:secret|token|password|passwd|key_secret)"
            r"""["']?[\s]*[=:]\s*["']?)([A-Za-z0-9/+=]{30,})"""
        ),
        r"\1********",
    ),
]


def _read_yaml(path: Path) -> Dict[str, Any]:
    try:
        import yaml

        data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    except FileNotFoundError:
        return {}
    except ImportError:
        logger.warning("PyYAML unavailable; TeamHarness adapter cannot parse %s", path)
        return {}
    return data if isinstance(data, dict) else {}


def _runtime_config_path() -> Optional[Path]:
    raw = os.getenv("TEAMHARNESS_RUNTIME_CONFIG", "").strip()
    return Path(raw) if raw else None


def load_runtime_config() -> Dict[str, Any]:
    path = _runtime_config_path()
    return _read_yaml(path) if path else {}


def _section(data: Dict[str, Any], name: str) -> Dict[str, Any]:
    value = data.get(name) or {}
    return value if isinstance(value, dict) else {}


def _string(value: Any) -> str:
    return str(value).strip() if value is not None else ""


def _string_list(value: Any) -> List[str]:
    if not isinstance(value, list):
        return []
    return [_string(item) for item in value if _string(item)]


def _string_fields(value: Any, keys: List[str]) -> Dict[str, str]:
    if not isinstance(value, dict):
        return {}
    result: Dict[str, str] = {}
    for key in keys:
        text = _string(value.get(key))
        if text:
            result[key] = text
    return result


def _read_json(path: Path) -> Dict[str, Any]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return {}
    return data if isinstance(data, dict) else {}


def _write_json_atomic(path: Path, payload: Dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f".{path.name}.tmp")
    tmp.write_text(json.dumps(payload, indent=2, ensure_ascii=False), encoding="utf-8")
    tmp.replace(path)


def _shared_dir() -> Path:
    raw = os.getenv("TEAMHARNESS_SHARED_DIR", "").strip() or os.getenv("HICLAW_SHARED_DIR", "").strip()
    if raw:
        return Path(raw)
    qwenpaw_dir = os.getenv("QWENPAW_WORKING_DIR", "").strip()
    if qwenpaw_dir:
        qwenpaw_path = Path(qwenpaw_dir)
        if qwenpaw_path.name == ".qwenpaw":
            return qwenpaw_path.parent.parent.parent / "shared"
    return Path.cwd() / "shared"


def _role_prompt(role: str) -> Optional[Path]:
    mapping = {
        "leader": ASSET_DIR / "prompts" / "agent" / "leader.md",
        "team_leader": ASSET_DIR / "prompts" / "agent" / "leader.md",
        "worker": ASSET_DIR / "prompts" / "agent" / "worker.md",
        "remote-member": ASSET_DIR / "prompts" / "agent" / "remote-member.md",
        "manager": ASSET_DIR / "prompts" / "manager" / "AGENTS.md",
    }
    return mapping.get(role)


def render_team_context(config: Dict[str, Any]) -> str:
    team = _section(config, "team")
    member = _section(config, "member")
    desired = _section(config, "desired")
    package = _section(desired, "agentPackage")
    channel_policy = _section(desired, "channelPolicy")
    base_prompt = ASSET_DIR / "prompts" / "team" / "TEAMS.md"
    base = base_prompt.read_text(encoding="utf-8").strip() if base_prompt.exists() else ""
    role = _string(member.get("role") or os.getenv("HICLAW_AGENT_ROLE") or "worker")
    role_prompt = _role_prompt(role)
    role_text = role_prompt.read_text(encoding="utf-8").strip() if role_prompt and role_prompt.exists() else ""

    lines = []
    if base:
        lines.append(base)
    else:
        lines.append("# Team Contract")
    if role_text:
        lines.extend(["", role_text])
    lines.extend(["", TEAMS_CONTEXT_START, "## Runtime Team Context", ""])

    facts = [
        ("team.name", team.get("name")),
        ("team.teamRoomId", team.get("teamRoomId")),
        ("team.leaderName", team.get("leaderName")),
        ("team.leaderRuntimeName", team.get("leaderRuntimeName")),
        ("team.admin.name", _section(team, "admin").get("name")),
        ("team.admin.matrixUserId", _section(team, "admin").get("matrixUserId")),
        ("member.name", member.get("name")),
        ("member.runtimeName", member.get("runtimeName")),
        ("member.role", member.get("role")),
        ("member.runtime", member.get("runtime")),
        ("member.matrixUserId", member.get("matrixUserId")),
        ("member.personalRoomId", member.get("personalRoomId")),
        ("desired.agentPackage.name", package.get("name")),
        ("desired.agentPackage.version", package.get("version")),
    ]
    for key, value in facts:
        text = _string(value)
        if text:
            lines.append(f"- {key}: {text}")
    members = team.get("members")
    if isinstance(members, list) and members:
        lines.extend(["", "### Team Members"])
        for item in members:
            entry = _string_fields(item, ["name", "runtimeName", "role", "matrixUserId", "personalRoomId"])
            if entry:
                lines.append("- " + ", ".join(f"{key}: {value}" for key, value in entry.items()))

    if channel_policy:
        lines.append("- desired.channelPolicy: configured")
    lines.append("")
    lines.append("Do not write secrets, credentials, or live task status into this file.")
    lines.append(TEAMS_CONTEXT_END)
    return "\n".join(lines).rstrip() + "\n"


def _write_team_context(workspace_dir: Path, config: Dict[str, Any]) -> Dict[str, Any]:
    target = workspace_dir / TEAMS_PROMPT_FILE
    text = render_team_context(config)
    target.parent.mkdir(parents=True, exist_ok=True)
    existing = target.read_text(encoding="utf-8") if target.exists() else None
    target.write_text(text, encoding="utf-8")
    return {
        "ok": True,
        "asset": "team-context",
        "path": str(target),
        "action": "created" if existing is None else ("unchanged" if existing == text else "updated"),
    }


def apply_team_context() -> Dict[str, Any]:
    config = load_runtime_config()
    targets = [
        {"agent": agent_id, **_write_team_context(workspace_dir, config)}
        for agent_id, workspace_dir in _iter_qwenpaw_agents()
    ]
    return {"ok": True, "asset": "team-context", "targets": targets}


def _sanitizer_rules() -> List[str]:
    config = load_runtime_config()
    desired = _section(config, "desired")
    policy = _section(desired, "outputSanitize")
    rules = _string_list(policy.get("keywords"))
    credentials = _section(config, "credentials")
    env_refs = _string_list(policy.get("envRefs"))
    for key in ("matrixTokenEnv", "gatewayKeyEnv", "storageAccessKeyEnv", "storageSecretKeyEnv"):
        value = _string(credentials.get(key))
        if value:
            env_refs.append(value)
    for env_name in env_refs:
        value = os.getenv(env_name, "")
        if len(value) >= 4:
            rules.append(value)
    deduped = []
    seen = set()
    for rule in rules:
        if rule and rule not in seen:
            seen.add(rule)
            deduped.append(rule)
    return deduped


def _qwenpaw_working_dir() -> Optional[Path]:
    raw = os.getenv("QWENPAW_WORKING_DIR", "").strip() or os.getenv("COPAW_WORKING_DIR", "").strip()
    if raw:
        return Path(raw).expanduser()
    home = os.getenv("HOME", "").strip()
    return Path(home).expanduser() / ".qwenpaw" if home else None


def _credagent_paths() -> List[Path]:
    paths: List[Path] = []
    for _agent_id, workspace_dir in _iter_qwenpaw_agents():
        paths.append(workspace_dir / "config" / "credagent.json")
    for env_name in ("HICLAW_AGENT_HOME", "HICLAW_WORKER_HOME"):
        raw = os.getenv(env_name, "").strip()
        if raw:
            paths.append(Path(raw).expanduser() / "config" / "credagent.json")
    working_dir = _qwenpaw_working_dir()
    if working_dir is not None:
        paths.append(working_dir / "workspaces" / "default" / "config" / "credagent.json")
        paths.append(working_dir.parent / "config" / "credagent.json")

    deduped: List[Path] = []
    seen = set()
    for path in paths:
        key = str(path)
        if key not in seen:
            seen.add(key)
            deduped.append(path)
    return deduped


def _credagent_specs() -> List[Dict[str, Any]]:
    specs = []
    for path in _credagent_paths():
        if not path.exists():
            continue
        spec = _read_json(path)
        if spec:
            specs.append(spec)
    return specs


def _normalize_credentials(specs: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    normalized: List[Dict[str, Any]] = []
    for spec in specs:
        credentials = spec.get("credentials", [])
        if not isinstance(credentials, list):
            continue
        for entry in credentials:
            if not isinstance(entry, dict):
                continue
            raw = _string(entry.get("path"))
            if not raw:
                continue
            expanded = str(Path(raw).expanduser())
            if raw.endswith("/") and not expanded.endswith("/"):
                expanded += "/"
            permit = entry.get("programPermit", [])
            if isinstance(permit, str):
                permit = [permit]
            elif not isinstance(permit, list):
                permit = []
            normalized.append(
                {
                    "path": expanded,
                    "programPermit": [str(item) for item in permit],
                    "writable": bool(entry.get("writable", False)),
                }
            )
    return normalized


def _credagent_output_sanitize_rules(specs: Optional[List[Dict[str, Any]]] = None) -> List[Dict[str, Any]]:
    rules: List[Dict[str, Any]] = []
    for spec in specs if specs is not None else _credagent_specs():
        raw_rules = spec.get("output_sanitize", [])
        if not isinstance(raw_rules, list):
            continue
        rules.extend(rule for rule in raw_rules if isinstance(rule, dict))
    return rules


def _sanitizer_patterns() -> List[tuple[re.Pattern[str], str]]:
    patterns = list(_BUILTIN_SANITIZER_PATTERNS)
    for rule in _credagent_output_sanitize_rules():
        try:
            rule_type = rule.get("type", "")
            if rule_type == "prefix":
                prefix = str(rule["prefix"])
                min_length = int(rule.get("min_length", 16))
                suffix_len = max(min_length - len(prefix), 1)
                escaped = re.escape(prefix)
                patterns.append((re.compile(rf"\b({escaped})[A-Za-z0-9]{{{suffix_len},}}"), r"\1****"))
            elif rule_type == "keyword":
                keywords = rule.get("keywords", [])
                if not isinstance(keywords, list) or not keywords:
                    continue
                kw_alt = "|".join(re.escape(str(keyword)) for keyword in keywords)
                patterns.append(
                    (
                        re.compile(rf"""(?i)({kw_alt})(["']?[\s]*[=:]\s*["']?)([A-Za-z0-9/+=]{{16,}})"""),
                        r"\1\2********",
                    )
                )
            elif rule_type == "regex":
                patterns.append((re.compile(str(rule["pattern"])), str(rule.get("replacement", "********"))))
        except (KeyError, TypeError, ValueError, re.error):
            continue
    return patterns


def _sanitize_text_with_rules(
    text: str,
    exact_rules: List[str],
    patterns: List[tuple[re.Pattern[str], str]],
) -> str:
    sanitized = text
    for rule in sorted(exact_rules, key=len, reverse=True):
        sanitized = re.sub(re.escape(rule), REDACTION, sanitized)
    for pattern, replacement in patterns:
        sanitized = pattern.sub(replacement, sanitized)
    return sanitized


def sanitize_text(text: str) -> str:
    return _sanitize_text_with_rules(text, _sanitizer_rules(), _sanitizer_patterns())


def _qwenpaw_config_path() -> Optional[Path]:
    try:
        from qwenpaw.config import get_config_path

        return Path(get_config_path())
    except Exception:
        working_dir = _qwenpaw_working_dir()
        return working_dir / "config.json" if working_dir is not None else None


def _reload_qwenpaw_file_guard() -> None:
    try:
        from qwenpaw.security.tool_guard.engine import get_guard_engine

        get_guard_engine().reload_rules()
    except Exception:
        return


def apply_credential_guard() -> Dict[str, Any]:
    specs = _credagent_specs()
    if not specs:
        return {
            "ok": True,
            "hook": "credential-guard",
            "applied": False,
            "credentials": 0,
            "outputSanitizeRules": 0,
            "reason": "credagent.json not found",
        }

    credentials = _normalize_credentials(specs)
    paths = {entry["path"] for entry in credentials}
    rules_count = len(_credagent_output_sanitize_rules(specs))
    config_path = _qwenpaw_config_path()
    if config_path is None:
        return {
            "ok": True,
            "hook": "credential-guard",
            "applied": False,
            "credentials": len(credentials),
            "outputSanitizeRules": rules_count,
            "reason": "qwenpaw config path unavailable",
        }

    config = _read_json(config_path)
    security = config.setdefault("security", {})
    plugins = config.setdefault("plugins", {})
    teamharness = plugins.setdefault("teamharness", {})
    guard_state = teamharness.setdefault("credentialGuard", {})
    previous_paths = {
        str(path)
        for path in (guard_state.get("paths", []) or [])
        if str(path)
    }

    file_guard = security.setdefault("file_guard", {})
    file_guard["enabled"] = True
    existing = {
        str(path)
        for path in (file_guard.get("sensitive_files", []) or [])
        if str(path)
    } - previous_paths
    existing.update(paths)
    file_guard["sensitive_files"] = sorted(existing)

    tool_guard = security.setdefault("tool_guard", {})
    tool_guard["enabled"] = True
    auto_rules = [
        str(rule)
        for rule in (tool_guard.get("auto_denied_rules", []) or [])
        if str(rule)
    ]
    if SENSITIVE_FILE_AUTO_DENY_RULE not in auto_rules:
        auto_rules.append(SENSITIVE_FILE_AUTO_DENY_RULE)
    tool_guard["auto_denied_rules"] = auto_rules

    guard_state["paths"] = sorted(paths)
    guard_state["source"] = "credagent.json"
    _write_json_atomic(config_path, config)
    _reload_qwenpaw_file_guard()
    return {
        "ok": True,
        "hook": "credential-guard",
        "applied": True,
        "credentials": len(credentials),
        "outputSanitizeRules": rules_count,
        "runtimeConfig": str(config_path),
        "reason": None,
    }


def sanitize_tool_result(result: Any) -> Dict[str, Any]:
    summary = {"ok": True, "hook": "output-sanitizer", "redacted": False, "blocks": 0}
    exact_rules = _sanitizer_rules()
    patterns = _sanitizer_patterns()
    if isinstance(result, dict):
        content = result.get("content") or result.get("output")
    else:
        content = getattr(result, "content", None) or getattr(result, "output", None)
    if isinstance(content, str):
        sanitized = _sanitize_text_with_rules(content, exact_rules, patterns)
        if sanitized != content:
            if isinstance(result, dict):
                result["content"] = sanitized
            else:
                setattr(result, "content", sanitized)
            summary["redacted"] = True
        summary["blocks"] = 1
        return summary
    if not isinstance(content, list):
        return summary

    for block in content:
        if isinstance(block, dict):
            text = block.get("text")
        else:
            text = getattr(block, "text", None)
        if not isinstance(text, str):
            continue
        summary["blocks"] += 1
        sanitized = _sanitize_text_with_rules(text, exact_rules, patterns)
        if sanitized != text:
            if isinstance(block, dict):
                block["text"] = sanitized
            else:
                setattr(block, "text", sanitized)
            summary["redacted"] = True
    return summary


def _iter_qwenpaw_agents() -> List[tuple[str, Path]]:
    try:
        from qwenpaw.config.utils import load_config
    except ImportError:
        workspace = os.getenv("QWENPAW_WORKSPACE_DIR", "").strip()
        if not workspace:
            qwenpaw_dir = os.getenv("QWENPAW_WORKING_DIR", "").strip()
            workspace = str(Path(qwenpaw_dir) / "workspaces" / "default") if qwenpaw_dir else ""
        return [("default", Path(workspace))] if workspace else []

    root = load_config()
    profiles = getattr(getattr(root, "agents", None), "profiles", {}) or {}
    agents = []
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


def _skill_entries() -> List[Dict[str, Any]]:
    manifest = _read_yaml(ASSET_DIR / "plugin.yaml")
    skills = _section(manifest, "skills")
    entries = []
    for group in ("agent", "team"):
        for entry in skills.get(group) or []:
            if isinstance(entry, dict):
                entries.append(entry)
    return entries


def _role_aliases(role: str) -> set[str]:
    normalized = _string(role) or "worker"
    aliases = {normalized}
    if normalized == "team_leader":
        aliases.add("leader")
    if normalized == "remote_member":
        aliases.add("remote-member")
    return aliases


def _skill_names_for_role(role: str) -> List[str]:
    role_aliases = _role_aliases(role)
    names = []
    for entry in _skill_entries():
        skill_id = _string(entry.get("id"))
        if not skill_id:
            continue
        roles = _string_list(entry.get("roles"))
        if roles and not role_aliases.intersection(roles):
            continue
        names.append(f"teamharness-{skill_id}")
    return names


def _enable_workspace_skills(workspace_dir: Path, role: str) -> Dict[str, Any]:
    installed = []
    failed = []
    try:
        from qwenpaw.agents.skill_system.pool_service import SkillPoolService
    except ImportError:
        return {"installed": installed, "skipped": "qwenpaw skill pool API unavailable"}

    service = SkillPoolService()
    for skill_name in _skill_names_for_role(role):
        result = service.download_to_workspace(skill_name, workspace_dir, overwrite=True)
        if result.get("success"):
            installed.append(skill_name)
        else:
            failed.append({"name": skill_name, "reason": result.get("reason") or "unknown"})
    return {"installed": installed, "failed": failed}


def _install_workspace_assets(agent_id: str, workspace_dir: Path, config: Dict[str, Any]) -> Dict[str, Any]:
    member = _section(config, "member")
    role = _string(member.get("role") or os.getenv("HICLAW_AGENT_ROLE") or "worker")
    workspace_dir.mkdir(parents=True, exist_ok=True)
    team_context = _write_team_context(workspace_dir, config)
    prompts = _ensure_prompt_files(agent_id, [TEAMS_PROMPT_FILE])
    skills = _enable_workspace_skills(workspace_dir, role)
    return {
        "agent": agent_id,
        "workspace": str(workspace_dir),
        "role": role,
        "teamContext": team_context,
        "prompts": prompts,
        "skills": skills,
    }


def _ensure_prompt_files(agent_id: str, prompt_files: List[str]) -> Dict[str, Any]:
    try:
        from qwenpaw.config.config import load_agent_config, save_agent_config
    except ImportError:
        return {"agent": agent_id, "skipped": "qwenpaw config API unavailable"}

    agent_config = load_agent_config(agent_id)
    existing = list(agent_config.system_prompt_files or [])
    changed = False
    for prompt_file in prompt_files:
        if prompt_file not in existing:
            existing.append(prompt_file)
            changed = True
    if changed:
        agent_config.system_prompt_files = existing
        save_agent_config(agent_id, agent_config)
    return {"agent": agent_id, "files": existing, "action": "updated" if changed else "unchanged"}


def _install_skills() -> Dict[str, Any]:
    installed = []
    try:
        from qwenpaw.agents.skill_system.registry import ensure_skill_pool_initialized, reconcile_pool_manifest
        from qwenpaw.agents.skill_system.store import get_skill_pool_dir
    except ImportError:
        return {"installed": installed, "skipped": "qwenpaw skill API unavailable"}

    ensure_skill_pool_initialized()
    pool_dir = get_skill_pool_dir()
    pool_dir.mkdir(parents=True, exist_ok=True)
    for entry in _skill_entries():
        source = ASSET_DIR / str(entry.get("path") or "")
        skill_id = _string(entry.get("id"))
        if not skill_id or not source.exists():
            continue
        target = pool_dir / f"teamharness-{skill_id}"
        _copytree_replace(source, target)
        installed.append(target.name)
    reconcile_pool_manifest()
    return {"installed": installed}


def _mcp_client_env() -> Dict[str, str]:
    env = {"TEAMHARNESS_SHARED_DIR": str(_shared_dir())}
    for name in (
        "TEAMHARNESS_RUNTIME_CONFIG",
        "HICLAW_MATRIX_URL",
        "HICLAW_WORKER_MATRIX_TOKEN",
        "HICLAW_MATRIX_USER_ID",
        "HICLAW_WORKER_ROLE",
        "HICLAW_AGENT_ROLE",
        "HICLAW_WORKER_NAME",
        "HICLAW_STORAGE_PREFIX",
        "HICLAW_SHARED_STORAGE_PREFIX",
        "HICLAW_FS_BUCKET",
        "HICLAW_CONTROLLER_URL",
        "HICLAW_AUTH_TOKEN_FILE",
        "HICLAW_CLUSTER_ID",
        "HICLAW_FS_ENDPOINT",
        "HICLAW_FS_ACCESS_KEY",
        "HICLAW_FS_SECRET_KEY",
        "MC_HOST_hiclaw",
        "QWENPAW_WORKING_DIR",
        "COPAW_WORKING_DIR",
    ):
        value = os.getenv(name, "").strip()
        if value:
            env[name] = value
    return env


def _ensure_mcp_client(agent_id: str, workspace_dir: Path) -> Dict[str, Any]:
    try:
        from qwenpaw.config.config import MCPClientConfig, MCPConfig, load_agent_config, save_agent_config
    except ImportError:
        marker = workspace_dir / ".teamharness" / "mcp-client.json"
        marker.parent.mkdir(parents=True, exist_ok=True)
        marker.write_text(json.dumps({"id": MCP_CLIENT_ID}, indent=2), encoding="utf-8")
        return {"agent": agent_id, "action": "marker", "path": str(marker)}

    agent_config = load_agent_config(agent_id)
    if agent_config.mcp is None:
        agent_config.mcp = MCPConfig()
    server_path = ASSET_DIR / "mcp" / "server.py"
    client = MCPClientConfig(
        name=MCP_CLIENT_ID,
        description="TeamHarness collaboration MCP server",
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


def apply_teamharness() -> Dict[str, Any]:
    config = load_runtime_config()
    credential_guard = apply_credential_guard()
    skills = _install_skills()
    agents = []
    mcp = []
    for agent_id, workspace_dir in _iter_qwenpaw_agents():
        agents.append(_install_workspace_assets(agent_id, workspace_dir, config))
        mcp.append(_ensure_mcp_client(agent_id, workspace_dir))
    return {"ok": True, "agents": agents, "skills": skills, "mcp": mcp, "credentialGuard": credential_guard}


def install_output_sanitizer_wrapper() -> Dict[str, Any]:
    try:
        from qwenpaw.agents.react_agent import QwenPawAgent
    except ImportError:
        return {"ok": True, "installed": False, "reason": "qwenpaw agent API unavailable"}
    if getattr(QwenPawAgent, "_teamharness_sanitizer_installed", False):
        return {"ok": True, "installed": True, "action": "unchanged"}

    original = getattr(QwenPawAgent, "_acting", None)
    if not callable(original):
        return {"ok": True, "installed": False, "reason": "qwenpaw agent _acting hook unavailable"}

    async def _acting_with_sanitizer(self, tool_call):
        result = await original(self, tool_call)
        sanitize_tool_result(result)
        return result

    QwenPawAgent._acting = _acting_with_sanitizer
    QwenPawAgent._teamharness_sanitizer_installed = True
    return {"ok": True, "installed": True, "action": "created"}


def _task_trace_module_path() -> Optional[Path]:
    candidate = PLUGIN_DIR / "task_trace.py"
    return candidate if candidate.exists() else None


def _load_task_trace_module() -> Tuple[Optional[Any], Optional[str]]:
    global _TASK_TRACE_MODULE
    if _TASK_TRACE_MODULE is not None:
        return _TASK_TRACE_MODULE, None

    trace_module_path = _task_trace_module_path()
    if trace_module_path is None:
        return None, "task_trace.py not found"

    try:
        import importlib.util

        spec = importlib.util.spec_from_file_location("teamharness_qwenpaw_task_trace", trace_module_path)
        if spec is None or spec.loader is None:
            return None, "cannot load trace module"
        module = importlib.util.module_from_spec(spec)
        sys.modules["teamharness_qwenpaw_task_trace"] = module
        spec.loader.exec_module(module)
    except Exception as exc:
        sys.modules.pop("teamharness_qwenpaw_task_trace", None)
        return None, str(exc)

    _TASK_TRACE_MODULE = module
    return module, None


def _room_id_from_request_context(context: Any) -> str:
    if not isinstance(context, dict):
        return ""
    for key in ("room_id", "roomId"):
        room_id = _string(context.get(key))
        if room_id:
            return room_id
    meta = context.get("channel_meta")
    if isinstance(meta, dict):
        room_id = _string(meta.get("room_id") or meta.get("roomId"))
        if room_id:
            return room_id
    session_id = _string(context.get("session_id"))
    if session_id.startswith("matrix:"):
        return session_id[len("matrix:") :]
    if _string(context.get("channel")) == "matrix":
        return _string(context.get("user_id"))
    return ""


def _task_trace_debug_enabled() -> bool:
    return os.getenv("AGENTTEAMS_TASK_TRACE_DEBUG", "").strip().lower() in {"1", "true", "yes"}


def _canonical_task_trace_room(trace_module: Any, room_id: str) -> str:
    canonicalize = getattr(trace_module, "canonical_room_id", None)
    if callable(canonicalize):
        try:
            return str(canonicalize(room_id) or "").strip()
        except Exception:
            return _string(room_id)
    return _string(room_id)


def _refresh_shared_tasks_for_task_trace(trace_module: Any, room_id: str) -> Dict[str, Any]:
    """Best-effort pull of shared task metadata before tagging a turn span."""
    room = _canonical_task_trace_room(trace_module, room_id)
    if not room:
        return {"ok": True, "action": "skipped", "reason": "missing room"}

    qwenpaw_dir = _qwenpaw_working_dir()
    if qwenpaw_dir is None:
        return {"ok": False, "reason": "workspace dir unavailable"}

    shared_dir = _shared_dir()
    shared_prefix = os.getenv("AGENTTEAMS_SHARED_STORAGE_PREFIX", "").strip().strip("/") or "shared"
    remote_prefix = f"{shared_prefix}/tasks"
    local_tasks_dir = shared_dir / "tasks"
    worker_name = (
        os.getenv("AGENTTEAMS_WORKER_NAME", "").strip()
        or os.getenv("AGENTTEAMS_AGENT_NAME", "").strip()
        or "qwenpaw"
    )

    try:
        from qwenpaw_worker.sync import FileSync

        sync = FileSync(
            endpoint=os.getenv("AGENTTEAMS_FS_ENDPOINT", ""),
            access_key=os.getenv("AGENTTEAMS_FS_ACCESS_KEY", ""),
            secret_key=os.getenv("AGENTTEAMS_FS_SECRET_KEY", ""),
            bucket=os.getenv("AGENTTEAMS_FS_BUCKET", "agentteams-storage"),
            worker_name=worker_name,
            local_dir=qwenpaw_dir.parent,
            shared_dir=shared_dir,
            shared_prefix=shared_prefix,
        )
        sync.mirror_prefix(remote_prefix, local_tasks_dir)
    except Exception as exc:
        if _task_trace_debug_enabled():
            logger.warning(
                "AgentTeamsTaskSpanProcessor shared tasks refresh failed room=%s remote=%s local=%s error_type=%s error=%s",
                room,
                remote_prefix,
                local_tasks_dir,
                type(exc).__name__,
                exc,
            )
        return {"ok": False, "reason": str(exc), "remote": remote_prefix}

    if _task_trace_debug_enabled():
        logger.info(
            "AgentTeamsTaskSpanProcessor shared tasks refreshed room=%s remote=%s local=%s",
            room,
            remote_prefix,
            local_tasks_dir,
        )
    return {"ok": True, "action": "refreshed", "remote": remote_prefix, "local": str(local_tasks_dir)}


def install_task_trace_context_wrapper(trace_module: Any) -> Dict[str, Any]:
    try:
        from qwenpaw.agents.react_agent import QwenPawAgent
    except ImportError:
        return {"ok": True, "installed": False, "reason": "qwenpaw agent API unavailable"}
    if getattr(QwenPawAgent, "_teamharness_trace_context_installed", False):
        return {"ok": True, "installed": True, "action": "unchanged"}

    original_call = getattr(QwenPawAgent, "__call__", None)
    original_reply = getattr(QwenPawAgent, "reply", None)
    if not callable(original_call) or not callable(original_reply):
        return {"ok": False, "installed": False, "reason": "qwenpaw agent call API unavailable"}

    def _set_room_context(agent: Any):
        room_id = _room_id_from_request_context(getattr(agent, "_request_context", {}) or {})
        return (trace_module.set_current_room(room_id) if room_id else None, room_id)

    def _tag_current_entry_span() -> None:
        workspace_dir = _qwenpaw_working_dir()
        if workspace_dir is None:
            return
        default_ws = workspace_dir / "workspaces" / "default"
        tag_pending = getattr(trace_module, "tag_pending_entry_span", None)
        if callable(tag_pending):
            get_pending = getattr(trace_module, "get_pending_entry_span", None)
            if callable(get_pending) and get_pending() is not None:
                tag_pending(default_ws)
                return
        tag_current = getattr(trace_module, "tag_current_entry_span", None)
        if callable(tag_current):
            tag_current(default_ws)

    async def _call_with_task_trace_context(self, *args, **kwargs):
        token, room_id = _set_room_context(self)
        try:
            _refresh_shared_tasks_for_task_trace(trace_module, room_id)
            _tag_current_entry_span()
            _retry_task_trace_registration(trace_module)
            return await original_call(self, *args, **kwargs)
        finally:
            if token is not None:
                trace_module.reset_current_room(token)

    async def _reply_with_task_trace_context(self, *args, **kwargs):
        token, room_id = _set_room_context(self)
        try:
            _refresh_shared_tasks_for_task_trace(trace_module, room_id)
            _tag_current_entry_span()
            _retry_task_trace_registration(trace_module)
            return await original_reply(self, *args, **kwargs)
        finally:
            if token is not None:
                trace_module.reset_current_room(token)

    QwenPawAgent.__call__ = _call_with_task_trace_context
    QwenPawAgent.reply = _reply_with_task_trace_context
    QwenPawAgent._teamharness_trace_context_installed = True
    QwenPawAgent._teamharness_trace_original_call = original_call
    QwenPawAgent._teamharness_trace_original_reply = original_reply
    return {"ok": True, "installed": True, "action": "created"}


def _retry_task_trace_registration(trace_module: Any) -> None:
    if _TASK_TRACE_REGISTERED:
        return
    workspace_dir = _qwenpaw_working_dir()
    if workspace_dir is None:
        return
    result = _register_task_trace_processor(trace_module, workspace_dir / "workspaces" / "default")
    if result.get("ok"):
        return
    reason = str(result.get("reason") or "unknown")
    logger.info("AgentTeamsTaskSpanProcessor lazy registration skipped: %s", reason)


def _register_task_trace_processor(trace_module: Any, workspace_dir: Path) -> Dict[str, Any]:
    global _TASK_TRACE_REGISTERED
    if _TASK_TRACE_REGISTERED:
        return {"ok": True, "processor": "AgentTeamsTaskSpanProcessor", "action": "unchanged"}
    result = trace_module.register_task_trace_processor(workspace_dir)
    if isinstance(result, dict) and result.get("ok"):
        _TASK_TRACE_REGISTERED = True
    return result if isinstance(result, dict) else {"ok": False, "reason": "invalid registration result"}


def install_task_trace_processor() -> Dict[str, Any]:
    """Register the AgentTeams Task-Trace SpanProcessor on the active TracerProvider."""
    workspace_dir = _qwenpaw_working_dir()
    if workspace_dir is None:
        return {"ok": False, "reason": "workspace dir unavailable"}
    default_workspace = workspace_dir / "workspaces" / "default"

    module, error = _load_task_trace_module()
    if module is None:
        return {"ok": False, "reason": error or "cannot load trace module"}

    try:
        context_result = install_task_trace_context_wrapper(module)
        result = _register_task_trace_processor(module, default_workspace)
        if isinstance(result, dict):
            result = dict(result)
            result["context"] = context_result
            if not result.get("ok"):
                logger.warning(
                    "AgentTeamsTaskSpanProcessor not registered: %s",
                    result.get("reason") or "unknown",
                )
        return result
    except Exception as exc:
        logger.warning("Failed to register AgentTeamsTaskSpanProcessor: %s", exc)
        return {"ok": False, "reason": str(exc)}


class TeamHarnessPlugin:
    def __init__(self) -> None:
        self.last_apply_result: Dict[str, Any] = {}
        self.sanitizer_result: Dict[str, Any] = {}
        self.trace_result: Dict[str, Any] = {}

    def _sync_runtime(self) -> Dict[str, Any]:
        self.last_apply_result = apply_teamharness()
        self.sanitizer_result = install_output_sanitizer_wrapper()
        self.trace_result = install_task_trace_processor()
        return self.last_apply_result

    def register(self, api: Any) -> None:
        def sync() -> Dict[str, Any]:
            return self._sync_runtime()

        def shutdown() -> None:
            return None

        api.register_startup_hook("teamharness_sync", sync, priority=40)
        api.register_shutdown_hook("teamharness_shutdown", shutdown, priority=100)
        self._register_http(api)

    def _register_http(self, api: Any) -> None:
        try:
            from fastapi import APIRouter
        except Exception:
            return
        router = APIRouter()

        @router.get("/health")
        def health() -> Dict[str, Any]:
            return {"ok": True, "plugin": "teamharness", "adapter": "qwenpaw"}

        @router.get("/status")
        def status() -> Dict[str, Any]:
            return {
                "ok": True,
                "lastApply": self.last_apply_result,
                "sanitizer": self.sanitizer_result,
                "trace": self.trace_result,
            }

        @router.post("/sync")
        def sync_endpoint() -> Dict[str, Any]:
            return self._sync_runtime()

        api.register_http_router(router, prefix="/teamharness", tags=["teamharness"])


plugin = TeamHarnessPlugin()
