"""
Bridge between HiClaw's standard space and CoPaw's runtime space.

The standard space is the OpenClaw-style sync root restored from MinIO, for
example ``/root/.hiclaw-worker/<name>/``. It is the durable, runtime-agnostic
layout owned by HiClaw:

  - ``openclaw.json``
  - ``SOUL.md`` / ``AGENTS.md`` / ``HEARTBEAT.md``
  - ``skills/``, ``config/``, ``credentials/``, and other synced files

The runtime space is CoPaw's native working directory under the standard space,
for example ``/root/.hiclaw-worker/<name>/.copaw/``. It is the layout that the
CoPaw process actually reads and mutates while running:

  - ``config.json``
  - ``providers.json`` and ``.copaw.secret/providers.json``
  - ``workspaces/default/agent.json``
  - ``workspaces/default/SOUL.md`` / ``AGENTS.md`` / ``HEARTBEAT.md``

Standard space -> runtime space:

  - Convert ``openclaw.json`` into CoPaw-native structured config:
    ``config.json``, ``providers.json``, and ``workspaces/default/agent.json``.
  - Patch CoPaw path constants so the running process reads this runtime space.
  - Copy ``providers.json`` into the adjacent secret dir that CoPaw reads.
  - Copy prompt files into ``workspaces/default/``.
  - Copy ``config/mcporter.json`` into
    ``workspaces/default/config/mcporter.json``; the legacy
    ``mcporter-servers.json`` source is still accepted.
  - Expose Manager-pushed ``skills/<name>/`` directories by making
    ``workspaces/default/skills`` a symlink to the standard-space ``skills/``
    directory. The standard space remains canonical.

Runtime space -> standard space:

  - Copy agent-edited prompt files from ``workspaces/default/`` back to the
    standard space when the runtime copy is newer.
  - Leave MinIO upload to ``sync.push_local``; this bridge only materializes the
    standard-space files that the normal push loop will persist.
"""

from __future__ import annotations

import json
import logging
import os
import shutil
from importlib import resources
from pathlib import Path
from typing import Any, Callable

logger = logging.getLogger(__name__)

# Sentinel returned by derivers to mean "skip this policy this run" (the
# corresponding key is left as-is in agent.json).
_MISSING: Any = object()


def bridge_standard_to_runtime(
    standard_dir: Path,
    runtime_dir: Path,
    controller_config: dict[str, Any],
    *,
    skill_names: list[str] | None = None,
    profile: str = "worker",
    agent: str = "default",
) -> None:
    """Materialize standard-space files into CoPaw's runtime space."""
    sync_outer_prompt_files_to_inner(standard_dir, runtime_dir)
    bridge_openclaw_to_copaw(
        controller_config,
        runtime_dir,
        profile=profile,
        agent=agent,
    )
    _apply_credential_guard(standard_dir, runtime_dir)
    sync_mcporter_config_to_runtime(standard_dir, runtime_dir)
    if skill_names is not None:
        sync_skills_to_runtime(standard_dir, runtime_dir, skill_names)


def refresh_standard_to_runtime(
    standard_dir: Path,
    runtime_dir: Path,
    controller_config: dict[str, Any],
    *,
    get_soul: Callable[[], str | None],
    get_agents_md: Callable[[], str | None],
    skill_names: list[str] | None = None,
    profile: str = "worker",
    agent: str = "default",
) -> None:
    """Refresh runtime space during re-bridge, including legacy prompt fallback."""
    sync_rebridged_prompt_files_to_inner(
        standard_dir,
        runtime_dir,
        get_soul=get_soul,
        get_agents_md=get_agents_md,
    )
    bridge_openclaw_to_copaw(
        controller_config,
        runtime_dir,
        profile=profile,
        agent=agent,
    )
    _apply_credential_guard(standard_dir, runtime_dir)
    sync_mcporter_config_to_runtime(standard_dir, runtime_dir)
    if skill_names is not None:
        sync_skills_to_runtime(standard_dir, runtime_dir, skill_names)


