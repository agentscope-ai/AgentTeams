#!/usr/bin/env python3
"""Resolve the small environment differences between AgentTeams and DSH."""

from __future__ import annotations

from pathlib import Path
import sys


def _unquote(value: str) -> str:
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        return value[1:-1]
    return value


def desired_model(path: Path) -> tuple[str, str]:
    """Read Controller-generated desired.model.{model,gatewayUrl}."""
    model = ""
    gateway_url = ""
    in_desired = False
    in_model = False
    for raw in path.read_text(encoding="utf-8").splitlines():
        if raw == "desired:":
            in_desired = True
            in_model = False
            continue
        if in_desired and raw and not raw.startswith(" "):
            break
        if not in_desired:
            continue
        if raw == "  model:":
            in_model = True
            continue
        if in_model and raw.startswith("  ") and not raw.startswith("    "):
            in_model = False
        if not in_model or not raw.startswith("    "):
            continue
        key, separator, value = raw.strip().partition(":")
        if not separator:
            continue
        if key == "model":
            model = _unquote(value)
        elif key == "gatewayUrl":
            gateway_url = _unquote(value)
    return model, gateway_url


def deepseek_base_url(explicit_url: str, agentteams_gateway_url: str, runtime_gateway_url: str = "") -> str:
    """Return the DSH chat-completions base without duplicating path segments."""
    explicit = explicit_url.strip().rstrip("/")
    if explicit:
        return explicit
    gateway = (runtime_gateway_url.strip() or agentteams_gateway_url.strip()).rstrip("/")
    if not gateway or gateway.endswith("/v1"):
        return gateway
    return f"{gateway}/v1"


if __name__ == "__main__":
    command = sys.argv[1] if len(sys.argv) > 1 else ""
    if command == "model":
        runtime_path = Path(sys.argv[2])
        fallback = sys.argv[3] if len(sys.argv) > 3 else ""
        model, _ = desired_model(runtime_path)
        print(model or fallback)
    elif command == "base-url":
        explicit = sys.argv[2] if len(sys.argv) > 2 else ""
        gateway = sys.argv[3] if len(sys.argv) > 3 else ""
        runtime_path = Path(sys.argv[4]) if len(sys.argv) > 4 else None
        runtime_gateway = desired_model(runtime_path)[1] if runtime_path is not None else ""
        print(deepseek_base_url(explicit, gateway, runtime_gateway))
    else:
        raise SystemExit("usage: runtime_env.py model RUNTIME_YAML [FALLBACK] | base-url EXPLICIT_URL GATEWAY_URL [RUNTIME_YAML]")
