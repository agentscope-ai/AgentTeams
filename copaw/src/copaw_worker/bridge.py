"""
Bridge: translate openclaw.json (HiClaw Worker config) into CoPaw's
config.json + providers.json, then set COPAW_WORKING_DIR so CoPaw
picks up the right workspace.
"""
from __future__ import annotations

import logging

logger = logging.getLogger(__name__)

import json
import os
import shutil
from pathlib import Path
from typing import Any


def _port_remap(url: str, is_container: bool) -> str:
    """Remap container-internal :8080 to host-exposed gateway port when needed."""
    if not is_container and url and ":8080" in url:
        gateway_port = os.environ.get("HICLAW_PORT_GATEWAY", "18080")
        return url.replace(":8080", f":{gateway_port}")
    return url


def _is_in_container() -> bool:
    return Path("/.dockerenv").exists() or Path("/run/.containerenv").exists()


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
    # import order. Without this the patched matrix_channel.py is never
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


def bridge_controller_to_copaw(
    openclaw_cfg: dict[str, Any],
    working_dir: Path,
    *,
    profile: str = "worker",
    agent: str = "default",
) -> None:
    """
    Read openclaw_cfg (parsed openclaw.json) and write:
      - <working_dir>/config.json          (global config)
      - <working_dir>/workspaces/default/agent.json (per-agent config)
      - <working_dir>/providers.json       (LLM credentials, for reference)
      - <working_dir>.secret/providers.json (where copaw actually reads from)

    Also sets COPAW_WORKING_DIR env var and patches copaw's module-level
    path constants so the running process uses the correct directory.

    """
    if profile not in {"worker", "manager"}:
        raise ValueError(f"unknown bridge profile: {profile}")

    working_dir.mkdir(parents=True, exist_ok=True)
    in_container = _is_in_container()

    _write_config_json(openclaw_cfg, working_dir)
    _write_agent_json(
        openclaw_cfg,
        working_dir,
        in_container,
        profile=profile,
        agent=agent,
    )
    _write_providers_json(openclaw_cfg, working_dir, in_container)

    os.environ["COPAW_WORKING_DIR"] = str(working_dir)

    # Patch module-level constants (import-time values won't reflect env change)
    _patch_copaw_paths(working_dir)

    # Copy providers.json into secret_dir — that's where copaw actually reads it
    secret_dir = _secret_dir(working_dir)
    providers_src = working_dir / "providers.json"
    if providers_src.exists():
        shutil.copy2(providers_src, secret_dir / "providers.json")


def bridge_openclaw_to_copaw(
    openclaw_cfg: dict[str, Any],
    working_dir: Path,
    *,
    profile: str = "manager",
    agent: str = "default",
) -> None:
    """Backward-compatible alias for the controller bridge entry point."""
    bridge_controller_to_copaw(
        openclaw_cfg,
        working_dir,
        profile=profile,
        agent=agent,
    )


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
    if m and "contextWindow" in m:
        return int(m["contextWindow"])
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


def _template_path(name: str) -> Path:
    return Path(__file__).resolve().parent / "templates" / name


def _dedup_union(left: list[Any], right: list[Any]) -> list[Any]:
    merged: list[Any] = []
    for item in [*left, *right]:
        if item not in merged:
            merged.append(item)
    return merged


def _deep_merge_controller_defaults(local: Any, remote: Any) -> Any:
    if isinstance(local, dict) and isinstance(remote, dict):
        result = dict(local)
        for key, value in remote.items():
            if key not in result:
                result[key] = value
            else:
                result[key] = _deep_merge_controller_defaults(result[key], value)
        return result
    return local


def _heartbeat_from_openclaw(cfg: dict[str, Any]) -> dict[str, Any] | None:
    raw = (
        cfg.get("agents", {})
        .get("defaults", {})
        .get("heartbeat")
    )
    if not raw:
        return None
    heartbeat = {"enabled": True}
    if "every" in raw:
        heartbeat["every"] = raw["every"]
    if "target" in raw:
        heartbeat["target"] = raw["target"]
    if "activeHours" in raw:
        heartbeat["active_hours"] = raw["activeHours"]
    return heartbeat


def _embedding_config_from_openclaw(
    cfg: dict[str, Any],
    in_container: bool,
) -> dict[str, Any] | None:
    raw = (
        cfg.get("agents", {})
        .get("defaults", {})
        .get("memorySearch")
    )
    if not raw:
        return None

    remote = raw.get("remote", {})
    return {
        "backend": raw.get("provider", "openai"),
        "model_name": raw.get("model", ""),
        "base_url": _port_remap(remote.get("baseUrl", ""), in_container),
        "api_key": remote.get("apiKey", ""),
        "dimensions": int(raw.get("outputDimensionality", 1024)),
    }