def bridge_openclaw_to_copaw(
    openclaw_cfg: dict[str, Any],
    working_dir: Path,
    *,
    profile: str = "worker",
    agent: str = "default",
) -> None:
    """Bridge OpenClaw-style config into CoPaw's runtime files."""
    if profile not in ("worker", "manager"):
        raise ValueError(
            f"unknown bridge profile: {profile!r} (use 'worker' or 'manager')"
        )

    working_dir.mkdir(parents=True, exist_ok=True)
    in_container = _is_in_container()

    _write_config_json(working_dir)
    _write_providers_json(openclaw_cfg, working_dir, in_container)
    _write_agent_json(
        openclaw_cfg,
        working_dir,
        in_container,
        profile=profile,
        agent=agent,
    )

    os.environ["COPAW_WORKING_DIR"] = str(working_dir)
    _patch_copaw_paths(working_dir)

    secret_dir = _secret_dir(working_dir)
    providers_src = working_dir / "providers.json"
    if providers_src.exists():
        shutil.copy2(providers_src, secret_dir / "providers.json")


def bridge_controller_to_copaw(
    controller_config: dict[str, Any],
    working_dir: Path,
    *,
    profile: str = "worker",
    agent: str = "default",
) -> None:
    """Compatibility alias for bridge_openclaw_to_copaw."""
    bridge_openclaw_to_copaw(
        controller_config,
        working_dir,
        profile=profile,
        agent=agent,
    )


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
    """Patch copaw's module-level path constants to point at working_dir."""
    secret_dir = _secret_dir(working_dir)
    secret_dir.mkdir(parents=True, exist_ok=True)

    try:
        import copaw.constant as _const
        _const.WORKING_DIR = working_dir
        _const.SECRET_DIR = secret_dir
        _const.ACTIVE_SKILLS_DIR = (
            working_dir / "workspaces" / "default" / "skills"
        )
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


def _template_text(name: str) -> str:
    """Read a template by basename from the in-tree templates/ directory."""
    return (resources.files("copaw_worker") / "templates" / name).read_text(
        encoding="utf-8"
    )


def _install_from_template(dst: Path, template_name: str) -> bool:
    """Copy template -> dst only if dst is missing. Returns True when installed."""
    if dst.exists():
        return False
    dst.parent.mkdir(parents=True, exist_ok=True)
    dst.write_text(_template_text(template_name), encoding="utf-8")
    logger.info("bridge: installed %s from template %s", dst, template_name)
    return True


def sync_mcporter_config_to_runtime(standard_dir: Path, runtime_dir: Path) -> Path | None:
    """Copy mcporter config from standard space into CoPaw's default workspace."""
    src_candidates = (
        standard_dir / "config" / "mcporter.json",
        standard_dir / "mcporter-servers.json",
    )
    src = next((candidate for candidate in src_candidates if candidate.exists()), None)
    if src is None:
        logger.info("No mcporter config found to copy from %s", standard_dir)
        return None

    dst = runtime_dir / "workspaces" / "default" / "config" / "mcporter.json"
    dst.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src, dst)
    logger.info("mcporter config copied to %s", dst)
    return dst


