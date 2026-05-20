"""OpenCode harness adapter."""
from __future__ import annotations

import json
import logging
from pathlib import Path
from typing import Any

from harness_worker.harness.base import BaseHarness, register_harness

logger = logging.getLogger(__name__)


def _resolve_active_model(cfg: dict[str, Any]) -> str:
    primary = cfg.get("agents", {}).get("defaults", {}).get("model", {}).get("primary", "")
    if primary and "/" in primary:
        return primary
    return "gpt-4o"


def _resolve_api_key(cfg: dict[str, Any]) -> str:
    providers = cfg.get("models", {}).get("providers", {})
    for v in providers.values():
        if key := v.get("apiKey", ""):
            return key
    return ""


@register_harness("opencode")
class OpenCodeHarness(BaseHarness):
    name = "opencode"

    def bridge_config(self, openclaw_cfg: dict[str, Any], harness_home: Path) -> None:
        cfg_file = harness_home / ".config" / "opencode" / "opencode.json"
        if cfg_file.exists():
            existing = json.loads(cfg_file.read_text())
        else:
            existing = {}
            cfg_file.parent.mkdir(parents=True, exist_ok=True)

        model = _resolve_active_model(openclaw_cfg)
        api_key = _resolve_api_key(openclaw_cfg)

        existing["model"] = model
        if api_key:
            existing.setdefault("provider", {})["apiKey"] = api_key

        cfg_file.write_text(json.dumps(existing, indent=2))
        logger.info("bridge: opencode config written to %s", cfg_file)

    def build_command(
        self,
        message: str,
        session_id: str | None,
        workspace: Path,
    ) -> list[str]:
        if session_id:
            return ["opencode", "run", message, "--format", "json", "--session", session_id]
        return ["opencode", "run", message, "--format", "json", "--continue"]

    def parse_output(self, stdout_bytes: bytes) -> tuple[str, str | None]:
        try:
            data = json.loads(stdout_bytes.decode("utf-8", errors="replace"))
            text = ""
            if isinstance(data, dict):
                text = data.get("message", data.get("text", ""))
            elif isinstance(data, list) and data:
                text = data[0].get("message", data[0].get("text", ""))
            return text or "(no response)", None
        except json.JSONDecodeError:
            return stdout_bytes.decode("utf-8", errors="replace") or "(parse error)", None

    def env(self, openclaw_cfg: dict[str, Any]) -> dict[str, str]:
        return {}