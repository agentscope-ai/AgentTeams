"""
Bridge: translate openclaw.json (AgentTeams Worker config) into CoPaw's
config.json + providers.json + workspaces/default/agent.json.

Two phases (W8.2):
  ``bridge_config`` — pure JSON writes; safe to call on every openclaw.json change.
  ``bootstrap_copaw_runtime`` — env vars + copaw module path patches; once per process.

AgentTeams Matrix stream-filter defaults (SSOT for workers):
  filter_tool_messages = True  — hide tool-call chatter from synced history
  filter_thinking = True       — hide thinking blocks from synced history
Templates under templates/agent.*.json and bridge overlay both use these values.

Matrix fields in config.json vs agent.json (W8.3, CoPaw 1.0.2+):
  - ``workspaces/default/agent.json`` is the per-agent SSOT for the agent loop
    (Matrix homeserver/token/allowlists, running.max_input_length, heartbeat).
  - ``config.json`` → ``channels.matrix`` is still written for ChannelManager /
    global config load paths and backward compatibility with older CoPaw reads.
  - Both receive the same controller-owned Matrix scalars today; deduplicating
    config.json is deferred until ChannelManager is verified to read agent.json only.
  - Root ``show_tool_details`` lives only in config.json (global UI knob); when
    ``AGENTTEAMS_QUIET_ROOMS`` is set, bridge writes it and MatrixChannel reads it.
"""
from __future__ import annotations

import logging

logger = logging.getLogger(__name__)

import json
import os
import shutil
from importlib import resources
from pathlib import Path
from typing import Any

# AgentTeams worker Matrix channel defaults — bridge overlay is authoritative.
MATRIX_FILTER_TOOL_MESSAGES = True
MATRIX_FILTER_THINKING = True

_PROMPT_FILES = ("SOUL.md", "AGENTS.md", "HEARTBEAT.md")


def propagate_prompts(local_dir: Path, copaw_working_dir: Path) -> None:
    """Copy L2 prompt files into CoPaw workspaces/default/ (agent read path)."""
    workspace_dir = copaw_working_dir / "workspaces" / "default"
    workspace_dir.mkdir(parents=True, exist_ok=True)
    for name in _PROMPT_FILES:
        src = Path(local_dir) / name
        if not src.is_file():
            continue
        (workspace_dir / name).write_text(src.read_text(errors="replace"))


def _port_remap(url: str, is_container: bool) -> str:
    """Remap container-internal :8080 to host-exposed gateway port when needed."""
    if not is_container and url and ":8080" in url:
        gateway_port = os.environ.get("AGENTTEAMS_PORT_GATEWAY", "18080")
        return url.replace(":8080", f":{gateway_port}")
    return url


def _is_in_container() -> bool:
    return Path("/.dockerenv").exists() or Path("/run/.containerenv").exists()


def quiet_rooms_enabled() -> bool:
    """True if AGENTTEAMS_QUIET_ROOMS is set truthily (default off).

    When enabled, bridge writes ``show_tool_details: false`` into config.json
    and MatrixChannel.from_config reads it so outbound tool chatter is suppressed.
    """
    raw = os.environ.get("AGENTTEAMS_QUIET_ROOMS")
    if raw is None:
        return False
    return raw.strip().lower() in ("true", "1", "yes", "on")


def _quiet_rooms_enabled() -> bool:
    """Backward-compatible alias for quiet_rooms_enabled()."""
    return quiet_rooms_enabled()


# Process-wide guard: bootstrap_copaw_runtime is idempotent per resolved path.
_BOOTSTRAPPED_WORKING_DIRS: set[str] = set()


def _secret_dir(working_dir: Path) -> Path:
    """Return the secret dir path that copaw uses alongside working_dir."""
    return Path(str(working_dir) + ".secret")