def sync_skills_to_runtime(
    standard_dir: Path,
    runtime_dir: Path,
    skill_names: list[str],
) -> list[str]:
    """Expose Manager-pushed skills in CoPaw runtime space via symlink."""
    standard_skills_dir = standard_dir / "skills"
    standard_skills_dir.mkdir(parents=True, exist_ok=True)

    # MinIO does not preserve Unix permission bits. Restore executable scripts
    # in the standard space because runtime skills are a direct symlink to it.
    for sh in standard_skills_dir.rglob("*.sh"):
        sh.chmod(sh.stat().st_mode | 0o111)

    skill_name_set = set(skill_names)
    for child in list(standard_skills_dir.iterdir()):
        if child.is_dir() and child.name not in skill_name_set:
            shutil.rmtree(child)
            logger.info("Removed stale standard skill no longer in MinIO: %s", child.name)

    workspace_skills_dir = runtime_dir / "workspaces" / "default" / "skills"
    workspace_skills_dir.parent.mkdir(parents=True, exist_ok=True)
    dedup_customized_skills(runtime_dir)

    expected_target = standard_skills_dir.resolve()
    if workspace_skills_dir.is_symlink():
        if workspace_skills_dir.resolve() != expected_target:
            workspace_skills_dir.unlink()
    elif workspace_skills_dir.exists():
        if workspace_skills_dir.is_dir():
            shutil.rmtree(workspace_skills_dir)
        else:
            workspace_skills_dir.unlink()

    if not workspace_skills_dir.exists():
        target = os.path.relpath(standard_skills_dir, workspace_skills_dir.parent)
        workspace_skills_dir.symlink_to(target, target_is_directory=True)
        logger.info("Linked runtime skills dir %s -> %s", workspace_skills_dir, target)

    installed = [
        skill_name
        for skill_name in skill_names
        if (standard_skills_dir / skill_name).exists()
    ]
    for skill_name in installed:
        logger.info("Exposed MinIO skill: %s", skill_name)
    enable_workspace_skills_by_default(runtime_dir, installed)
    return installed


