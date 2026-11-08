#!/usr/bin/env python3
"""Deterministic QwenPaw projection of a bundle contract.yaml.

The bundle contract (apiVersion agentteams/v1alpha1) is the runtime-neutral
source of truth. This script is the reference QwenPaw projection used by the
golden tests; it maps the contract sections onto the QwenPaw runtime
configuration without any runtime, network, or filesystem state.

Output is byte-stable: JSON with sorted keys, 2-space indent, and a trailing
newline. Secret material never appears in the contract; access-token
references are resolved from a local secrets file (mapping of
access_token_ref to value).
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import yaml

CONTRACT_API_VERSION = "agentteams/v1alpha1"
CHANNEL_KEY = "agentteams_matrix"
DEFAULT_HISTORY_LIMIT = 50
DEFAULT_HISTORY_MODE = "mention-buffer"


def _fail(message: str) -> None:
    raise SystemExit(f"ERROR: {message}")


def _load_contract(path: Path) -> dict:
    if not path.is_file():
        _fail(f"missing contract: {path}")
    try:
        with path.open("r", encoding="utf-8") as handle:
            data = yaml.safe_load(handle)
    except yaml.YAMLError as exc:
        _fail(f"invalid yaml {path}: {exc}")
    if not isinstance(data, dict):
        _fail(f"contract must be a mapping: {path}")
    if data.get("apiVersion") != CONTRACT_API_VERSION:
        _fail(
            f"contract apiVersion must be {CONTRACT_API_VERSION}, "
            f"got {data.get('apiVersion')!r}"
        )
    return data


def _load_secrets(path: Path) -> dict:
    if not path.is_file():
        _fail(f"missing secrets file: {path}")
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except ValueError as exc:
        _fail(f"invalid secrets json {path}: {exc}")
    if not isinstance(data, dict):
        _fail(f"secrets must be a mapping: {path}")
    return data


def _as_list(value) -> list[str]:
    if value is None:
        return []
    if isinstance(value, (list, tuple)):
        return [str(item) for item in value]
    return [str(value)]


def _project_matrix(matrix: dict, secrets: dict) -> dict:
    token_ref = str(matrix.get("access_token_ref") or "")
    access_token = str(secrets.get(token_ref) or "")
    if token_ref and not access_token:
        _fail(f"missing secret value for access_token_ref {token_ref!r}")
    require_mention = bool(matrix.get("require_mention", True))
    rooms = _as_list(matrix.get("rooms"))
    return {
        CHANNEL_KEY: {
            "access_token": access_token,
            "allow_from": _as_list(matrix.get("allow_from")),
            "enabled": True,
            "group_allow_from": _as_list(matrix.get("group_allow_from")),
            "groups": {room: {"requireMention": require_mention} for room in rooms},
            "history_limit": int(matrix.get("history_limit", DEFAULT_HISTORY_LIMIT)),
            "history_mode": str(matrix.get("history_mode") or DEFAULT_HISTORY_MODE),
            "homeserver": str(matrix.get("homeserver") or "").rstrip("/"),
            "require_mention": require_mention,
            "rooms": rooms,
            "user_id": str(matrix.get("user_id") or ""),
        }
    }


def project(contract: dict, secrets: dict) -> dict:
    metadata = contract.get("metadata") or {}
    matrix = contract.get("matrix") or {}
    identity = contract.get("identity") or {}
    skills = contract.get("skills") or {}
    mcp = contract.get("mcp") or {}
    storage = contract.get("storage") or {}
    approvals = contract.get("approvals") or {}
    return {
        "approvals": {
            "level": str(approvals.get("level") or "none"),
        },
        "bundle": str(metadata.get("name") or ""),
        "channels": _project_matrix(matrix, secrets),
        "contract": str(contract.get("apiVersion") or ""),
        "identity": {
            "agents_path": str(identity.get("agents_path") or ""),
            "display_name": str(identity.get("display_name") or ""),
            "name": str(identity.get("name") or ""),
            "soul_path": str(identity.get("soul_path") or ""),
        },
        "mcp": {
            "servers": [
                {
                    "args": _as_list(server.get("args")),
                    "command": str(server.get("command") or ""),
                    "id": str(server.get("id") or ""),
                    "static": bool(server.get("static", False)),
                    "transport": str(server.get("transport") or "stdio"),
                }
                for server in (mcp.get("servers") or [])
            ],
            "static_representable": bool(mcp.get("static_representable", True)),
        },
        "skills": {
            "roots": _as_list(skills.get("roots")),
        },
        "storage": {
            "prefix": str(storage.get("prefix") or ""),
            "sync_mode": str(storage.get("sync_mode") or "on-start"),
        },
        "version": str(metadata.get("version") or ""),
    }


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        description="Project a bundle contract.yaml into a QwenPaw configuration."
    )
    parser.add_argument("--contract", required=True, type=Path)
    parser.add_argument("--secrets", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args(argv)

    contract = _load_contract(args.contract)
    secrets = _load_secrets(args.secrets)
    result = project(contract, secrets)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(
        json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
