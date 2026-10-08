"""Adapter registry: AgentTeams runtime name -> CLI adapter."""
from __future__ import annotations

import os
from typing import Optional

from cli_harness_worker.adapters.base import CLIAdapter
from cli_harness_worker.adapters.atomcode import AtomCodeAdapter
from cli_harness_worker.adapters.claude_code import ClaudeCodeAdapter
from cli_harness_worker.adapters.codex import CodexAdapter
from cli_harness_worker.adapters.dsh import DSHAdapter
from cli_harness_worker.adapters.kimi_code import KimiCodeAdapter
from cli_harness_worker.adapters.pi import PiAdapter

# Default when no runtime is discoverable (e.g. sandbox backend, which
# cannot inject container env).
DEFAULT_RUNTIME = "claude-code"

# All runtime names understood by this harness, including legacy aliases.
_REGISTRY: dict[str, type[CLIAdapter]] = {
    "atomcode": AtomCodeAdapter,
    "codex": CodexAdapter,
    "claude-code": ClaudeCodeAdapter,
    "kimi-code": KimiCodeAdapter,
    "pi": PiAdapter,
    "dsh": DSHAdapter,
}

# Alternative spellings accepted from AGENTTEAMS_WORKER_RUNTIME /
# AGENTTEAMS_CLI_ADAPTER so typos in CR specs fail soft instead of leaving
# the Worker with no adapter. Note: "deepseek-harness" is deliberately NOT
# an alias — upstream ships a dedicated deepseek-harness runtime on its own
# image, and it must keep routing there.
RUNTIME_ALIASES: dict[str, str] = {
    "claudecode": "claude-code",
    "claude": "claude-code",
    "kimi": "kimi-code",
    "kimicode": "kimi-code",
}


def normalize_runtime(raw: str) -> str:
    """Map a raw runtime string onto a known adapter name (default fallback)."""
    candidate = (raw or "").strip().lower().replace("_", "-")
    candidate = RUNTIME_ALIASES.get(candidate, candidate)
    if candidate in _REGISTRY:
        return candidate
    return DEFAULT_RUNTIME


def get_adapter(
    runtime: str,
    workspace: Optional[str] = None,
) -> CLIAdapter:
    """Instantiate the adapter for ``runtime``."""
    name = normalize_runtime(runtime)
    return _REGISTRY[name](workspace=workspace)


def adapter_names() -> list[str]:
    return sorted(_REGISTRY)