def _patch_copaw_paths(working_dir: Path) -> None:
    """Patch copaw's module-level path constants to point at working_dir.

    copaw.constant captures WORKING_DIR / SECRET_DIR at import time from
    env vars, so setting COPAW_WORKING_DIR after import has no effect.
    We must update the live module objects directly.
    """
    secret_dir = _secret_dir(working_dir)
    secret_dir.mkdir(parents=True, exist_ok=True)

    try:
        import copaw.constant as _const
        _const.WORKING_DIR = working_dir
        _const.SECRET_DIR = secret_dir
        _const.ACTIVE_SKILLS_DIR = working_dir / "active_skills"
        _const.CUSTOMIZED_SKILLS_DIR = working_dir / "customized_skills"
        _const.MEMORY_DIR = working_dir / "memory"
        _const.CUSTOM_CHANNELS_DIR = working_dir / "custom_channels"
        _const.MODELS_DIR = working_dir / "models"
    except ImportError:
        pass

    try:
        import copaw.providers.store as _store
        _store._PROVIDERS_JSON = secret_dir / "providers.json"
        _store._LEGACY_PROVIDERS_JSON_CANDIDATES = (
            Path(__file__).resolve().parent / "providers.json",
            working_dir / "providers.json",
        )
    except ImportError:
        pass

    try:
        import copaw.envs.store as _envs
        _envs._BOOTSTRAP_WORKING_DIR = working_dir
        _envs._BOOTSTRAP_SECRET_DIR = secret_dir
        _envs._ENVS_JSON = secret_dir / "envs.json"
        _envs._LEGACY_ENVS_JSON_CANDIDATES = (working_dir / "envs.json",)
    except (ImportError, AttributeError):
        pass

    # copaw.app.channels.registry binds CUSTOM_CHANNELS_DIR via
    # `from ...constant import CUSTOM_CHANNELS_DIR` at import time, so it keeps
    # a STALE copy of the default path even after we patch copaw.constant above.
    # _discover_custom_channels() / register_custom_channel_routes() read this
    # module global at CALL time, so rebinding it here (before ChannelManager
    # starts) makes them see our working_dir/custom_channels regardless of
    # import order. Without this the custom_channels shim is never
    # discovered and copaw falls back to its builtin (broken) Matrix channel.
    try:
        import copaw.app.channels.registry as _channels_registry
        _channels_registry.CUSTOM_CHANNELS_DIR = working_dir / "custom_channels"
        logger.info(
            "bridge: patched channels registry CUSTOM_CHANNELS_DIR -> %s",
            _channels_registry.CUSTOM_CHANNELS_DIR,
        )
    except ImportError:
        pass


def bridge_config(
    openclaw_cfg: dict[str, Any],
    working_dir: Path,
    *,
    profile: str = "manager",
) -> None:
    """Write CoPaw JSON artifacts from openclaw.json (no runtime side effects)."""
    working_dir.mkdir(parents=True, exist_ok=True)
    in_container = _is_in_container()

    _write_config_json(openclaw_cfg, working_dir, in_container)
    _write_agent_json(openclaw_cfg, working_dir, in_container, profile=profile)
    _write_providers_json(openclaw_cfg, working_dir, in_container)


def bootstrap_copaw_runtime(working_dir: Path, *, force: bool = False) -> None:
    """Patch copaw path constants and secret providers (once per process/path)."""
    working_dir.mkdir(parents=True, exist_ok=True)
    key = str(working_dir.resolve())
    if not force and key in _BOOTSTRAPPED_WORKING_DIRS:
        logger.debug("bootstrap_copaw_runtime skipped (already bootstrapped): %s", key)
        return

    os.environ["COPAW_WORKING_DIR"] = str(working_dir)
    _patch_copaw_paths(working_dir)

    secret_dir = _secret_dir(working_dir)
    secret_dir.mkdir(parents=True, exist_ok=True)
    providers_src = working_dir / "providers.json"
    if providers_src.exists():
        shutil.copy2(providers_src, secret_dir / "providers.json")

    _BOOTSTRAPPED_WORKING_DIRS.add(key)


def reset_bootstrap_state() -> None:
    """Clear bootstrap guard (for unit tests simulating multiple workers)."""
    _BOOTSTRAPPED_WORKING_DIRS.clear()


