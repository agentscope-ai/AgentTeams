"""Bridge openclaw.json (AgentTeams Worker config) into OpenHuman config.toml.

Mirrors the semantics previously implemented in openhuman-worker-entrypoint.sh:
Matrix settings from ``channels.matrix`` with env-var fallbacks, and LLM routing
through the HiClaw AI gateway (``models.providers["hiclaw-gateway"]``).
"""
from __future__ import annotations

import json
import logging
import os
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Mapping, Optional

logger = logging.getLogger(__name__)

_HICLAW_GATEWAY_PROVIDER = "hiclaw-gateway"
_PRIMARY_MODEL_RE = re.compile(rf"^{re.escape(_HICLAW_GATEWAY_PROVIDER)}/")


@dataclass(frozen=True)
class LlmConfig:
    base_url: str
    api_key: str
    default_model: str


@dataclass(frozen=True)
class BridgeResult:
    config_toml: str
    llm: Optional[LlmConfig]


def _is_in_container() -> bool:
    return Path("/.dockerenv").exists() or Path("/run/.containerenv").exists()


def _port_remap(url: str, is_container: bool, env: Mapping[str, str]) -> str:
    """Remap container-internal :8080 to host gateway port when running on host."""
    if not is_container and url and ":8080" in url:
        gateway_port = env.get("AGENTTEAMS_PORT_GATEWAY", "18080")
        return url.replace(":8080", f":{gateway_port}")
    return url


def _load_openclaw(path: Path) -> Dict[str, Any]:
    if not path.is_file():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        logger.warning("Failed to read openclaw.json at %s: %s", path, exc)
        return {}
    return data if isinstance(data, dict) else {}


def _merge_allowed_users(matrix_cfg: Mapping[str, Any]) -> List[str]:
    dm_allow = (matrix_cfg.get("dm") or {}).get("allowFrom") or []
    group_allow = matrix_cfg.get("groupAllowFrom") or []
    seen: set[str] = set()
    merged: List[str] = []
    for raw in list(dm_allow) + list(group_allow):
        if not isinstance(raw, str):
            continue
        value = raw.strip()
        if not value or value in seen:
            continue
        seen.add(value)
        merged.append(value)
    return merged


def _toml_string(value: str) -> str:
    escaped = value.replace("\\", "\\\\").replace('"', '\\"')
    return f'"{escaped}"'


def _format_allowed_users(users: List[str]) -> str:
    if not users:
        return ""
    lines = [f"  {_toml_string(user)}," for user in users]
    if lines:
        lines[-1] = lines[-1].rstrip(",")
    return "\n".join(lines)


def _matrix_from_openclaw(
    cfg: Mapping[str, Any],
    *,
    env: Mapping[str, str],
) -> Dict[str, Any]:
    matrix_raw = (cfg.get("channels") or {}).get("matrix") or {}
    in_container = _is_in_container()

    homeserver = _port_remap(
        matrix_raw.get("homeserver") or env.get("MATRIX_HOMESERVER_URL", ""),
        in_container,
        env,
    )
    access_token = matrix_raw.get("accessToken") or env.get("MATRIX_ACCESS_TOKEN", "")
    user_id = matrix_raw.get("userId") or env.get("MATRIX_USER_ID", "")
    room_id = env.get("MATRIX_HOME_ROOM_ID", "")
    device_id = env.get("MATRIX_DEVICE_ID", "")

    allowed_users = _merge_allowed_users(matrix_raw)
    if not allowed_users:
        raw_allowed = env.get("MATRIX_ALLOWED_USERS", "")
        if raw_allowed:
            allowed_users = [
                part.strip()
                for part in raw_allowed.split(",")
                if part.strip()
            ]

    return {
        "homeserver": homeserver,
        "access_token": access_token,
        "room_id": room_id,
        "user_id": user_id,
        "device_id": device_id,
        "allowed_users": allowed_users,
    }


def _llm_from_openclaw(
    cfg: Mapping[str, Any],
    *,
    env: Mapping[str, str],
) -> Optional[LlmConfig]:
    in_container = _is_in_container()
    providers = (cfg.get("models") or {}).get("providers") or {}
    gateway = providers.get(_HICLAW_GATEWAY_PROVIDER) or {}

    base_url = _port_remap(gateway.get("baseUrl") or "", in_container, env)
    api_key = gateway.get("apiKey") or ""

    primary = (
        ((cfg.get("agents") or {}).get("defaults") or {})
        .get("model", {})
        .get("primary", "")
    )
    default_model = _PRIMARY_MODEL_RE.sub("", primary) if primary else ""

    if not base_url:
        gateway_url = (env.get("AGENTTEAMS_AI_GATEWAY_URL") or "").rstrip("/")
        if gateway_url:
            base_url = f"{gateway_url}/v1"
    if not api_key:
        api_key = env.get("AGENTTEAMS_WORKER_GATEWAY_KEY", "")
    if not default_model:
        default_model = env.get("AGENTTEAMS_DEFAULT_MODEL", "qwen-plus")

    if not base_url or not api_key:
        return None
    return LlmConfig(base_url=base_url, api_key=api_key, default_model=default_model)


def render_config_toml(matrix: Mapping[str, Any]) -> str:
    """Render the Matrix section of OpenHuman config.toml."""
    allowed_block = _format_allowed_users(list(matrix.get("allowed_users") or []))
    optional_lines: List[str] = []
    user_id = matrix.get("user_id") or ""
    device_id = matrix.get("device_id") or ""
    if user_id:
        optional_lines.append(f'user_id = {_toml_string(str(user_id))}')
    if device_id:
        optional_lines.append(f'device_id = {_toml_string(str(device_id))}')

    optional_block = ""
    if optional_lines:
        optional_block = "\n" + "\n".join(optional_lines)

    return (
        "# Auto-generated by openhuman-worker bridge\n"
        "# Do not edit manually — changes will be overwritten on container restart.\n"
        "\n"
        "[channels_config]\n"
        "\n"
        "[channels_config.matrix]\n"
        f"homeserver = {_toml_string(str(matrix.get('homeserver') or ''))}\n"
        f"access_token = {_toml_string(str(matrix.get('access_token') or ''))}\n"
        f"room_id = {_toml_string(str(matrix.get('room_id') or ''))}\n"
        "allowed_users = [\n"
        f"{allowed_block}\n"
        f"]{optional_block}\n"
    )


def bridge_openclaw_to_openhuman(
    openclaw_cfg: Mapping[str, Any],
    *,
    env: Optional[Mapping[str, str]] = None,
) -> BridgeResult:
    """Translate openclaw config dict into OpenHuman bridge outputs."""
    env_map = dict(os.environ if env is None else env)
    matrix = _matrix_from_openclaw(openclaw_cfg, env=env_map)
    llm = _llm_from_openclaw(openclaw_cfg, env=env_map)
    config_toml = render_config_toml(matrix)
    return BridgeResult(config_toml=config_toml, llm=llm)


def bridge_openclaw_file(
    openclaw_json: Path,
    *,
    env: Optional[Mapping[str, str]] = None,
) -> BridgeResult:
    """Load openclaw.json from disk and bridge it."""
    return bridge_openclaw_to_openhuman(_load_openclaw(openclaw_json), env=env)


def write_config_toml(workspace: Path, result: BridgeResult) -> Path:
    """Write bridged config.toml under workspace."""
    workspace.mkdir(parents=True, exist_ok=True)
    path = workspace / "config.toml"
    path.write_text(result.config_toml, encoding="utf-8")
    return path
