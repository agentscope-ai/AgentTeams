"""Bridge: openclaw.json → CLI adapter env + prompt context.

The Manager publishes per-worker ``openclaw.json`` with the Matrix channel
config and the LLM provider (models.providers.<pid> with baseUrl/apiKey).
This module extracts the provider config so CLI coding agents can be pointed
at the Higress AI gateway with plain environment variables, and assembles the
per-message prompt context (SOUL.md) that headless CLIs would otherwise miss.
"""
from __future__ import annotations

import os
from pathlib import Path
from typing import Any, Dict, Optional


def _is_in_container() -> bool:
    return Path("/.dockerenv").exists() or Path("/run/.containerenv").exists()


def _port_remap(url: str, is_container: bool) -> str:
    """Remap container-internal :8080 to the host-exposed gateway port.

    Only relevant when running the harness on the host (dev); inside a
    container the published :8080 baseUrls are directly routable.
    """
    if not is_container and url and ":8080" in url:
        gateway_port = os.environ.get("AGENTTEAMS_PORT_GATEWAY", "18080")
        return url.replace(":8080", f":{gateway_port}")
    return url


def resolve_active_model(cfg: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    """Return the active model + parent provider from openclaw.json.

    Selection order mirrors the hermes bridge:
      1. ``agents.defaults.model.primary`` ("provider_id/model_id")
      2. First model of the first provider (deterministic dict ordering)

    The returned dict is augmented with ``_provider`` (baseUrl/apiKey source).
    """
    providers_raw = cfg.get("models", {}).get("providers", {})
    if not providers_raw:
        return None

    primary = (
        cfg.get("agents", {})
        .get("defaults", {})
        .get("model", {})
        .get("primary", "")
    )
    if primary and "/" in primary:
        pid, mid = primary.split("/", 1)
        provider = providers_raw.get(pid, {})
        for m in provider.get("models", []):
            if m.get("id") == mid:
                return {**m, "_provider": provider, "_provider_id": pid}

    for provider_cfg in providers_raw.values():
        models = provider_cfg.get("models", [])
        if models:
            return {**models[0], "_provider": provider_cfg}

    return None


def resolve_matrix_config(cfg: Dict[str, Any]) -> Dict[str, Any]:
    return cfg.get("channels", {}).get("matrix", {}) or {}


def adapter_env(cfg: Dict[str, Any], adapter) -> Dict[str, str]:
    """Provider env vars for ``adapter`` from openclaw.json."""
    model = resolve_active_model(cfg) or {}
    provider = model.get("_provider", {}) or {}
    base_url = _port_remap(provider.get("baseUrl", ""), _is_in_container())
    api_key = provider.get("apiKey", "")
    model_id = model.get("id", "")
    env = adapter.required_env(model_id, base_url, api_key)
    if model_id and not os.environ.get("AGENTTEAMS_CLI_MODEL"):
        env.setdefault("AGENTTEAMS_CLI_MODEL", model_id)
    return env


def build_prompt(task: str, soul: Optional[str] = None) -> str:
    """Assemble the headless prompt: task text plus SOUL.md context.

    Most CLI agents already pick AGENTS.md / CLAUDE.md up from the working
    directory; SOUL.md is AgentTeams-specific, so it is prepended here.
    """
    parts = []
    if soul:
        parts.append(
            "<agent-identity>\n"
            "You are an AgentTeams Worker. Follow this identity while working:\n"
            f"{soul.strip()}\n"
            "</agent-identity>"
        )
    parts.append(task.strip())
    return "\n\n".join(parts)
