"""Codex CLI harness adapter."""
from __future__ import annotations

import json
import logging
from pathlib import Path
from typing import Any

from harness_worker.harness.base import BaseHarness, register_harness

logger = logging.getLogger(__name__)


def _resolve_api_key(cfg: dict[str, Any]) -> str:
    providers = cfg.get("models", {}).get("providers", {})
    for v in providers.values():
        if key := v.get("apiKey", ""):
            return key
    return ""


@register_harness("codex")
class CodexHarness(BaseHarness):
    name = "codex"

    def bridge_config(self, openclaw_cfg: dict[str, Any], harness_home: Path) -> None:
        cfg_file = harness_home / ".codex" / "config.toml"
        if cfg_file.exists():
            return

        cfg_file.parent.mkdir(parents=True, exist_ok=True)
        api_key = _resolve_api_key(openclaw_cfg)

        content = f"""[model]
default = "gpt-5.5"

[approval_policy]
mode = "on-request"

[sandbox]
mode = "workspace-write"

[features]
memories = false
undo = false
hooks = true
"""
        if api_key:
            content += f'\n[auth]\napikey = "{api_key}"\n'

        cfg_file.write_text(content)
        logger.info("bridge: codex config written to %s", cfg_file)

    def build_command(
        self,
        message: str,
        session_id: str | None,
        workspace: Path,
    ) -> list[str]:
        argv = ["codex", "exec", message, "--json", "--ephemeral", "--sandbox", "workspace-write"]
        return argv

    def parse_output(self, stdout_bytes: bytes) -> tuple[str, str | None]:
        lines = stdout_bytes.decode("utf-8", errors="replace").splitlines()
        text_parts = []
        for line in lines:
            line = line.strip()
            if not line:
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            msg = event.get("message", {})
            if isinstance(msg, dict):
                content = msg.get("content", [])
                if isinstance(content, list):
                    for block in content:
                        if isinstance(block, dict) and block.get("type") == "text":
                            text_parts.append(block.get("text", ""))
        text = "\n".join(text_parts) if text_parts else "(no response)"
        return text, None

    def env(self, openclaw_cfg: dict[str, Any]) -> dict[str, str]:
        env = {}
        if api_key := _resolve_api_key(openclaw_cfg):
            env["CODEX_API_KEY"] = api_key
        return env