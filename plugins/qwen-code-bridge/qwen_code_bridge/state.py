# -*- coding: utf-8 -*-
"""File-backed state directory for the coding-cli plugin (design doc §3).

All mutable plugin state (install task state; dispatch task state from P2)
lives under ``<workspace>/.qwenpaw/qwen-code-bridge/`` as files, so the plugin
survives hot reloads and restarts without module-level mutable globals.

Resolution priority:
1. ``QWENPAW_CODING_CLI_STATE_DIR`` env override (tests / ops).
2. ``$QWENPAW_WORKING_DIR/.qwenpaw/qwen-code-bridge`` — same env resolution as
   agentteams-matrix-channel (channel.py L3796-3802).
3. ``qwenpaw.constant.WORKING_DIR/.qwenpaw/qwen-code-bridge``.
"""

from __future__ import annotations

import os
from pathlib import Path

STATE_DIR_ENV = "QWEN_CODE_BRIDGE_STATE_DIR"
LEGACY_STATE_DIR_ENV = "QWENPAW_CODING_CLI_STATE_DIR"
WORKING_DIR_ENV = "QWENPAW_WORKING_DIR"


def _qwenpaw_working_dir() -> Path:
    env = os.environ.get(WORKING_DIR_ENV, "").strip()
    if env:
        return Path(env).expanduser()
    try:
        from qwenpaw.constant import WORKING_DIR
    except Exception:  # pragma: no cover - qwenpaw always present in host
        return Path.home() / ".qwenpaw"
    return Path(str(WORKING_DIR)).expanduser()


def state_dir() -> Path:
    """Resolve the plugin state directory (does not create it)."""
    override = os.environ.get(STATE_DIR_ENV, "").strip()
    if not override:
        override = os.environ.get(LEGACY_STATE_DIR_ENV, "").strip()
    if override:
        return Path(override).expanduser()
    return _qwenpaw_working_dir() / ".qwenpaw" / "qwen-code-bridge"


def install_dir() -> Path:
    """Directory holding install task files (``<task_id>.json``)."""
    return state_dir() / "install"


__all__ = ["STATE_DIR_ENV", "WORKING_DIR_ENV", "install_dir", "state_dir"]


def cli_home() -> str:
    """Persisted CLI HOME from env or state/config.json.

    Setting HOME (not just QWEN_HOME) is what moves the CLI session store:
    Storage uses ``os.homedir()/.qwen/projects/<dir>/chats``. Empty means
    the container-default home (lost on rebuild).
    """
    import json as _json
    import os as _os

    raw = _os.environ.get("QWEN_CODE_BRIDGE_CLI_HOME", "").strip()
    if raw:
        return raw
    try:
        cfg = state_dir() / "config.json"
        if cfg.exists():
            data = _json.loads(cfg.read_text(encoding="utf-8"))
            if isinstance(data, dict):
                value = data.get("cli_home") or data.get("qwen_home")
                return str(value or "").strip()
    except (OSError, ValueError):
        pass
    return ""


# Backwards-compatible name used by earlier builds.
def qwen_home() -> str:
    return cli_home()


#: Name of the worker-proxy policy hook registered in the dispatch CLI
#: home (idempotency key: an entry with the same name is replaced).
APPROVAL_HOOK_NAME = "agentteams-worker-proxy-policy"

#: Per-dispatch environment variable carrying the run roots (JSON array)
#: that ``hook_policy.py`` reads.  Set by the dispatch spawn path — a
#: per-task value, so it must not live in the shared settings.json.
APPROVAL_ROOTS_ENV = "AGENTTEAMS_APPROVAL_ROOTS"


def approval_hook_command() -> str:
    """Shell command running the policy hook under the current interpreter.

    Absolute path by design: a plugin upgrade may move the package, and
    ``prepare_cli_home`` rewrites the settings on every dispatch, picking
    up the new location.
    """
    import shlex
    import sys

    script = Path(__file__).resolve().with_name("hook_policy.py")
    return f"{shlex.quote(sys.executable)} {shlex.quote(str(script))}"


def ensure_approval_hook(qdir: Path) -> bool:
    """Idempotently install the worker-proxy PreToolUse hook.

    Merges into ``<qdir>/settings.json``: any existing hook entry named
    ``agentteams-worker-proxy-policy`` (under any PreToolUse matcher) is
    replaced by the fresh copy; every other setting and hook is
    preserved.  Returns False — file left untouched — when the settings
    are not a valid JSON object or cannot be written.
    """
    import json

    path = qdir / "settings.json"
    try:
        data: dict = {}
        if path.exists():
            loaded = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(loaded, dict):
                return False
            data = loaded
        hooks = data.get("hooks")
        if not isinstance(hooks, dict):
            hooks = {}
        pre = hooks.get("PreToolUse")
        if not isinstance(pre, list):
            pre = []
        ours = {
            "type": "command",
            "command": approval_hook_command(),
            "name": APPROVAL_HOOK_NAME,
            "timeout": 10,
        }
        kept: list = []
        for entry in pre:
            inner = entry.get("hooks") if isinstance(entry, dict) else None
            if isinstance(inner, list) and any(
                isinstance(h, dict) and h.get("name") == APPROVAL_HOOK_NAME
                for h in inner
            ):
                continue  # replaced by the fresh copy below
            kept.append(entry)
        kept.append({"matcher": "*", "hooks": [ours]})
        hooks["PreToolUse"] = kept
        data["hooks"] = hooks
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(
            json.dumps(data, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        os.replace(tmp, path)
        return True
    except (OSError, ValueError):
        return False


def prepare_cli_home(home: str) -> str:
    """Ensure ``<home>/.qwen`` has settings/token; return the QWEN_HOME path.

    Seeds from the current/default homes on first use so auth keeps working
    when HOME is redirected to a persisted path.
    """
    import shutil
    from pathlib import Path

    if not home:
        return ""
    base = Path(home).expanduser()
    qdir = base / ".qwen"
    qdir.mkdir(parents=True, exist_ok=True)
    for name in ("settings.json", "serve-token"):
        dst = qdir / name
        if dst.exists():
            continue
        for src in (
            Path.home() / ".qwen" / name,
            Path("/root/.qwen") / name,
            Path("/home/node/.qwen") / name,
        ):
            try:
                if src.exists():
                    shutil.copy2(src, dst)
                    break
            except OSError:
                continue
    # Dispatch-home policy hook (#1411): covers the tool calls the CLI
    # would auto-approve without a can_use_tool request.  Idempotent, and
    # never clobbers other settings or hooks.
    ensure_approval_hook(qdir)
    for name in ("settings.json", "serve-token"):
        try:
            (qdir / name).chmod(0o600)
        except OSError:
            pass
    return str(qdir)


def qwen_cli() -> str:
    """Persisted Qwen Code binary from env or state/config.json.

    Empty means "resolve 'qwen' from PATH" (container layer; lost on rebuild).
    """
    import json as _json
    import os as _os

    raw = _os.environ.get("QWEN_CODE_BRIDGE_CLI", "").strip()
    if raw:
        return raw
    try:
        cfg = state_dir() / "config.json"
        if cfg.exists():
            data = _json.loads(cfg.read_text(encoding="utf-8"))
            if isinstance(data, dict):
                return str(data.get("qwen_cli") or "").strip()
    except (OSError, ValueError):
        pass
    return ""
