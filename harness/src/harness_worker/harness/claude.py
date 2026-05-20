"""Claude Code harness adapter.

LLM Routing Architecture
------------------------
Higress ai-proxy 2.0 has Auto Protocol Detection: the gateway inspects the
request path to determine the wire format without any extra configuration.

  Client path              Detected format    Upstream
  /v1/chat/completions  →  OpenAI             MiniMax /v1/chat/completions
  /v1/messages          →  Anthropic (Claude) MiniMax /v1/chat/completions (converted)

Claude CLI always sends requests to ANTHROPIC_BASE_URL + /v1/messages, so
setting ANTHROPIC_BASE_URL to the Higress gateway URL is enough — no /anthropic
suffix required. Higress converts the Anthropic request to the upstream provider
format (OpenAI for MiniMax) before forwarding, and converts the response back.

Credential priority (resolved at bridge_config time):
  1. HICLAW_CLAUDE_BASE_URL + HICLAW_LLM_API_KEY   explicit operator override
  2. HICLAW_AI_GATEWAY_URL + HICLAW_WORKER_GATEWAY_KEY  default in cluster
       → Claude CLI  →  http://higress-gateway/v1/messages
       → Higress auto-detects Anthropic, converts, forwards to MiniMax
  3. _DEFAULT_BASE_URL + _DEFAULT_API_KEY           local dev fallback

Model constraint
----------------
The model sent in the request body must match a Higress AI route's
modelPredicate. The route for MiniMax-M2 already exists. If a worker uses a
different model (e.g. MiniMax-M2.7), a matching route must be created in
Higress or the route's model predicate updated.

Per-worker settings override
-----------------------------
Drop a file at MinIO path <worker>/.harness/claude.settings.json to inject
extra Claude CLI settings (e.g. customInstructions). Bridge merges it into
workspace/.claude/settings.json before controller-managed fields are applied,
so operator values (model, permissions, env) always take precedence.
"""
from __future__ import annotations

import json
import logging
import os
from pathlib import Path
from typing import Any

from harness_worker.harness.base import BaseHarness, register_harness

logger = logging.getLogger(__name__)

# Dev fallback: MiniMax Anthropic-compatible endpoint used when the cluster
# gateway env vars are absent (local development without a controller).
_DEFAULT_BASE_URL = "https://api.minimax.io/anthropic"
_DEFAULT_API_KEY = "apikey-testing"
_DEFAULT_MODEL = "MiniMax-M2.7"


def _resolve_active_model(cfg: dict[str, Any]) -> str:
    """Read active model id from openclaw.json agents.defaults.model.primary.

    Format: "hiclaw-gateway/MiniMax-M2"  →  returns "MiniMax-M2".
    The returned name is passed directly to `claude --model` and as the model
    field in every API request, so it must match a Higress route modelPredicate.
    """
    providers_raw = cfg.get("models", {}).get("providers", {})
    primary = cfg.get("agents", {}).get("defaults", {}).get("model", {}).get("primary", "")
    if primary and "/" in primary:
        pid, mid = primary.split("/", 1)
        provider = providers_raw.get(pid, {})
        for m in provider.get("models", []):
            if m.get("id") == mid:
                return mid
        if mid:
            return mid
    # Fallback: first model of the first configured provider
    for provider_cfg in providers_raw.values():
        models = provider_cfg.get("models", [])
        if models:
            return models[0].get("id", _DEFAULT_MODEL)
    return _DEFAULT_MODEL


def _resolve_credentials(openclaw_cfg: dict[str, Any]) -> tuple[str, str]:
    """Return (base_url, api_key) for Claude CLI's ANTHROPIC_* env vars.

    See module docstring for the full priority chain.
    """
    # Priority 1: explicit operator override — useful for pointing at a
    # different LLM provider or a custom Anthropic-compatible endpoint.
    explicit_url = os.environ.get("HICLAW_CLAUDE_BASE_URL", "")
    explicit_key = os.environ.get("HICLAW_LLM_API_KEY", "")
    if explicit_url and explicit_key:
        return explicit_url, explicit_key

    # Priority 2: Higress gateway (default in cluster).
    # The controller always injects both env vars into every worker pod.
    # Claude CLI calls ANTHROPIC_BASE_URL/v1/messages; Higress detects the
    # Anthropic path and converts the request to the upstream provider format.
    gateway_url = os.environ.get("HICLAW_AI_GATEWAY_URL", "")
    gateway_key = os.environ.get("HICLAW_WORKER_GATEWAY_KEY", "")
    if gateway_url and gateway_key:
        return gateway_url.rstrip("/"), gateway_key

    # Priority 3: local dev fallback (no controller / no gateway).
    return _DEFAULT_BASE_URL, _DEFAULT_API_KEY


