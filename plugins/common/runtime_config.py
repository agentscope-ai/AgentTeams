"""Shared runtime.yaml / JSON runtime config loader for TeamHarness plugins."""

from __future__ import annotations

import json
import os
import re
from pathlib import Path
from typing import Any


def load_runtime_config(
    *,
    primary_env: str = "TEAMHARNESS_RUNTIME_CONFIG",
    fallback_env: str | None = "AGENTTEAMS_MEMBER_RUNTIME_CONFIG",
) -> dict[str, Any]:
    """Load member runtime config from env path (JSON or YAML)."""
    runtime_config = os.getenv(primary_env, "").strip()
    if not runtime_config and fallback_env:
        runtime_config = os.getenv(fallback_env, "").strip()
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
        data = simple_yaml_sections(text)
    return data if isinstance(data, dict) else {}


def simple_yaml_sections(text: str) -> dict[str, Any]:
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
                data[key] = yaml_scalar(value)
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
                data[section][key] = yaml_scalar(value)
                nested_section = None
            else:
                data[section][key] = {}
                nested_section = key
            continue
        deep = re.match(r"^\s{4}([A-Za-z0-9_]+):\s*(.*)$", line)
        if deep and section and nested_section and isinstance(data.get(section), dict):
            parent = data[section].get(nested_section)
            if isinstance(parent, dict):
                parent[deep.group(1)] = yaml_scalar(deep.group(2).strip())
    return data


def yaml_scalar(value: str) -> Any:
    if value in {"", "null", "Null", "NULL", "~"}:
        return ""
    if value in {"true", "True", "TRUE"}:
        return True
    if value in {"false", "False", "FALSE"}:
        return False
    if (value.startswith("'") and value.endswith("'")) or (
        value.startswith('"') and value.endswith('"')
    ):
        return value[1:-1]
    return value


def section(data: dict[str, Any], name: str) -> dict[str, Any]:
    value = data.get(name)
    return value if isinstance(value, dict) else {}