def enable_workspace_skills_by_default(
    runtime_dir: Path,
    skill_names: list[str],
) -> None:
    """Seed CoPaw's workspace manifest so exposed HiClaw skills are active."""
    if not skill_names:
        return

    workspace_dir = runtime_dir / "workspaces" / "default"
    workspace_skills_dir = workspace_dir / "skills"
    manifest_path = workspace_dir / "skill.json"

    manifest: dict[str, Any] = {
        "schema_version": "workspace-skill-manifest.v1",
        "version": 1,
        "skills": {},
    }
    if manifest_path.exists():
        try:
            loaded = json.loads(manifest_path.read_text(encoding="utf-8"))
            if isinstance(loaded, dict):
                manifest.update(loaded)
        except json.JSONDecodeError:
            logger.warning(
                "Invalid CoPaw skill manifest, recreating: %s",
                manifest_path,
            )

    if not isinstance(manifest.get("skills"), dict):
        manifest["skills"] = {}
    skills = manifest["skills"]
    changed = False
    for skill_name in sorted(set(skill_names)):
        if not (workspace_skills_dir / skill_name / "SKILL.md").exists():
            continue
        existing = skills.get(skill_name)
        if isinstance(existing, dict):
            if existing.get("enabled") is not True:
                existing["enabled"] = True
                changed = True
            if not existing.get("channels"):
                existing["channels"] = ["all"]
                changed = True
            continue
        skills[skill_name] = {
            "enabled": True,
            "channels": ["all"],
            "source": "customized",
        }
        changed = True

    if changed or not manifest_path.exists():
        workspace_dir.mkdir(parents=True, exist_ok=True)
        manifest_path.write_text(
            json.dumps(manifest, indent=2, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )


def dedup_customized_skills(runtime_dir: Path) -> None:
    """Remove customized skills that shadow CoPaw builtins."""
    customized_dir = runtime_dir / "customized_skills"
    if not customized_dir.is_dir():
        return

    try:
        import copaw.agents.skills as _skills_pkg
        builtin_skills_root = Path(_skills_pkg.__file__).resolve().parent
    except (ImportError, AttributeError):
        return

    builtin_names: set[str] = set()
    if builtin_skills_root.is_dir():
        for child in builtin_skills_root.iterdir():
            if child.is_dir() and not child.name.startswith("_"):
                builtin_names.add(child.name)

    if not builtin_names:
        return

    for child in list(customized_dir.iterdir()):
        if child.is_dir() and child.name in builtin_names:
            shutil.rmtree(child)
            logger.info(
                "Removed stale customized skill '%s' (now a builtin)",
                child.name,
            )


def sync_outer_prompt_files_to_inner(local_dir: Path, copaw_working_dir: Path) -> None:
    """Copy OpenClaw-style prompt files into CoPaw's default workspace."""
    workspace_dir = copaw_working_dir / "workspaces" / "default"
    workspace_dir.mkdir(parents=True, exist_ok=True)

    for name in ("SOUL.md", "AGENTS.md"):
        src = local_dir / name
        if src.exists():
            (workspace_dir / name).write_text(src.read_text())

    heartbeat_dst = workspace_dir / "HEARTBEAT.md"
    if not heartbeat_dst.exists():
        heartbeat_src = local_dir / "HEARTBEAT.md"
        if heartbeat_src.exists():
            heartbeat_dst.write_text(heartbeat_src.read_text())


def sync_rebridged_prompt_files_to_inner(
    local_dir: Path,
    copaw_working_dir: Path,
    *,
    get_soul: Callable[[], str | None],
    get_agents_md: Callable[[], str | None],
) -> None:
    """Refresh CoPaw prompt files during re-bridge while preserving legacy fallback."""
    soul_path = local_dir / "SOUL.md"
    agents_path = local_dir / "AGENTS.md"
    soul = soul_path.read_text() if soul_path.exists() else get_soul()
    agents = agents_path.read_text() if agents_path.exists() else get_agents_md()

    workspace_dir = copaw_working_dir / "workspaces" / "default"
    if soul:
        workspace_dir.mkdir(parents=True, exist_ok=True)
        (workspace_dir / "SOUL.md").write_text(soul)
    if agents:
        workspace_dir.mkdir(parents=True, exist_ok=True)
        (workspace_dir / "AGENTS.md").write_text(agents)


def _matrix_raw(cfg: dict[str, Any]) -> dict[str, Any]:
    return cfg.get("channels", {}).get("matrix", {})


def _matrix_bool(
    cfg: dict[str, Any],
    camel_key: str,
    snake_key: str,
    default: bool,
) -> bool:
    matrix = _matrix_raw(cfg)
    if camel_key in matrix:
        return bool(matrix.get(camel_key))
    if snake_key in matrix:
        return bool(matrix.get(snake_key))
    return default


def _resolve_active_model(cfg: dict[str, Any]) -> dict[str, Any] | None:
    """Return the config dict of the active model from openclaw.json, or None."""
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

    for provider_cfg in providers_raw.values():
        models = provider_cfg.get("models", [])
        if models:
            return models[0]

    return None


def _resolve_context_window(cfg: dict[str, Any]) -> int | None:
    m = _resolve_active_model(cfg)
    if m and "contextWindow" in m:
        return int(m["contextWindow"])
    return None


def _resolve_vision_enabled(cfg: dict[str, Any]) -> bool:
    """True if the active model declares image input support."""
    m = _resolve_active_model(cfg)
    if m is None:
        return False
    return "image" in m.get("input", [])


def _resolve_embedding_config(
    cfg: dict[str, Any],
    in_container: bool,
) -> dict[str, Any] | None:
    """Extract embedding config from openclaw's ``agents.defaults.memorySearch``."""
    memory_search = (
        cfg.get("agents", {})
        .get("defaults", {})
        .get("memorySearch", {})
    )
    if not memory_search:
        return None

    remote = memory_search.get("remote", {})
    base_url = _port_remap(remote.get("baseUrl", ""), in_container)
    api_key = remote.get("apiKey", "")
    model = memory_search.get("model", "")

    if not base_url or not model:
        return None

    if not api_key:
        logger.warning(
            "memorySearch.remote.apiKey is empty; embedding requests will likely fail",
        )

    dimensions = (
        memory_search.get("outputDimensionality")
        or int(os.environ.get("HICLAW_EMBEDDING_DIMENSIONS", "0"))
        or 1024
    )

    return {
        "backend": "openai",
        "api_key": api_key,
        "base_url": base_url,
        "model_name": model,
        "dimensions": dimensions,
        "enable_cache": True,
        "use_dimensions": False,
    }


def _resolve_history_limit(cfg: dict[str, Any]) -> int | None:
    matrix_raw = _matrix_raw(cfg)
    hl = matrix_raw.get("historyLimit")
    if hl is None:
        hl = cfg.get("messages", {}).get("groupChat", {}).get("historyLimit")
    return int(hl) if hl is not None else None


def _derive_matrix_user_id(cfg: dict[str, Any], _in_container: bool = False) -> Any:
    """Derive CoPaw Matrix user_id from OpenClaw config or env."""
    m = _matrix_raw(cfg)
    uid = m.get("userId") or m.get("user_id")
    if uid:
        return uid
    domain = os.environ.get("HICLAW_MATRIX_DOMAIN") or os.environ.get("MATRIX_DOMAIN", "")
    if not domain:
        return _MISSING
    local = os.environ.get("HICLAW_WORKER_NAME") or os.environ.get("WORKER_NAME", "manager")
    return f"@{local}:{domain}"


def _derive_heartbeat(cfg: dict[str, Any], _in_container: bool = False) -> Any:
    """Map openclaw agents.defaults.heartbeat -> copaw heartbeat block."""
    hb = cfg.get("agents", {}).get("defaults", {}).get("heartbeat")
    if not isinstance(hb, dict) or not hb:
        return _MISSING
    out: dict[str, Any] = {"enabled": True}
    if "every" in hb:
        out["every"] = hb["every"]
    if "target" in hb:
        out["target"] = hb["target"]
    if "activeHours" in hb:
        out["active_hours"] = hb["activeHours"]
    return out


def _get_path(container: dict[str, Any], path: tuple[str, ...]) -> Any:
    """Return value at ``path`` inside nested dicts, or ``_MISSING``."""
    node: Any = container
    for key in path:
        if not isinstance(node, dict) or key not in node:
            return _MISSING
        node = node[key]
    return node


def _set_path(container: dict[str, Any], path: tuple[str, ...], value: Any) -> None:
    """Assign ``value`` at ``path``, creating intermediate dicts as needed."""
    node = container
    for key in path[:-1]:
        nxt = node.get(key)
        if not isinstance(nxt, dict):
            nxt = {}
            node[key] = nxt
        node = nxt
    node[path[-1]] = value


def _deep_merge_local_wins(remote: Any, local: Any) -> Any:
    """Deep-merge two JSON trees where local leaves win over remote."""
    if isinstance(remote, dict) and isinstance(local, dict):
        out: dict[str, Any] = {}
        for k in remote.keys() | local.keys():
            if k in remote and k in local:
                out[k] = _deep_merge_local_wins(remote[k], local[k])
            elif k in remote:
                out[k] = remote[k]
            else:
                out[k] = local[k]
        return out
    return local


def _union_list(remote: list[Any] | None, local: list[Any] | None) -> list[Any]:
    """Concat local then remote, dedup preserving order. Local entries win order."""
    seen: set[str] = set()
    out: list[Any] = []
    for item in (local or []) + (remote or []):
        try:
            key = (
                json.dumps(item, sort_keys=True)
                if isinstance(item, (dict, list))
                else repr(item)
            )
        except TypeError:
            key = repr(item)
        if key not in seen:
            seen.add(key)
            out.append(item)
    return out


def _apply_policy(
    existing: dict[str, Any],
    path: tuple[str, ...],
    policy: str,
    remote_value: Any,
) -> None:
    """Apply one merge policy for one path. ``remote_value == _MISSING`` skips."""
    if remote_value is _MISSING:
        return

    if policy == "remote-wins":
        _set_path(existing, path, remote_value)
        return

    if policy == "union":
        local_value = _get_path(existing, path)
        local_list = local_value if isinstance(local_value, list) else []
        remote_list = remote_value if isinstance(remote_value, list) else []
        _set_path(existing, path, _union_list(remote_list, local_list))
        return

    if policy == "deep-merge":
        local_value = _get_path(existing, path)
        if local_value is _MISSING:
            _set_path(existing, path, remote_value)
        else:
            _set_path(existing, path, _deep_merge_local_wins(remote_value, local_value))
        return

    if policy == "seed":
        local_value = _get_path(existing, path)
        if local_value is _MISSING:
            _set_path(existing, path, remote_value)
        return

    raise ValueError(f"unknown merge policy: {policy}")


_PolicyDeriver = Callable[[dict[str, Any], bool], Any]


_CONTROLLER_FIELDS: list[tuple[tuple[str, ...], str, _PolicyDeriver]] = [
    (("channels", "matrix", "enabled"),
     "remote-wins", lambda c, _: _matrix_raw(c).get("enabled", True)),
    (("channels", "matrix", "homeserver"),
     "remote-wins", lambda c, ic: _port_remap(_matrix_raw(c).get("homeserver", ""), ic)),
    (("channels", "matrix", "access_token"),
     "remote-wins", lambda c, _: _matrix_raw(c).get("accessToken", "")),
    (("channels", "matrix", "user_id"),
     "remote-wins", _derive_matrix_user_id),
    (("channels", "matrix", "encryption"),
     "remote-wins", lambda c, _: _matrix_raw(c).get("encryption", False)),
    (("channels", "matrix", "dm_policy"),
     "remote-wins", lambda c, _: _matrix_raw(c).get("dm", {}).get("policy", "allowlist")),
    (("channels", "matrix", "group_policy"),
     "remote-wins", lambda c, _: _matrix_raw(c).get("groupPolicy", "allowlist")),
    (("channels", "matrix", "filter_tool_messages"),
     "remote-wins", lambda c, _: _matrix_bool(c, "filterToolMessages", "filter_tool_messages", False)),
    (("channels", "matrix", "filter_thinking"),
     "remote-wins", lambda c, _: _matrix_bool(c, "filterThinking", "filter_thinking", True)),
    (("channels", "matrix", "vision_enabled"),
     "remote-wins", lambda c, _: _resolve_vision_enabled(c)),
    (("channels", "matrix", "history_limit"),
     "remote-wins",
     lambda c, _: _resolve_history_limit(c) if _resolve_history_limit(c) is not None else _MISSING),
    (("channels", "matrix", "allow_from"),
     "union", lambda c, _: _matrix_raw(c).get("dm", {}).get("allowFrom", []) or []),
    (("channels", "matrix", "group_allow_from"),
     "union", lambda c, _: _matrix_raw(c).get("groupAllowFrom", []) or []),
    (("channels", "matrix", "groups"),
     "deep-merge", lambda c, _: _matrix_raw(c).get("groups", {}) or {}),
    (("running", "max_input_length"),
     "remote-wins",
     lambda c, _: _resolve_context_window(c) if _resolve_context_window(c) is not None else _MISSING),
    (("running", "embedding_config"),
     "remote-wins",
     lambda c, ic: _resolve_embedding_config(c, ic) if _resolve_embedding_config(c, ic) is not None else _MISSING),
    (("heartbeat",), "seed", _derive_heartbeat),
]


def _apply_credential_guard(standard_dir: Path, runtime_dir: Path) -> None:
    """Inject credagent.json paths into CoPaw's file guard config."""
    from copaw_worker.hooks.credential_guard import apply_credential_guard

    count = apply_credential_guard(standard_dir, runtime_dir)
    if count > 0:
        logger.info("bridge: credential guard applied %d protected paths", count)


def _write_config_json(working_dir: Path) -> None:
    """Install config.json from template if missing. Never overwrite."""
    _install_from_template(working_dir / "config.json", "config.json")


def _write_agent_json(
    controller_config: dict[str, Any],
    working_dir: Path,
    in_container: bool,
    *,
    profile: str = "worker",
    agent: str = "default",
) -> None:
    """Create agent.json from template if absent; then overlay controller fields."""
    agent_path = working_dir / "workspaces" / agent / "agent.json"
    _install_from_template(agent_path, f"agent.{profile}.json")

    try:
        with open(agent_path) as f:
            existing = json.load(f)
        if not isinstance(existing, dict):
            raise ValueError("agent.json root is not a dict")
    except Exception as exc:
        logger.warning(
            "agent.json at %s is unreadable (%s); re-seeding from template",
            agent_path,
            exc,
        )
        agent_path.unlink(missing_ok=True)
        _install_from_template(agent_path, f"agent.{profile}.json")
        with open(agent_path) as f:
            existing = json.load(f)

    for path, policy, deriver in _CONTROLLER_FIELDS:
        remote_value = deriver(controller_config, in_container)
        _apply_policy(existing, path, policy, remote_value)

    # workspace_dir depends on local filesystem layout; seed once, never rewrite.
    existing.setdefault("workspace_dir", str(agent_path.parent))

    with open(agent_path, "w") as f:
        json.dump(existing, f, indent=2, ensure_ascii=False)


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
        base_url = _port_remap(provider_cfg.get("baseUrl", ""), in_container)
        api_key = provider_cfg.get("apiKey", "")

        models_raw = provider_cfg.get("models", [])
        models = [
            {
                "id": m["id"],
                "name": m.get("name", m["id"]),
                "supports_image": "image" in m.get("input", []),
                "supports_video": "video" in m.get("input", []),
                "supports_multimodal": "image" in m.get("input", []) or "video" in m.get("input", []),
            }
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

        if not active_provider_id and models:
            active_provider_id = provider_id
            active_model = models[0]["id"]

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

def bridge_runtime_to_standard(standard_dir: Path) -> None:
    """Materialize runtime-space edits back into the standard sync root."""
    sync_inner_prompt_files_to_outer(standard_dir)


def sync_inner_prompt_files_to_outer(local_dir: Path) -> None:
    """Copy agent-edited prompt files from CoPaw workspace back to sync root."""
    inner_outer_files = ("AGENTS.md", "SOUL.md", "HEARTBEAT.md")
    copaw_ws_dir = local_dir / ".copaw" / "workspaces" / "default"
    for name in inner_outer_files:
        inner = copaw_ws_dir / name
        outer = local_dir / name
        if not inner.exists():
            continue
        try:
            inner_mtime = inner.stat().st_mtime
        except OSError:
            continue
        # Only copy if inner is newer than outer (or outer doesn't exist)
        outer_mtime = outer.stat().st_mtime if outer.exists() else 0
        if inner_mtime > outer_mtime:
            inner_content = inner.read_text(errors="replace")
            outer_content = outer.read_text(errors="replace") if outer.exists() else ""
            if inner_content != outer_content:
                outer.write_text(inner_content)
                logger.debug(
                    "Inner→Outer sync: .copaw/workspaces/default/%s → %s",
                    name,
                    name,
                )



def _main_cli(argv: list[str] | None = None) -> int:
    import argparse

    parser = argparse.ArgumentParser(
        prog="python -m copaw_worker.bridge",
        description=(
            "Bridge Controller config (openclaw.json today) into CoPaw's "
            "config.json / agent.json / providers.json."
        ),
    )
    parser.add_argument("--openclaw-json", required=True,
                        help="Path to the controller config file (openclaw.json)")
    parser.add_argument("--working-dir", required=True,
                        help="CoPaw working dir (e.g. ~/.copaw)")
    parser.add_argument("--profile", default="worker", choices=["worker", "manager"],
                        help="Template profile to use on first boot")
    parser.add_argument("--agent", default="default",
                        help="CoPaw workspace key (maps to workspaces/<agent>/). "
                             "Default: 'default'. Exposed for multi-agent setups.")
    args = parser.parse_args(argv)

    openclaw_path = Path(args.openclaw_json)
    if not openclaw_path.exists():
        print(f"ERROR: {openclaw_path} not found", flush=True)
        return 1

    working_dir = Path(args.working_dir)
    working_dir.mkdir(parents=True, exist_ok=True)

    with open(openclaw_path) as f:
        controller_config = json.load(f)

    bridge_openclaw_to_copaw(
        controller_config,
        working_dir,
        profile=args.profile,
        agent=args.agent,
    )
    print(
        f"Bridged {openclaw_path} -> {working_dir} "
        f"(profile={args.profile}, agent={args.agent})",
        flush=True,
    )
    return 0


if __name__ == "__main__":
    import sys
    sys.exit(_main_cli())