@register_harness("claude")
class ClaudeHarness(BaseHarness):
    name = "claude"

    def __init__(self) -> None:
        self._model: str = _DEFAULT_MODEL
        self._base_url: str = _DEFAULT_BASE_URL
        self._api_key: str = _DEFAULT_API_KEY

    def bridge_config(self, openclaw_cfg: dict[str, Any], harness_home: Path) -> None:
        """Write workspace/.claude/settings.json from openclaw.json.

        harness_home is workspace_dir/.harness; settings go one level up so
        the Claude CLI picks them up from the workspace root.

        Merge order (later wins):
          1. Existing settings.json on disk  (user customisations survive restarts)
          2. <harness_home>/claude.settings.json  (per-worker MinIO override)
          3. Controller-managed fields: model, permissions, env
        """
        self._model = _resolve_active_model(openclaw_cfg)
        self._base_url, self._api_key = _resolve_credentials(openclaw_cfg)

        workspace = harness_home.parent
        cfg_file = workspace / ".claude" / "settings.json"
        cfg_file.parent.mkdir(parents=True, exist_ok=True)

        existing: dict[str, Any] = {}
        if cfg_file.exists():
            try:
                existing = json.loads(cfg_file.read_text())
            except (json.JSONDecodeError, OSError):
                existing = {}

        # Apply per-worker overrides synced from MinIO (e.g. customInstructions).
        # These are merged before controller fields so operators always win.
        override_file = harness_home / "claude.settings.json"
        if override_file.exists():
            try:
                overrides = json.loads(override_file.read_text())
                existing = _deep_merge(existing, overrides)
                logger.info("bridge: applied claude.settings.json overrides from %s", override_file)
            except (json.JSONDecodeError, OSError) as exc:
                logger.warning("bridge: ignoring invalid claude.settings.json: %s", exc)

        # Controller-managed fields — always overwrite whatever is on disk.
        existing["model"] = self._model
        # dontAsk: non-interactive mode required for subprocess invocation.
        # bypassPermissions is blocked when running as root (container default).
        existing["permissions"] = {"defaultMode": "dontAsk"}
        existing["env"] = {**existing.get("env", {}), **self._build_env()}

        cfg_file.write_text(json.dumps(existing, indent=2))
        logger.info(
            "bridge: claude settings → %s (model=%s, url=%s)",
            cfg_file, self._model, self._base_url,
        )

    def _build_env(self) -> dict[str, str]:
        # Set every ANTHROPIC_*_MODEL alias to the same value so Claude CLI
        # does not fall back to a model not registered in Higress.
        return {
            "ANTHROPIC_BASE_URL":                        self._base_url,
            "ANTHROPIC_API_KEY":                         self._api_key,
            "ANTHROPIC_AUTH_TOKEN":                      self._api_key,
            "ANTHROPIC_MODEL":                           self._model,
            "ANTHROPIC_SMALL_FAST_MODEL":                self._model,
            "ANTHROPIC_DEFAULT_SONNET_MODEL":            self._model,
            "ANTHROPIC_DEFAULT_OPUS_MODEL":              self._model,
            "ANTHROPIC_DEFAULT_HAIKU_MODEL":             self._model,
            "API_TIMEOUT_MS":                            "3000000",
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":  "1",
        }

    def build_command(
        self,
        message: str,
        session_id: str | None,
        workspace: Path,
    ) -> list[str]:
        # --output-format stream-json requires --verbose; both are mandatory for
        # streaming line-by-line parsing in worker._invoke_harness.
        argv = [
            "claude", "-p", message,
            "--output-format", "stream-json",
            "--verbose",
            "--model", self._model,
        ]
        if session_id:
            argv += ["--resume", session_id]
        return argv

    def process_stream_line(self, line: str, state: dict) -> None:
        # Stream-JSON events from `claude --output-format stream-json`:
        #   content_block_delta / text_delta  → accumulate text fragments
        #   result                            → capture session_id, fallback text
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            return

        event_type = event.get("type")

        if event_type == "content_block_delta":
            delta = event.get("delta", {})
            if delta.get("type") == "text_delta":
                state.setdefault("text_chunks", []).append(delta.get("text", ""))
            elif delta.get("type") == "thinking_delta":
                logger.debug("thinking chunk: %s", delta.get("thinking", "")[:80])

        elif event_type == "result":
            state["session_id"] = event.get("session_id")
            # Fallback: if no deltas were emitted, use the final result string.
            if not state.get("text_chunks"):
                result = event.get("result", "")
                if result:
                    state.setdefault("text_chunks", []).append(result)

    def parse_output(self, stdout_bytes: bytes) -> tuple[str, str | None]:
        state: dict = {}
        for line in stdout_bytes.decode("utf-8", errors="replace").splitlines():
            self.process_stream_line(line.strip(), state)
        text = "".join(state.get("text_chunks", [])) or "(no response)"
        return text, state.get("session_id")

    def env(self, openclaw_cfg: dict[str, Any]) -> dict[str, str]:
        model = _resolve_active_model(openclaw_cfg) if openclaw_cfg else self._model
        base_url, api_key = _resolve_credentials(openclaw_cfg) if openclaw_cfg else (self._base_url, self._api_key)
        return {
            "ANTHROPIC_BASE_URL":                        base_url,
            "ANTHROPIC_API_KEY":                         api_key,
            "ANTHROPIC_AUTH_TOKEN":                      api_key,
            "ANTHROPIC_MODEL":                           model,
            "ANTHROPIC_SMALL_FAST_MODEL":                model,
            "ANTHROPIC_DEFAULT_SONNET_MODEL":            model,
            "ANTHROPIC_DEFAULT_OPUS_MODEL":              model,
            "ANTHROPIC_DEFAULT_HAIKU_MODEL":             model,
            "API_TIMEOUT_MS":                            "3000000",
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":  "1",
        }


def _deep_merge(base: dict, override: dict) -> dict:
    """Merge override into base; dicts recurse, scalars replace."""
    out = dict(base)
    for k, v in override.items():
        if k in out and isinstance(out[k], dict) and isinstance(v, dict):
            out[k] = _deep_merge(out[k], v)
        else:
            out[k] = v
    return out
