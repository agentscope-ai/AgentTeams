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

import asyncio
import fcntl
import json
import logging
import os
import pty
import re
import shutil
import signal
from pathlib import Path
from typing import Any

from harness_worker.harness.base import BaseHarness, register_harness

logger = logging.getLogger(__name__)

_MAX_ACTIVITY_LINES = 20

# ── REPL / pty constants ──────────────────────────────────────────────────────
# 3s of silence = REPL command finished. API-calling commands (/compact) can
# take 30+ s; the outer timeout_seconds cap prevents infinite waits.
_REPL_IDLE_SEC = 3.0
_REPL_READ_CHUNK = 4096
# 8 iterations × 0.7s/screen = max 5.6s to get through all wizard screens.
_REPL_WIZARD_MAX_SCREENS = 8
# Drain up to 6s (20 × 0.3s) after /exit for final goodbye output.
_REPL_EXIT_DRAIN_ROUNDS = 20
_REPL_EXIT_DRAIN_TIMEOUT = 0.3
# Markers that indicate the REPL prompt is fully drawn and ready for input.
_REPL_PROMPT_MARKERS = ("⏵⏵", "for shortcuts", "/help for help")

_ANSI_RE = re.compile(r"\x1b(?:\[[0-9;?]*[a-zA-Z]|\][^\x07]*\x07|[=>])")

# ── REPL output cleaning ──────────────────────────────────────────────────────
# Substrings that identify a line as claude-cli chrome (banner, mode line,
# auth warnings). Coupled to claude-cli v2.x UI; update when the CLI changes.
_REPL_CHROME_SUBSTRINGS = (
    "Claude Code v",
    "API Usage Billing",
    "is now available",
    "/model to switch",
    "don't ask on (shift+tab",
    "for shortcuts",
    "Auth conflict",
    "ANTHROPIC_AUTH_TOKEN",
    "ANTHROPIC_API_KEY",
    "Unset the ANTHROPIC",
    "claude/logout",
    "Trying to use",
    "/effort",
    "Press Esc",
    "Esc to interrupt",
)
_REPL_CHROME_PREFIXES = ("╭", "╰", "│", "▐", "▌", "▝", "▘", "▛", "▜", "▟", "█", "─", "•")
_REPL_CHROME_EXACT = frozenset({"", ">", "│", "╰─", "/exit", "❯", "·", "?"})

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