# ---------------------------------------------------------------------------
# config.json
# ---------------------------------------------------------------------------

def _write_config_json(
    cfg: dict[str, Any],
    working_dir: Path,
) -> None:
    config_path = working_dir / "config.json"
    if config_path.exists():
        return
    tmpl_path = _template_path("config.json")
    if tmpl_path.exists():
        shutil.copy2(tmpl_path, config_path)
        return
    config_path.write_text(json.dumps({"security": {}}, indent=2))




# ---------------------------------------------------------------------------
# agent.json — per-agent config (CoPaw 1.0.2+ reads this, not config.json)
# ---------------------------------------------------------------------------

def _write_agent_json(
    cfg: dict[str, Any],
    working_dir: Path,
    in_container: bool,
    *,
    profile: str = "worker",
    agent: str = "default",
) -> None:
    """Create agent.json from template, then overlay Matrix channel config.

    CoPaw 1.0.2+ reads workspace/agent.json for per-agent configuration.
    The template provides defaults; we overlay controller-owned fields
    (Matrix access_token, homeserver, allowlists, context window).
    """
    workspace_dir = working_dir / "workspaces" / agent
    workspace_dir.mkdir(parents=True, exist_ok=True)
    agent_path = workspace_dir / "agent.json"

    # Install from template if missing
    if not agent_path.exists():
        template_name = f"agent.{profile}.json"
        try:
            tmpl_path = _template_path(template_name)
            if tmpl_path.exists():
                shutil.copy2(str(tmpl_path), str(agent_path))
            else:
                # Fallback: create minimal agent.json
                minimal = {
                    "id": "default",
                    "name": "Manager" if profile == "manager" else "Default Agent",
                    "language": "zh",
                    "channels": {
                        "console": {"enabled": True},
                        "matrix": {
                            "enabled": True,
                            "filter_tool_messages": False,
                            "filter_thinking": True,
                            "allow_from": [],
                            "group_allow_from": [],
                            "groups": {},
                        },
                    },
                    "running": {"max_iters": 200},
                }
                with open(agent_path, "w") as f:
                    json.dump(minimal, f, indent=2)
        except Exception:
            logger.exception("failed to seed CoPaw agent template")

    # Load existing agent.json
    try:
        with open(agent_path) as f:
            agent_cfg = json.load(f)
    except Exception:
        agent_cfg = {"id": "default", "channels": {}, "running": {}}

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
    matrix_ch["allow_from"] = _dedup_union(
        matrix_ch.get("allow_from", []),
        dm_allow_from,
    )
    matrix_ch["group_allow_from"] = _dedup_union(
        matrix_ch.get("group_allow_from", []),
        group_allow_from,
    )
    matrix_ch["groups"] = _deep_merge_controller_defaults(
        matrix_ch.get("groups", {}),
        groups,
    )
    matrix_ch["filter_tool_messages"] = matrix_raw.get("filterToolMessages", False)
    matrix_ch["filter_thinking"] = matrix_raw.get("filterThinking", True)
    matrix_ch["vision_enabled"] = _resolve_vision_enabled(cfg)

    # Bridge context window
    context_window = _resolve_context_window(cfg)
    if context_window is not None:
        agent_cfg.setdefault("running", {})["max_input_length"] = context_window

    embedding_config = _embedding_config_from_openclaw(cfg, in_container)
    running_cfg = agent_cfg.setdefault("running", {})
    if embedding_config is not None:
        running_cfg["embedding_config"] = embedding_config
    else:
        running_cfg.pop("embedding_config", None)

    if "heartbeat" not in agent_cfg:
        heartbeat = _heartbeat_from_openclaw(cfg)
        if heartbeat is not None:
            agent_cfg["heartbeat"] = heartbeat

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

def bridge_standard_to_runtime(
    standard_dir,
    runtime_dir,
    openclaw_cfg: dict[str, Any],
    *,
    skill_names: list[str] | None = None,
    profile: str = "worker",
    agent: str = "default",
) -> None:
    """Materialize controller-owned standard workspace files into CoPaw."""
    bridge_controller_to_copaw(
        openclaw_cfg,
        Path(runtime_dir),
        profile=profile,
        agent=agent,
    )
    sync_outer_prompt_files_to_inner(standard_dir, runtime_dir, agent=agent)
    sync_mcporter_config_to_runtime(standard_dir, runtime_dir, agent=agent)
    sync_skills_to_runtime(standard_dir, runtime_dir, skill_names, agent=agent)