def bridge_openclaw_to_copaw(
    openclaw_cfg: dict[str, Any],
    working_dir: Path,
    *,
    profile: str = "manager",
) -> None:
    """
    Full bridge: write JSON configs then bootstrap CoPaw runtime paths.

    Re-bridge on openclaw.json changes should call ``bridge_config`` only.
    """
    bridge_config(openclaw_cfg, working_dir, profile=profile)
    bootstrap_copaw_runtime(working_dir)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _resolve_active_model(cfg: dict[str, Any]) -> dict[str, Any] | None:
    """Return the config dict of the active model from openclaw.json, or None.

    Prefers agents.defaults.model.primary ("provider_id/model_id");
    falls back to the first model of the first provider.
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
                return m

    # Fallback: first provider, first model
    for provider_cfg in providers_raw.values():
        models = provider_cfg.get("models", [])
        if models:
            return models[0]

    return None


def _resolve_context_window(cfg: dict[str, Any]) -> int | None:
    """Return the contextWindow of the active (or first) model, or None."""
    m = _resolve_active_model(cfg)
    if not m or "contextWindow" not in m:
        return None
    try:
        return int(m["contextWindow"])
    except (TypeError, ValueError):
        return None


def _resolve_vision_enabled(cfg: dict[str, Any]) -> bool:
    """Return True if the active model declares image input support.

    The openclaw.json model's ``input`` field is a list of supported modalities
    (e.g. ["text", "image"]).  If the field is absent we assume text-only to
    avoid sending images to a model that cannot handle them.
    """
    m = _resolve_active_model(cfg)
    if m is None:
        return False
    input_types = m.get("input", [])
    return "image" in input_types


def _resolve_matrix_user_id(
    matrix_raw: dict[str, Any],
    *,
    profile: str = "worker",
) -> str:
    """Resolve the Matrix MXID that CoPaw tools use for proactive sends."""
    explicit = matrix_raw.get("userId") or matrix_raw.get("user_id")
    if explicit:
        return str(explicit)

    env_user_id = (
        os.environ.get("AGENTTEAMS_MATRIX_USER_ID")
        or os.environ.get("HICLAW_MATRIX_USER_ID")
        or os.environ.get("COPAW_MATRIX_USER_ID")
    )
    if env_user_id:
        return env_user_id

    matrix_domain = (
        os.environ.get("AGENTTEAMS_MATRIX_DOMAIN")
        or os.environ.get("HICLAW_MATRIX_DOMAIN")
    )
    localpart = (
        os.environ.get("AGENTTEAMS_WORKER_NAME")
        or os.environ.get("HICLAW_WORKER_NAME")
        or ("manager" if profile == "manager" else "")
    )
    if matrix_domain and localpart:
        return f"@{localpart}:{matrix_domain}"

    return ""


# ---------------------------------------------------------------------------
# config.json
# ---------------------------------------------------------------------------

def _write_config_json(
    cfg: dict[str, Any],
    working_dir: Path,
    in_container: bool,
) -> None:
    matrix_raw = cfg.get("channels", {}).get("matrix", {})
    homeserver = _port_remap(
        matrix_raw.get("homeserver", ""), in_container
    )
    access_token = matrix_raw.get("accessToken", "")
    user_id = _resolve_matrix_user_id(matrix_raw)

    # DM allowlist
    dm_cfg = matrix_raw.get("dm", {})
    dm_policy = dm_cfg.get("policy", "allowlist")
    dm_allow_from: list[str] = dm_cfg.get("allowFrom", [])

    # Group allowlist
    group_policy = matrix_raw.get("groupPolicy", "allowlist")
    group_allow_from: list[str] = matrix_raw.get("groupAllowFrom", [])

    # Per-room/group config (pass through as-is for MatrixChannel to use)
    groups = matrix_raw.get("groups", {})

    # History limit: openclaw uses camelCase "historyLimit", bridge to snake_case.
    history_limit = matrix_raw.get("historyLimit")
    if history_limit is None:
        history_limit = (
            cfg.get("messages", {}).get("groupChat", {}).get("historyLimit")
        )

    matrix_channel_cfg: dict[str, Any] = {
        "enabled": matrix_raw.get("enabled", True),
        "homeserver": homeserver,
        "access_token": access_token,
        "encryption": matrix_raw.get("encryption", False),
        "dm_policy": dm_policy,
        "allow_from": dm_allow_from,
        "group_policy": group_policy,
        "group_allow_from": group_allow_from,
        "groups": groups,
        "filter_tool_messages": MATRIX_FILTER_TOOL_MESSAGES,
        "filter_thinking": MATRIX_FILTER_THINKING,
        "vision_enabled": _resolve_vision_enabled(cfg),
    }
    if history_limit is not None:
        matrix_channel_cfg["history_limit"] = int(history_limit)
    if user_id:
        matrix_channel_cfg["user_id"] = user_id

    config_path = working_dir / "config.json"
    # Merge with existing config to avoid clobbering other settings
    existing: dict[str, Any] = {}
    if config_path.exists():
        with open(config_path) as f:
            existing = json.load(f)

    existing.setdefault("channels", {})["matrix"] = matrix_channel_cfg
    # Disable console channel (we use Matrix)
    existing["channels"].setdefault("console", {})["enabled"] = False

    # Bridge model context window → agents.running.max_input_length so that
    # CoPaw's memory compaction threshold tracks the actual model capability.
    # We read contextWindow from the first model of the primary (or first)
    # provider to avoid hard-coding a default that mismatches the real model.
    context_window = _resolve_context_window(cfg)
    if context_window is not None:
        existing.setdefault("agents", {}).setdefault("running", {})[
            "max_input_length"
        ] = context_window

    # Phase 5b "quiet rooms" (env-gated, default off): root-level
    # show_tool_details is a separate knob from channels.matrix.* above
    # (copaw/src/matrix/config.py:1164, defaults True, never set anywhere
    # today). Only touch it when the operator opts in — leaving the env
    # unset must produce byte-identical config.json output to before this
    # flag existed.
    if quiet_rooms_enabled():
        existing["show_tool_details"] = False
    else:
        # Restart-time overlay: if quiet rooms was previously enabled and is
        # now off, don't leave a stale show_tool_details behind.
        existing.pop("show_tool_details", None)

    with open(config_path, "w") as f:
        json.dump(existing, f, indent=2, ensure_ascii=False)




# ---------------------------------------------------------------------------
# agent.json — per-agent config (CoPaw 1.0.2+ reads this, not config.json)
# ---------------------------------------------------------------------------

def _minimal_agent_json(profile: str) -> dict[str, Any]:
    """Minimal agent.json when an in-tree template file is unavailable."""
    return {
        "id": "default",
        "name": "Manager" if profile == "manager" else "Default Agent",
        "language": "zh",
        "channels": {
            "console": {"enabled": True},
            "matrix": {
                "enabled": True,
                "filter_tool_messages": MATRIX_FILTER_TOOL_MESSAGES,
                "filter_thinking": MATRIX_FILTER_THINKING,
                "allow_from": [],
                "group_allow_from": [],
                "groups": {},
            },
        },
        "running": {"max_iters": 200},
    }


def _materialize_agent_json(agent_path: Path, profile: str) -> None:
    """Install agent.json from in-tree template."""
    template_name = f"agent.{profile}.json"
    tmpl_path = Path(__file__).resolve().parent / "templates" / template_name
    if tmpl_path.is_file():
        try:
            shutil.copy2(tmpl_path, agent_path)
            return
        except OSError as exc:
            logger.warning(
                "failed to copy agent.json template %s: %s", tmpl_path, exc
            )
            raise RuntimeError(
                f"cannot materialize agent.json from template {template_name}"
            ) from exc

    raise RuntimeError(
        f"agent.json template {template_name} not found at {tmpl_path}"
    )


def _write_agent_json(
    cfg: dict[str, Any],
    working_dir: Path,
    in_container: bool,
    *,
    profile: str = "worker",
) -> None:
    """Create agent.json from template, then overlay Matrix channel config.

    CoPaw 1.0.2+ reads workspace/agent.json for per-agent configuration.
    The template provides defaults; we overlay controller-owned fields
    (Matrix access_token, homeserver, allowlists, context window).
    """
    workspace_dir = working_dir / "workspaces" / "default"
    workspace_dir.mkdir(parents=True, exist_ok=True)
    agent_path = workspace_dir / "agent.json"

    # Install from template if missing
    if not agent_path.exists():
        _materialize_agent_json(agent_path, profile)

    try:
        with open(agent_path) as f:
            agent_cfg = json.load(f)
    except (OSError, json.JSONDecodeError) as exc:
        raise RuntimeError(
            f"cannot read agent.json at {agent_path} for profile {profile!r}: {exc}"
        ) from exc

    if not isinstance(agent_cfg, dict):
        raise RuntimeError(
            f"agent.json at {agent_path} must be a JSON object for profile {profile!r}"
        )

    # Overlay Matrix channel config from openclaw.json
    matrix_raw = cfg.get("channels", {}).get("matrix", {})
    homeserver = _port_remap(matrix_raw.get("homeserver", ""), in_container)
    access_token = matrix_raw.get("accessToken", "")
    user_id = _resolve_matrix_user_id(matrix_raw, profile=profile)

    dm_cfg = matrix_raw.get("dm", {})
    dm_allow_from: list[str] = dm_cfg.get("allowFrom", [])
    group_allow_from: list[str] = matrix_raw.get("groupAllowFrom", [])
    groups = matrix_raw.get("groups", {})

    matrix_ch = agent_cfg.setdefault("channels", {}).setdefault("matrix", {})
    matrix_ch["enabled"] = matrix_raw.get("enabled", True)
    if homeserver:
        matrix_ch["homeserver"] = homeserver
    if access_token:
        matrix_ch["access_token"] = access_token
    if user_id:
        matrix_ch["user_id"] = user_id
    matrix_ch["allow_from"] = dm_allow_from
    matrix_ch["group_allow_from"] = group_allow_from
    matrix_ch["groups"] = groups
    matrix_ch["filter_tool_messages"] = MATRIX_FILTER_TOOL_MESSAGES
    matrix_ch["filter_thinking"] = MATRIX_FILTER_THINKING

    # Disable console channel (we use Matrix)
    agent_cfg.setdefault("channels", {}).setdefault("console", {})["enabled"] = False

    # Bridge context window
    context_window = _resolve_context_window(cfg)
    if context_window is not None:
        agent_cfg.setdefault("running", {})["max_input_length"] = context_window

    # Manager heartbeat interval — AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL flows from
    # the controller (see hiclaw-controller/internal/service/worker_env.go) and
    # overrides the template-seeded heartbeat.every (hardcoded "30m" in
    # templates/agent.manager.json). Only the manager profile reads this env
    # var; the worker template already defaults to 10m on its own.
    if profile == "manager":
        heartbeat_every = os.environ.get(
            "AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL", "10m"
        ).strip() or "10m"
        agent_cfg.setdefault("heartbeat", {})["every"] = heartbeat_every

    # Set workspace_dir
    agent_cfg.setdefault("workspace_dir", str(workspace_dir))

    with open(agent_path, "w") as f:
        json.dump(agent_cfg, f, indent=2, ensure_ascii=False)

# ---------------------------------------------------------------------------
# providers.json
# ---------------------------------------------------------------------------

def _write_providers_json(
    cfg: dict[str, Any],
    working_dir: Path,
    in_container: bool,
) -> None:
    providers_raw = cfg.get("models", {}).get("providers", {})

    custom_providers: dict[str, Any] = {}
    active_provider_id = ""
    active_model = ""

    for provider_id, provider_cfg in providers_raw.items():
        base_url = _port_remap(
            provider_cfg.get("baseUrl", ""), in_container
        )
        api_key = provider_cfg.get("apiKey", "")

        models_raw = provider_cfg.get("models", [])
        models = [
            {"id": m["id"], "name": m.get("name", m["id"])}
            for m in models_raw
            if m.get("id")
        ]

        custom_providers[provider_id] = {
            "id": provider_id,
            "name": provider_id,
            "default_base_url": base_url,
            "api_key_prefix": "",
            "models": models,
            "base_url": base_url,
            "api_key": api_key,
            "chat_model": "OpenAIChatModel",
        }

        # Use first provider + first model as active LLM
        if not active_provider_id and models:
            active_provider_id = provider_id
            active_model = models[0]["id"]

    # Resolve active model from agents.defaults.model.primary
    # Format: "provider_id/model_id"
    primary = (
        cfg.get("agents", {})
        .get("defaults", {})
        .get("model", {})
        .get("primary", "")
    )
    if primary and "/" in primary:
        pid, mid = primary.split("/", 1)
        if pid in custom_providers:
            active_provider_id = pid
            active_model = mid

    providers_data: dict[str, Any] = {
        "providers": {},
        "custom_providers": custom_providers,
        "active_llm": {
            "provider_id": active_provider_id,
            "model": active_model,
        },
    }

    providers_path = working_dir / "providers.json"
    with open(providers_path, "w") as f:
        json.dump(providers_data, f, indent=2, ensure_ascii=False)



# ---------------------------------------------------------------------------
# Runtime-to-standard sync (worker uses this to push edits back to sync root)
# ---------------------------------------------------------------------------

def bridge_runtime_to_standard(standard_dir):
    """Materialize runtime-space edits back into the standard sync root."""
    sync_inner_prompt_files_to_outer(standard_dir)


def sync_inner_prompt_files_to_outer(local_dir):
    """Copy agent-edited prompt files from CoPaw workspace back to sync root."""
    inner_outer_files = ("AGENTS.md", "SOUL.md", "HEARTBEAT.md")
    copaw_ws_dir = Path(local_dir) / ".copaw" / "workspaces" / "default"
    for name in inner_outer_files:
        inner = copaw_ws_dir / name
        outer = Path(local_dir) / name
        if not inner.exists():
            continue
        try:
            inner_mtime = inner.stat().st_mtime
        except OSError:
            continue
        outer_mtime = outer.stat().st_mtime if outer.exists() else 0
        if inner_mtime > outer_mtime:
            inner_content = inner.read_text(errors="replace")
            outer_content = outer.read_text(errors="replace") if outer.exists() else ""
            if inner_content != outer_content:
                outer.write_text(inner_content)
                logger.debug(
                    "Inner->Outer sync: .copaw/workspaces/default/%s -> %s",
                    name,
                    name,
                )

# ---------------------------------------------------------------------------
# CLI entry point — used by manager/scripts/init/start-copaw-manager.sh
# ---------------------------------------------------------------------------

def _main_cli(argv=None):
    import argparse

    parser = argparse.ArgumentParser(
        prog="python -m copaw_worker.bridge",
        description="Bridge Controller config into CoPaw runtime files.",
    )
    parser.add_argument("--openclaw-json", required=True,
                        help="Path to openclaw.json")
    parser.add_argument("--working-dir", required=True,
                        help="CoPaw working dir (e.g. ~/.copaw)")
    parser.add_argument("--profile", default="manager",
                        choices=["worker", "manager"],
                        help="Template profile (default: manager)")
    args = parser.parse_args(argv)

    from pathlib import Path as _Path
    import json as _json

    openclaw_path = _Path(args.openclaw_json)
    if not openclaw_path.exists():
        print(f"ERROR: {openclaw_path} not found", flush=True)
        return 1

    working_dir = _Path(args.working_dir)
    working_dir.mkdir(parents=True, exist_ok=True)

    with open(openclaw_path) as f:
        controller_config = _json.load(f)

    bridge_openclaw_to_copaw(
        controller_config,
        working_dir,
        profile=args.profile,
    )
    return 0


if __name__ == "__main__":
    import sys as _sys
    _sys.exit(_main_cli())
