"""CLI coding-agent adapters.

Each adapter knows how to invoke one headless CLI coding agent with a single
task prompt and collect its final output. Adapters also declare the
environment variables (API base URL / key / model) they consume so the bridge
can forward openclaw.json provider config.

Adapter selection order (see normalize_runtime):
  1. AGENTTEAMS_WORKER_RUNTIME  — injected by the controller backends
  2. AGENTTEAMS_CLI_ADAPTER     — escape hatch for backends that cannot pass
                                  env (e.g. SandboxClaim) or custom images
  3. DEFAULT_RUNTIME            — claude-code
"""
from __future__ import annotations

from cli_harness_worker.adapters.base import AdapterError, CLIAdapter, RunResult
from cli_harness_worker.adapters.registry import (
    DEFAULT_RUNTIME,
    RUNTIME_ALIASES,
    adapter_names,
    get_adapter,
    normalize_runtime,
)

__all__ = [
    "AdapterError",
    "CLIAdapter",
    "DEFAULT_RUNTIME",
    "RUNTIME_ALIASES",
    "RunResult",
    "adapter_names",
    "get_adapter",
    "normalize_runtime",
]
