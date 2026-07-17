"""Merge remote (MinIO/Manager) and local (Worker disk) ``openclaw.json`` configs.

Design principle (local-first)
------------------------------

Local (Worker disk) is the authoritative base. Periodic pulls from MinIO only
overlay Manager-managed slices so the Worker keeps its own customizations.

MERGE_RULES
-----------

Base document
  Start from **local**. Top-level keys not listed below remain local-only
  (``tools``, ``agents``, ``mcp``, etc.).

``models``
  **Remote wins wholesale.** If remote defines ``models``, replace local
  ``models`` entirely. If remote omits ``models``, keep local.

``gateway``
  **Remote wins wholesale.** Same rule as ``models``.

``channels``
  **Deep merge with remote winning leaf conflicts.** Local-only channel keys
  are preserved. Merge runs when either side has a ``channels`` object.

``channels.matrix.accessToken``
  **Local wins.** After the channel deep merge, if local had
  ``channels.matrix.accessToken``, restore it (Worker re-login after restart).

``plugins``
  Merge when either side has ``plugins``:

  ``plugins.entries``
    **Deep merge with local winning leaf conflicts** on shared keys (remote
    provides base/defaults; local customizations survive periodic sync).

  ``plugins.load.paths``
    **Set union**, sorted unique, when either side defines paths.

Output
  JSON text with ``indent=2`` and ``ensure_ascii=False`` (Unicode preserved).

This module is the single implementation shared by:

- ``shared/lib/merge-openclaw-config.sh`` (``python3 -m agentteams_openclaw_merge``)
- ``copaw_worker.sync``
- ``hermes_worker.sync``
"""
from __future__ import annotations

import json
from typing import Any


def deep_merge(base: dict[str, Any], override: dict[str, Any]) -> dict[str, Any]:
    """Deep merge *override* into *base* (override wins leaf conflicts)."""
    result = dict(base)
    for key, val in override.items():
        if key in result and isinstance(result[key], dict) and isinstance(val, dict):
            result[key] = deep_merge(result[key], val)
        else:
            result[key] = val
    return result


def merge_openclaw_config(remote_text: str, local_text: str) -> str:
    """Merge remote and local openclaw.json text; return merged JSON string."""
    remote = json.loads(remote_text)
    local = json.loads(local_text)
    merged: dict[str, Any] = dict(local)

    if remote.get("models") is not None:
        merged["models"] = remote["models"]
    if remote.get("gateway") is not None:
        merged["gateway"] = remote["gateway"]

    r_channels = remote.get("channels") or {}
    l_channels = local.get("channels") or {}
    if r_channels or l_channels:
        merged["channels"] = deep_merge(dict(l_channels), dict(r_channels))
        l_token = local.get("channels", {}).get("matrix", {}).get("accessToken")
        if l_token:
            merged.setdefault("channels", {}).setdefault("matrix", {})[
                "accessToken"
            ] = l_token

    r_plugins = remote.get("plugins")
    l_plugins = local.get("plugins")
    if r_plugins or l_plugins:
        r_plugins = dict(r_plugins or {})
        l_plugins = dict(l_plugins or {})
        out_plugins: dict[str, Any] = dict(l_plugins)
        r_entries = r_plugins.get("entries") or {}
        l_entries = l_plugins.get("entries") or {}
        if r_entries or l_entries:
            out_plugins["entries"] = deep_merge(dict(r_entries), dict(l_entries))
        r_paths = r_plugins.get("load", {}).get("paths")
        l_paths = l_plugins.get("load", {}).get("paths")
        if r_paths is not None or l_paths is not None:
            out_load = dict(l_plugins.get("load") or {})
            out_load["paths"] = sorted(set((r_paths or []) + (l_paths or [])))
            out_plugins["load"] = out_load
        merged["plugins"] = out_plugins

    return json.dumps(merged, indent=2, ensure_ascii=False)
