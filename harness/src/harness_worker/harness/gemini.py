"""Gemini CLI harness adapter."""
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
        _, mid = primary.split("/", 1)
        return mid
    return "gemini-2.5-pro"


def _resolve_api_key(cfg: dict[str, Any]) -> str:
    providers = cfg.get("models", {}).get("providers", {})
    for v in providers.values():
        if key := v.get("apiKey", ""):
            return key
    return ""


@register_harness("gemini")
class GeminiHarness(BaseHarness):
    name = "gemini"

    def bridge_config(self, openclaw_cfg: dict[str, Any], harness_home: Path) -> None:
        cfg_file = harness_home / ".gemini" / "settings.json"
        if cfg_file.exists():
            existing = json.loads(cfg_file.read_text())
        else:
            existing = {}
            cfg_file.parent.mkdir(parents=True, exist_ok=True)

        model = _resolve_active_model(openclaw_cfg)
        api_key = _resolve_api_key(openclaw_cfg)

        existing.setdefault("model", {})["name"] = model
        existing.setdefault("security", {}).setdefault("auth", {})["selectedType"] = "api-key"
        if api_key:
            existing["security"]["auth"]["apiKey"] = api_key
        existing.setdefault("general", {})["defaultApprovalMode"] = "auto_edit"

        cfg_file.write_text(json.dumps(existing, indent=2))
        logger.info("bridge: gemini settings written to %s", cfg_file)

    def build_command(
        self,
        message: str,
        session_id: str | None,
        workspace: Path,
    ) -> list[str]:
        argv = ["gemini", "--prompt", message, "--yolo", "--output-format", "json"]
        return argv

    def parse_output(self, stdout_bytes: bytes) -> tuple[str, str | None]:
        try:
            data = json.loads(stdout_bytes.decode("utf-8", errors="replace"))
            text = ""
            if isinstance(data, dict):
                candidates = data.get("candidates", [])
                if candidates:
                    content = candidates[0].get("content", {})
                    parts = content.get("parts", [])
                    text = "\n".join(p.get("text", "") for p in parts if isinstance(p, dict))
            elif isinstance(data, list):
                text = "\n".join(
                    p.get("text", "") for p in data if isinstance(p, dict) and p.get("text")
                )
            return text or "(no response)", None
        except json.JSONDecodeError:
            return stdout_bytes.decode("utf-8", errors="replace") or "(parse error)", None

    def env(self, openclaw_cfg: dict[str, Any]) -> dict[str, str]:
        env = {"GEMINI_CLI_TRUST_WORKSPACE": "true"}
        if api_key := _resolve_api_key(openclaw_cfg):
            env["GEMINI_API_KEY"] = api_key
        return env