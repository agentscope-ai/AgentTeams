"""Bridge: openclaw.json → harness-native config files.

Two-phase pattern copied from copaw_worker.bridge:
  1. create: install template if missing
  2. overlay: apply _CONTROLLER_FIELDS on every restart
"""
from __future__ import annotations

import json
import logging
import os
import shutil
from importlib import resources
from pathlib import Path
from typing import Any, Callable

from harness_worker.harness import build_harness

logger = logging.getLogger(__name__)
_MISSING: Any = object()


def _port_remap(url: str, is_container: bool) -> str:
    if not is_container and url and ":8080" in url:
        gateway_port = os.environ.get("HICLAW_PORT_GATEWAY", "18080")
        return url.replace(":8080", f":{gateway_port}")
    return url


def _is_in_container() -> bool:
    return Path("/.dockerenv").exists() or Path("/run/.containerenv").exists()


def _template_text(name: str) -> str:
    return (resources.files("harness_worker") / "templates" / name).read_text(encoding="utf-8")


def _install_from_template(dst: Path, template_name: str) -> bool:
    if dst.exists():
        return False
    dst.parent.mkdir(parents=True, exist_ok=True)
    dst.write_text(_template_text(template_name), encoding="utf-8")
    logger.info("bridge: installed %s from template %s", dst, template_name)
    return True


def bridge_openclaw_to_harness(
    openclaw_cfg: dict[str, Any],
    harness_home: Path,
    harness_type: str,
) -> None:
    harness_home.mkdir(parents=True, exist_ok=True)
    in_container = _is_in_container()

    harness_adapter = build_harness(harness_type)
    harness_adapter.bridge_config(openclaw_cfg, harness_home)

    os.environ["HICLAW_HARNESS_HOME"] = str(harness_home)


def _get_path(container: dict[str, Any], path: tuple[str, ...]) -> Any:
    node: Any = container
    for key in path:
        if not isinstance(node, dict) or key not in node:
            return _MISSING
        node = node[key]
    return node


def _set_path(container: dict[str, Any], path: tuple[str, ...], value: Any) -> None:
    node = container
    for key in path[:-1]:
        nxt = node.get(key)
        if not isinstance(nxt, dict):
            nxt = {}
            node[key] = nxt
        node = nxt
    node[path[-1]] = value


def _deep_merge_local_wins(remote: Any, local: Any) -> Any:
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
    seen: set[str] = set()
    out: list[Any] = []
    for item in (local or []) + (remote or []):
        try:
            key = json.dumps(item, sort_keys=True) if isinstance(item, (dict, list)) else repr(item)
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


def _resolve_active_model(cfg: dict[str, Any]) -> dict[str, Any] | None:
    providers_raw = cfg.get("models", {}).get("providers", {})
    if not providers_raw:
        return None
    primary = cfg.get("agents", {}).get("defaults", {}).get("model", {}).get("primary", "")
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


def _resolve_api_key(cfg: dict[str, Any], provider: str) -> str:
    providers = cfg.get("models", {}).get("providers", {})
    return providers.get(provider, {}).get("apiKey", "")


def _gateway_url(cfg: dict[str, Any], in_container: bool) -> str:
    gateway = cfg.get("gateway", {})
    url = gateway.get("url", "")
    return _port_remap(url, in_container)


def _resolve_matrix_user_id(cfg: dict[str, Any], _in_container: bool = False) -> Any:
    m = cfg.get("channels", {}).get("matrix", {})
    uid = m.get("userId") or m.get("user_id")
    if uid:
        return uid
    domain = os.environ.get("HICLAW_MATRIX_DOMAIN") or os.environ.get("MATRIX_DOMAIN", "")
    if not domain:
        return _MISSING
    local = os.environ.get("HICLAW_WORKER_NAME", "harness-worker")
    return f"@{local}:{domain}"