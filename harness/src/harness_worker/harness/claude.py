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
import shutil
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
        """Write workspace/.claude/settings.json, .claude.json (MCP), CLAUDE.md, and .claude/skills/.

        harness_home is workspace_dir/.harness; settings go one level up so
        the Claude CLI picks them up from the workspace root.

        Merge order for settings.json (later wins):
          1. Existing settings.json on disk      (user customisations survive)
          2. <harness_home>/claude.settings.json (per-worker MinIO override)
          3. Controller-managed fields: model, permissions, env (always win)

        MCP servers are written separately to .claude.json under
        projects[workspace]["mcpServers"] — Claude Code reads project-level
        MCP servers from there, not from settings.json["mcpServers"].

        Side effects:
          - workspace/.claude.json  MCP servers updated (controller owns the key)
          - workspace/CLAUDE.md     generated from SOUL.md + AGENTS.md
          - workspace/.claude/skills/ synced from workspace/skills/ via symlinks
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

        # Remove stale mcpServers from settings.json — Claude Code reads
        # project-level MCP servers from .claude.json, not from settings.json.
        existing.pop("mcpServers", None)

        # Controller-managed fields — always overwrite whatever is on disk.
        existing["model"] = self._model
        # dontAsk: non-interactive mode required for subprocess invocation.
        # bypassPermissions is blocked when running as root (container default).
        # allow mcp__* so native MCP tool calls are not denied in dontAsk mode.
        existing["permissions"] = {"defaultMode": "dontAsk", "allow": ["mcp__*", "Write(*)", "Read(*)", "Bash(*)"]}
        existing["env"] = {**existing.get("env", {}), **self._build_env()}

        cfg_file.write_text(json.dumps(existing, indent=2))

        # Write MCP servers to .claude.json under projects[workspace]["mcpServers"].
        # Claude Code stores project-level MCP servers here (type "http" / "sse" / "stdio").
        self._write_mcp_dot_claude(workspace, self._build_mcp_servers(workspace, harness_home))
        logger.info(
            "bridge: claude settings → %s (model=%s, url=%s)",
            cfg_file, self._model, self._base_url,
        )

        # Generate CLAUDE.md from SOUL.md + AGENTS.md so Claude CLI has the
        # agent's persona and behaviour rules as project instructions.
        self._generate_claude_md(workspace)

        # Mirror workspace/skills/ → workspace/.claude/skills/ so Claude Code
        # discovers skills natively without listing them in CLAUDE.md.
        self._sync_skills_dir(workspace)

        # Copy .harness/claudeignore → workspace/.claudeignore so Claude Code
        # respects operator-defined ignore patterns.
        self._write_claudeignore(workspace, harness_home)

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

    def _write_mcp_dot_claude(self, workspace: Path, mcp_servers: dict[str, Any]) -> None:
        """Write project-level MCP servers into workspace/.claude.json.

        Claude Code reads project-level MCP servers from
        .claude.json["projects"][cwd]["mcpServers"], NOT from settings.json.
        Since HOME = workspace in the harness container, .claude.json is at
        workspace/.claude.json.

        Controller fully owns the mcpServers key: the entire dict is replaced
        so stale entries from previous runs (persisted in MinIO) are removed.
        All other .claude.json content (cachedGrowthBookFeatures, etc.) is preserved.
        """
        dot_claude = workspace / ".claude.json"
        try:
            data: dict[str, Any] = json.loads(dot_claude.read_text(encoding="utf-8")) if dot_claude.exists() else {}
        except (json.JSONDecodeError, OSError):
            data = {}

        workspace_key = str(workspace)
        data.setdefault("projects", {}).setdefault(workspace_key, {})

        if mcp_servers:
            data["projects"][workspace_key]["mcpServers"] = mcp_servers
        else:
            data["projects"][workspace_key].pop("mcpServers", None)

        dot_claude.write_text(json.dumps(data, indent=2), encoding="utf-8")
        logger.info("bridge: wrote %d MCP server(s) to .claude.json projects[%s]", len(mcp_servers), workspace_key)

    def _build_mcp_servers(self, workspace: Path, harness_home: Path) -> dict[str, Any]:
        """Read config/mcporter.json and .harness/mcp-local.json; return mcpServers for .claude.json.

        Transport mapping:
          "http"  → {"type": "http", "url": ...}   (MCP Streamable HTTP)
          "sse"   → {"type": "sse",  "url": ...}   (SSE persistent connection)
          "stdio" → {"type": "stdio", "command": ..., "args": ..., "env": ...}

        Sources (later entries win on name collision):
          1. workspace/config/mcporter.json  — Manager-managed HTTP/SSE servers
          2. workspace/mcporter-servers.json — backward-compat fallback
          3. .harness/mcp-local.json         — harness-local stdio/HTTP override (not pushed to MinIO)
        """
        _TRANSPORT_MAP = {"http": "http", "sse": "sse"}
        result: dict[str, Any] = {}

        # --- HTTP/SSE from Manager-managed mcporter.json ---
        for candidate in (
            workspace / "config" / "mcporter.json",
            workspace / "mcporter-servers.json",
        ):
            if not candidate.exists():
                continue
            try:
                config = json.loads(candidate.read_text(encoding="utf-8"))
            except (json.JSONDecodeError, OSError):
                continue
            for name, srv in config.get("mcpServers", {}).items():
                transport = srv.get("transport", "http")
                claude_type = _TRANSPORT_MAP.get(transport)
                if claude_type and srv.get("url"):
                    entry: dict[str, Any] = {"type": claude_type, "url": srv["url"]}
                    if srv.get("headers"):
                        entry["headers"] = srv["headers"]
                    result[name] = entry
            if result:
                break

        # --- stdio (and additional HTTP/SSE) from .harness/mcp-local.json ---
        local_cfg = harness_home / "mcp-local.json"
        if local_cfg.exists():
            try:
                local = json.loads(local_cfg.read_text(encoding="utf-8"))
                for name, srv in local.get("mcpServers", {}).items():
                    transport = srv.get("transport", "stdio")
                    if transport == "stdio" and srv.get("command"):
                        entry = {"type": "stdio", "command": srv["command"]}
                        if srv.get("args"):
                            entry["args"] = srv["args"]
                        if srv.get("env"):
                            entry["env"] = srv["env"]
                        result[name] = entry
                    elif _TRANSPORT_MAP.get(transport) and srv.get("url"):
                        entry = {"type": _TRANSPORT_MAP[transport], "url": srv["url"]}
                        if srv.get("headers"):
                            entry["headers"] = srv["headers"]
                        result[name] = entry
                logger.info("bridge: loaded mcp-local.json (%d total MCP server(s))", len(result))
            except (json.JSONDecodeError, OSError) as exc:
                logger.warning("bridge: ignoring invalid mcp-local.json: %s", exc)

        if result:
            logger.info("bridge: wiring %d MCP server(s) to .claude.json", len(result))
        return result

    def _generate_claude_md(self, workspace: Path) -> None:
        """Generate workspace/CLAUDE.md from SOUL.md + AGENTS.md.

        Claude CLI reads CLAUDE.md automatically as project instructions.
        Source files are NOT modified so copaw/hermes runtimes remain compatible.
        If neither file exists, CLAUDE.md is left untouched.
        """
        parts: list[str] = []
        for fname in ("SOUL.md", "AGENTS.md"):
            f = workspace / fname
            if f.exists():
                try:
                    content = f.read_text(encoding="utf-8").strip()
                    if content:
                        parts.append(content)
                except OSError:
                    pass
        if not parts:
            return
        claude_md = workspace / "CLAUDE.md"
        claude_md.write_text("\n\n---\n\n".join(parts) + "\n", encoding="utf-8")
        logger.info("bridge: generated CLAUDE.md (%d sections)", len(parts))

    def _sync_skills_dir(self, workspace: Path) -> None:
        """Mirror workspace/skills/ → workspace/.claude/skills/ via symlinks.

        Claude Code discovers skills from .claude/skills/<name>/SKILL.md.
        Symlinks avoid data duplication; push_loop still pushes from workspace/skills/.
        Stale symlinks for removed skills are cleaned up automatically.
        Non-symlink entries (user-managed) are left untouched.
        """
        src_dir = workspace / "skills"
        dst_dir = workspace / ".claude" / "skills"
        if not src_dir.is_dir():
            return
        dst_dir.mkdir(parents=True, exist_ok=True)

        current_skills = {d.name for d in src_dir.iterdir() if d.is_dir()}

        for existing in list(dst_dir.iterdir()):
            if existing.name not in current_skills and existing.is_symlink():
                existing.unlink()

        for skill_name in current_skills:
            skill_link = dst_dir / skill_name
            skill_target = (src_dir / skill_name).resolve()
            if skill_link.is_symlink():
                if skill_link.resolve() == skill_target:
                    continue
                skill_link.unlink()
            elif skill_link.exists():
                continue  # user-managed directory, don't touch
            skill_link.symlink_to(skill_target)

        logger.info("bridge: synced %d skills to .claude/skills/", len(current_skills))

    def _write_claudeignore(self, workspace: Path, harness_home: Path) -> None:
        """Copy .harness/claudeignore → workspace/.claudeignore.

        Operator drops .harness/claudeignore in MinIO to control what Claude Code
        ignores when scanning project files. Falls back to safe defaults if absent.
        """
        src = harness_home / "claudeignore"
        dst = workspace / ".claudeignore"
        if src.exists():
            shutil.copy2(src, dst)
            logger.info("bridge: wrote .claudeignore from %s", src)
        elif not dst.exists():
            dst.write_text(
                "# generated by hiclaw harness\n"
                ".harness/\n"
                ".claude/\n"
                "*.tar\n"
                "*.log\n",
                encoding="utf-8",
            )

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