def refresh_standard_to_runtime(
    standard_dir,
    runtime_dir,
    openclaw_cfg: dict[str, Any],
    *,
    get_soul=None,
    get_agents_md=None,
    skill_names: list[str] | None = None,
    profile: str = "worker",
    agent: str = "default",
) -> None:
    """Refresh runtime files, seeding missing prompts from legacy readers."""
    standard_path = Path(standard_dir)
    standard_path.mkdir(parents=True, exist_ok=True)

    fallbacks = {
        "SOUL.md": get_soul,
        "AGENTS.md": get_agents_md,
    }
    for name, reader in fallbacks.items():
        path = standard_path / name
        if path.exists() or reader is None:
            continue
        content = reader()
        if content:
            path.write_text(content)

    bridge_standard_to_runtime(
        standard_path,
        runtime_dir,
        openclaw_cfg,
        skill_names=skill_names,
        profile=profile,
        agent=agent,
    )


def sync_outer_prompt_files_to_inner(standard_dir, runtime_dir, *, agent: str = "default"):
    """Copy standard prompt files into the CoPaw workspace."""
    standard_path = Path(standard_dir)
    workspace_dir = Path(runtime_dir) / "workspaces" / agent
    workspace_dir.mkdir(parents=True, exist_ok=True)

    for name in ("SOUL.md", "AGENTS.md"):
        src = standard_path / name
        if src.exists():
            shutil.copy2(src, workspace_dir / name)

    heartbeat_src = standard_path / "HEARTBEAT.md"
    heartbeat_dst = workspace_dir / "HEARTBEAT.md"
    if heartbeat_src.exists() and not heartbeat_dst.exists():
        shutil.copy2(heartbeat_src, heartbeat_dst)


def sync_mcporter_config_to_runtime(standard_dir, runtime_dir, *, agent: str = "default"):
    """Copy mcporter config into the CoPaw workspace config directory."""
    standard_path = Path(standard_dir)
    runtime_path = Path(runtime_dir)
    src = standard_path / "config" / "mcporter.json"
    if not src.exists():
        src = standard_path / "mcporter-servers.json"
    if not src.exists():
        return None

    dst = runtime_path / "workspaces" / agent / "config" / "mcporter.json"
    dst.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src, dst)
    return dst


def _ensure_symlink(link: Path, target: Path) -> None:
    if link.is_symlink():
        if link.resolve() == target.resolve():
            return
        link.unlink()
    elif link.exists():
        if link.is_dir():
            shutil.rmtree(link)
        else:
            link.unlink()
    link.symlink_to(target, target_is_directory=True)


def _load_skill_manifest(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {
            "schema_version": "workspace-skill-manifest.v1",
            "version": 1,
            "skills": {},
        }
    try:
        manifest = json.loads(path.read_text())
    except json.JSONDecodeError:
        manifest = {}
    manifest.setdefault("schema_version", "workspace-skill-manifest.v1")
    manifest.setdefault("version", 1)
    manifest.setdefault("skills", {})
    return manifest


def sync_skills_to_runtime(
    standard_dir,
    runtime_dir,
    skill_names: list[str] | None,
    *,
    agent: str = "default",
) -> list[str]:
    """Expose standard-space skills in the CoPaw workspace and manifest."""
    standard_path = Path(standard_dir)
    runtime_path = Path(runtime_dir)
    standard_skills = standard_path / "skills"
    standard_skills.mkdir(parents=True, exist_ok=True)

    requested = set(skill_names or [])
    installed: list[str] = []
    for child in list(standard_skills.iterdir()):
        if not child.is_dir():
            continue
        if requested and child.name not in requested:
            shutil.rmtree(child)
            continue
        if requested or (child / "SKILL.md").exists():
            installed.append(child.name)
            for script in child.rglob("*.sh"):
                script.chmod(script.stat().st_mode | 0o111)

    workspace_dir = runtime_path / "workspaces" / agent
    workspace_dir.mkdir(parents=True, exist_ok=True)
    _ensure_symlink(workspace_dir / "skills", standard_skills)

    manifest_path = workspace_dir / "skill.json"
    manifest = _load_skill_manifest(manifest_path)
    for name in installed:
        entry = manifest["skills"].setdefault(name, {})
        entry["enabled"] = True
        entry.setdefault("channels", ["all"])
        entry.setdefault("source", "customized")
    manifest_path.write_text(json.dumps(manifest, indent=2, ensure_ascii=False))
    return installed


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