def _build_anthropic_env(base_url: str, api_key: str, model: str) -> dict[str, str]:
    """Build the ANTHROPIC_* env dict passed to every claude subprocess.

    Every model alias is pinned to the same value so Claude CLI never falls
    back to a model not registered in Higress. ANTHROPIC_AUTH_TOKEN and
    ANTHROPIC_API_KEY are both set to the same key; if claude-cli starts
    warning about "auth conflict" for a new version, unset AUTH_TOKEN here.
    """
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
        (workspace / "memory").mkdir(parents=True, exist_ok=True)

        # Suppress the first-run onboarding wizard. Claude CLI has TWO config locations:
        #   ~/.claude/settings.json     → user preferences (theme, etc.)
        #   ~/.claude.json              → app state (hasCompletedOnboarding, userID, ...)
        # The wizard checks `hasCompletedOnboarding` in ~/.claude.json; without it the TUI
        # walks the user through 3-4 screens (theme → trust folder → "press enter") that
        # cannot be answered with a single keystroke and block the REPL.
        _home = Path.home()
        _home_settings = _home / ".claude" / "settings.json"
        _home_settings.parent.mkdir(parents=True, exist_ok=True)
        if not _home_settings.exists():
            _home_settings.write_text(json.dumps({"theme": "dark"}, indent=2))

        _claude_state = _home / ".claude.json"
        try:
            _state = json.loads(_claude_state.read_text()) if _claude_state.exists() else {}
            if not isinstance(_state, dict):
                _state = {}
        except (json.JSONDecodeError, OSError):
            _state = {}
        _state.update({
            "hasCompletedOnboarding": True,
            "bypassPermissionsModeAccepted": True,
            "theme": "dark",
            "tipsHistory": _state.get("tipsHistory") or {},
        })
        # Pre-approve the custom ANTHROPIC_API_KEY so Claude CLI doesn't show the
        # "Detected a custom API key in your environment — use it?" prompt.
        # The CLI stores approval by the last 20 chars of the key.
        _api_key_suffix = self._api_key[-20:] if len(self._api_key) >= 20 else self._api_key
        _existing_resp = _state.get("customApiKeyResponses") or {}
        _approved = list(_existing_resp.get("approved") or [])
        if _api_key_suffix and _api_key_suffix not in _approved:
            _approved.append(_api_key_suffix)
        _state["customApiKeyResponses"] = {
            "approved": _approved,
            "rejected": list(_existing_resp.get("rejected") or []),
        }
        # Mark this workspace as a trusted project so the "Trust folder?" prompt is skipped.
        _state.setdefault("projects", {})
        _proj = _state["projects"].setdefault(str(workspace), {})
        _proj["hasTrustDialogAccepted"] = True
        _proj["hasCompletedProjectOnboarding"] = True
        _claude_state.write_text(json.dumps(_state, indent=2))

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
        return _build_anthropic_env(self._base_url, self._api_key, self._model)

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

    async def invoke_repl(
        self,
        message: str,
        session_id: str | None,
        workspace: Path,
        env: dict[str, str],
        timeout_seconds: float,
    ) -> tuple[str, str | None]:
        argv = ["claude", "--model", self._model]
        if session_id:
            argv += ["--resume", session_id]

        master_fd, slave_fd = pty.openpty()
        buf = bytearray()
        loop = asyncio.get_running_loop()

        try:
            proc = await asyncio.create_subprocess_exec(
                *argv,
                stdin=slave_fd,
                stdout=slave_fd,
                stderr=slave_fd,
                env=env,
                cwd=str(workspace),
                start_new_session=True,
            )
            # Close slave_fd in the parent immediately after fork so the master
            # gets EOF when the child exits (keeping it open would block reads).
            os.close(slave_fd)
            slave_fd = None  # mark consumed; finally block skips it

            # Non-blocking reads so add_reader can drive the event loop
            fl = fcntl.fcntl(master_fd, fcntl.F_GETFL)
            fcntl.fcntl(master_fd, fcntl.F_SETFL, fl | os.O_NONBLOCK)

            data_queue: asyncio.Queue = asyncio.Queue()

            def _on_readable() -> None:
                try:
                    chunk = os.read(master_fd, _REPL_READ_CHUNK)
                    if chunk:
                        data_queue.put_nowait(chunk)
                except BlockingIOError:
                    pass
                except OSError:
                    # EIO: slave side closed (process exited)
                    loop.remove_reader(master_fd)
                    data_queue.put_nowait(None)  # EOF sentinel

            loop.add_reader(master_fd, _on_readable)
            try:
                # --- Phase 1: wait for REPL prompt to be fully drawn ---
                # The mode indicator line ("⏵⏵ don't ask on ... · /effort") is the
                # last thing claude prints before it's ready for input. Waiting for
                # it (instead of using an idle timeout) prevents the banner from
                # bleeding into command output if claude is slow to flush.
                initial_buf = bytearray()
                phase1_deadline = loop.time() + 8.0
                prompt_seen = False
                while loop.time() < phase1_deadline:
                    try:
                        item = await asyncio.wait_for(data_queue.get(), timeout=1.0)
                    except asyncio.TimeoutError:
                        clean = _ANSI_RE.sub("", initial_buf.decode("utf-8", errors="replace"))
                        if any(m in clean for m in _REPL_PROMPT_MARKERS):
                            prompt_seen = True
                            break
                        continue
                    if item is None:
                        break
                    initial_buf.extend(item)
                    clean = _ANSI_RE.sub("", initial_buf.decode("utf-8", errors="replace"))
                    if not prompt_seen and any(m in clean for m in _REPL_PROMPT_MARKERS):
                        prompt_seen = True
                        # Drain a bit more in case trailing chrome is still flushing
                        try:
                            await asyncio.sleep(0.3)
                            while True:
                                extra = await asyncio.wait_for(data_queue.get(), timeout=0.2)
                                if extra is None:
                                    break
                                initial_buf.extend(extra)
                        except asyncio.TimeoutError:
                            pass
                        break

                initial_text = initial_buf.decode("utf-8", errors="replace")

                # --- Phase 2: handle any onboarding/security wizard screens ---
                # bridge_config() pre-seeds ~/.claude.json with hasCompletedOnboarding +
                # customApiKeyResponses, but Claude CLI can still show ad-hoc prompts
                # (theme, trust folder, "custom API key detected", "press enter to continue").
                # Loop: detect any prompt → answer it → drain → check again, until REPL is ready.
                async def _drain_screen() -> str:
                    sbuf = bytearray()
                    while True:
                        try:
                            it = await asyncio.wait_for(data_queue.get(), timeout=0.5)
                        except asyncio.TimeoutError:
                            break
                        if it is None:
                            break
                        sbuf.extend(it)
                    return sbuf.decode("utf-8", errors="replace")

                def _classify(text: str) -> str | None:
                    # Strip ANSI before matching so cursor pointer chars don't break detection
                    clean = _ANSI_RE.sub("", text)
                    if "custom API key" in clean or "Detected a custom API key" in clean:
                        return "api_key"  # default cursor is on "No"; we need "1" for Yes
                    if "Choose the text style" in clean or "Dark mode" in clean and "Light mode" in clean:
                        return "theme"  # dark already pre-selected with ❯; just press Enter
                    if "Trust the files" in clean or "Do you trust" in clean:
                        return "trust"
                    if "Press Enter" in clean or "press Enter" in clean:
                        return "enter"
                    return None

                kind = _classify(initial_text)
                if kind:
                    logger.info("invoke_repl: wizard detected (kind=%s), auto-answering", kind)
                    for screen in range(_REPL_WIZARD_MAX_SCREENS):
                        # Re-classify on each loop with latest buffer
                        if kind == "api_key":
                            # Type "1" then Enter to pick "Yes"
                            os.write(master_fd, b"1\r")
                        else:
                            # theme/trust/enter — dark is pre-selected, just Enter
                            os.write(master_fd, b"\r")
                        await asyncio.sleep(0.7)
                        screen_text = await _drain_screen()
                        clean_screen = _ANSI_RE.sub("", screen_text)
                        if any(m in clean_screen for m in _REPL_PROMPT_MARKERS) or "Try \"" in clean_screen:
                            logger.info("invoke_repl: REPL prompt reached after %d screen(s)", screen + 1)
                            break
                        next_kind = _classify(screen_text)
                        if next_kind is None:
                            # No new prompt detected — assume REPL is ready (or wizard finished silently)
                            await asyncio.sleep(0.4)
                            break
                        kind = next_kind
                    # Reset buf — wizard output would confuse _clean_repl_output
                    initial_buf = bytearray()

                buf.extend(initial_buf)

                # --- Phase 3: send the slash command ---
                # In raw TTY mode, Enter is CR (\r), not LF (\n). Claude REPL only
                # submits the input when it sees \r — \n shows up as text-in-buffer
                # without triggering the slash command handler.
                os.write(master_fd, (message + "\r").encode("utf-8"))

                # --- Phase 4: collect command output with idle detection ---
                deadline = loop.time() + timeout_seconds
                while loop.time() < deadline:
                    remaining = deadline - loop.time()
                    idle_wait = min(_REPL_IDLE_SEC, remaining)
                    try:
                        item = await asyncio.wait_for(
                            data_queue.get(), timeout=idle_wait
                        )
                    except asyncio.TimeoutError:
                        # No new output for _REPL_IDLE_SEC → command finished
                        break
                    if item is None:
                        break  # EOF: process exited on its own
                    buf.extend(item)

                # --- Phase 5: exit REPL cleanly ---
                try:
                    os.write(master_fd, b"/exit\r")
                except OSError:
                    pass

                for _ in range(_REPL_EXIT_DRAIN_ROUNDS):
                    try:
                        item = await asyncio.wait_for(data_queue.get(), timeout=_REPL_EXIT_DRAIN_TIMEOUT)
                        if item is None:
                            break
                        buf.extend(item)
                    except asyncio.TimeoutError:
                        break

            finally:
                try:
                    loop.remove_reader(master_fd)
                except Exception:
                    pass

            # Terminate if still running (e.g. timeout path)
            if proc.returncode is None:
                try:
                    os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
                except (ProcessLookupError, OSError):
                    pass

            await asyncio.wait_for(proc.wait(), timeout=5.0)

        except asyncio.TimeoutError:
            pass
        finally:
            if slave_fd is not None:  # not yet closed (error before fork completed)
                try:
                    os.close(slave_fd)
                except OSError:
                    pass
            try:
                os.close(master_fd)
            except OSError:
                pass

        text = self._clean_repl_output(buf.decode("utf-8", errors="replace"), message)
        return text or "(no REPL output)", session_id

    @staticmethod
    def _clean_repl_output(raw: str, sent_message: str) -> str:
        text = _ANSI_RE.sub("", raw)
        text = text.replace("\r\n", "\n").replace("\r", "")

        # Cut at REPL goodbye — everything after is post-exit chrome.
        for marker in ("Resume this session with:", "claude --resume "):
            if marker in text:
                text = text[:text.index(marker)]
                break

        needle = sent_message.strip()
        seen_echo = not needle

        lines = []
        for ln in text.split("\n"):
            if not seen_echo and needle and needle in ln:
                seen_echo = True
                continue
            stripped = ln.strip()
            if stripped in _REPL_CHROME_EXACT:
                continue
            if stripped.startswith(_REPL_CHROME_PREFIXES):
                continue
            if any(s in ln for s in _REPL_CHROME_SUBSTRINGS):
                continue
            lines.append(ln)
        return "\n".join(lines).strip()

    def process_stream_line(self, line: str, state: dict) -> None:
        # Stream-JSON events from `claude --output-format stream-json --verbose`:
        #   Wrapped CLI events (primary):
        #     system/init  → session bootstrap
        #     assistant    → text + tool_use content blocks
        #     user         → tool_result content blocks
        #     result       → session_id, cost, duration
        #   Raw SSE events (fallback for legacy CLI versions):
        #     content_block_start/delta/stop, content_block_delta/text_delta
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            if line.strip():
                logger.debug("claude raw: %s", line[:200])
            return

        event_type = event.get("type")

        # --- Wrapped CLI events ---
        if event_type == "assistant":
            self._handle_assistant_message(event, state)
        elif event_type == "user":
            self._handle_user_message(event, state)
        elif event_type == "system":
            if event.get("subtype") == "init":
                state["session_id"] = event.get("session_id")
                logger.info("claude session init: %s", state["session_id"])

        # --- SSE pass-through events (fallback) ---
        elif event_type == "content_block_start":
            cb = event.get("content_block", {})
            if cb.get("type") == "tool_use":
                idx = event.get("index")
                state.setdefault("active_tools", {})[idx] = {
                    "name": cb.get("name", "unknown"),
                    "input_fragments": [],
                }
                logger.info("claude tool start: %s", cb.get("name"))
        elif event_type == "content_block_delta":
            delta = event.get("delta", {})
            dt = delta.get("type")
            if dt == "text_delta":
                state.setdefault("text_chunks", []).append(delta.get("text", ""))
            elif dt == "thinking_delta":
                logger.debug("claude thinking: %s", delta.get("thinking", "")[:80])
            elif dt == "input_json_delta":
                idx = event.get("index")
                tools = state.get("active_tools", {})
                if idx in tools:
                    tools[idx]["input_fragments"].append(delta.get("partial_json", ""))
        elif event_type == "content_block_stop":
            idx = event.get("index")
            tools = state.get("active_tools", {})
            if idx in tools:
                self._log_completed_tool(tools.pop(idx), state)

        # --- Final result event ---
        elif event_type == "result":
            state["session_id"] = event.get("session_id")
            usage = event.get("usage") or {}
            input_tokens = usage.get("input_tokens")
            output_tokens = usage.get("output_tokens")
            dur = event.get("duration_ms")
            turns = event.get("num_turns")
            if input_tokens is not None or output_tokens is not None or dur is not None:
                logger.info(
                    "claude result: input_tokens=%s output_tokens=%s duration=%sms turns=%s",
                    input_tokens, output_tokens, dur, turns,
                )
                dur_sec = f"{dur / 1000:.1f}s" if dur else "?"
                in_str = str(input_tokens) if input_tokens is not None else "?"
                out_str = str(output_tokens) if output_tokens is not None else "?"
                turns_str = str(turns) if turns is not None else "?"
                state.setdefault("text_chunks", []).append(
                    f"\n> 📊 **in/out** {in_str}/{out_str} tok · ⏱ {dur_sec} · {turns_str} turns\n"
                )
            overflow = state.get("activity_count", 0) - _MAX_ACTIVITY_LINES
            if overflow > 0:
                state.setdefault("text_chunks", []).append(f"\n> _… +{overflow} more tool calls_\n")
            # Fallback: if no content was emitted, use the final result string.
            if not state.get("text_chunks"):
                result = event.get("result", "")
                if result:
                    state.setdefault("text_chunks", []).append(result)

    def _handle_assistant_message(self, event: dict, state: dict) -> None:
        msg = event.get("message", {})
        for block in msg.get("content", []):
            btype = block.get("type")
            if btype == "text":
                text = block.get("text", "")
                if text:
                    state.setdefault("text_chunks", []).append(text)
            elif btype == "tool_use":
                self._log_tool_use(block.get("name", "unknown"), block.get("input") or {}, state)

    def _handle_user_message(self, event: dict, state: dict) -> None:
        msg = event.get("message", {})
        content = msg.get("content", [])
        if isinstance(content, str):
            return
        for block in content:
            if block.get("type") != "tool_result":
                continue
            raw = block.get("content")
            if isinstance(raw, list):
                text = "".join(c.get("text", "") for c in raw if isinstance(c, dict))
            else:
                text = str(raw) if raw is not None else ""
            is_err = block.get("is_error", False)
            preview = text.strip().replace("\n", " ⏎ ")[:200]
            if is_err:
                if state.get("activity_count", 0) < _MAX_ACTIVITY_LINES:
                    state.setdefault("text_chunks", []).append(f"\n> ❌ **error**: {preview}\n")
                logger.warning("claude tool_result (ERROR): %s", preview)
            else:
                logger.info("claude tool_result: %s", preview)
                if preview and state.get("activity_count", 0) < _MAX_ACTIVITY_LINES:
                    state.setdefault("text_chunks", []).append(f"\n> ✅ `{preview[:80]}`\n")

    def _log_completed_tool(self, tool_data: dict, state: dict) -> None:
        raw = "".join(tool_data["input_fragments"])
        try:
            args = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            logger.warning("claude tool %s: unparseable args: %s", tool_data["name"], raw[:200])
            return
        self._log_tool_use(tool_data["name"], args, state)

    @staticmethod
    def _format_tool_ui(name: str, args: dict) -> str:
        if name == "Bash":
            cmd = str(args.get("command", "")).replace("\n", " ↵ ")
            if len(cmd) > 120:
                cmd = cmd[:117] + "..."
            return f"🔧 **Bash**: `{cmd}`"
        elif name == "Read":
            return f"📖 **Read**: `{args.get('file_path') or args.get('path') or '?'}`"
        elif name in ("Edit", "Write", "MultiEdit", "NotebookEdit", "Replace"):
            return f"✍️ **{name}**: `{args.get('file_path') or args.get('path') or '?'}`"
        elif name in ("Glob", "Grep", "Search"):
            return f"🔍 **{name}**: `{args.get('pattern') or args.get('query') or '?'}`"
        elif name in ("Browser", "WebSearch", "WebFetch", "Fetch"):
            return f"🌐 **{name}**: `{args.get('url') or args.get('query') or '?'}`"
        elif name == "AskUser":
            return f"👤 **Ask**: `{str(args.get('question', '...'))[:120]}`"
        elif name == "TodoWrite":
            todos = args.get("todos") or []
            return f"📝 **TodoWrite**: {len(todos)} item(s)"
        elif name == "Task":
            desc = args.get("description") or args.get("prompt", "")
            return f"🤖 **Task**: `{str(desc)[:120]}`"
        elif name.startswith("mcp__"):
            clean_name = name.replace("mcp__", "").replace("__", ": ", 1).replace("__", "_")
            mcp_arg = ""
            if args:
                first_val = str(next(iter(args.values()), ""))
                if first_val:
                    mcp_arg = f" `{first_val[:50]}{'...' if len(first_val) > 50 else ''}`"
            return f"🔌 **{clean_name}**{mcp_arg}"
        else:
            return f"⚙️ **{name}**"

    def _log_tool_use(self, name: str, args: dict, state: dict) -> None:
        if name == "Bash":
            logger.info("claude Bash: %s", str(args.get("command", ""))[:300])
        elif name in ("Edit", "Write", "Read", "MultiEdit", "NotebookEdit", "Replace"):
            logger.info("claude %s: %s", name, args.get("file_path") or args.get("path") or "?")
        elif name in ("Glob", "Grep", "Search"):
            logger.info("claude %s: %s", name, args.get("pattern") or args.get("query") or "?")
        elif name.startswith("mcp__"):
            logger.info("claude MCP %s: %s", name, json.dumps(args)[:200])
        else:
            logger.info("claude tool %s: %s", name, json.dumps(args)[:200])

        count = state.get("activity_count", 0)
        if count < _MAX_ACTIVITY_LINES:
            state.setdefault("text_chunks", []).append(f"\n> {self._format_tool_ui(name, args)}\n")
        state["activity_count"] = count + 1

    def parse_output(self, stdout_bytes: bytes) -> tuple[str, str | None]:
        state: dict = {}
        for line in stdout_bytes.decode("utf-8", errors="replace").splitlines():
            self.process_stream_line(line.strip(), state)
        text = "".join(state.get("text_chunks", [])) or "(no response)"
        return text, state.get("session_id")

    def env(self, openclaw_cfg: dict[str, Any]) -> dict[str, str]:
        if openclaw_cfg:
            model = _resolve_active_model(openclaw_cfg)
            base_url, api_key = _resolve_credentials(openclaw_cfg)
        else:
            model, base_url, api_key = self._model, self._base_url, self._api_key
        return _build_anthropic_env(base_url, api_key, model)


def _deep_merge(base: dict, override: dict) -> dict:
    """Merge override into base; dicts recurse, scalars replace."""
    out = dict(base)
    for k, v in override.items():
        if k in out and isinstance(out[k], dict) and isinstance(v, dict):
            out[k] = _deep_merge(out[k], v)
        else:
            out[k] = v
    return out
