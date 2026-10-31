# -*- coding: utf-8 -*-
"""Qwen Code Bridge plugin entry (management + dispatch + session tooling).

Compatibility contract (design doc §2):

- Only the two-generation intersection ``PluginApi`` methods are used
  (``register_http_router`` / ``register_startup_hook`` /
  ``register_shutdown_hook`` / ``register_middleware``; the last one
  carries the P2 session-injection middleware).
- New-framework-only capabilities are probed with ``hasattr`` (P1
  declares none).
- No runtime patching of framework internals, no module-level mutable
  state: all state lives
  under ``$WORKSPACE/.qwenpaw/coding-cli/`` as files (see
  ``qwen_code_bridge/state.py``).
- ``register`` is idempotent: a repeated call (e.g. new-framework
  update_config replay) detects existing registrations through the
  public registry introspection API and skips them instead of raising.
"""

import logging
import os
import sys
from pathlib import Path

from qwenpaw.plugins.api import PluginApi

logger = logging.getLogger(__name__)

PLUGIN_ID = "qwen-code-bridge"
HTTP_PREFIX = "/qwen-code-bridge"
STARTUP_HOOK_NAME = "qwen_code_bridge.ensure_state_dir"
SHUTDOWN_HOOK_NAME = "qwen_code_bridge.shutdown"
WAKE_QUEUE_HOOK_NAME = "qwen_code_bridge.wake_queue"

_PLUGIN_DIR = Path(os.path.dirname(os.path.abspath(__file__)))


def _ensure_importable() -> None:
    """Make the sibling backend package importable (workbench pattern)."""
    plugin_dir = str(_PLUGIN_DIR)
    if plugin_dir not in sys.path:
        sys.path.insert(0, plugin_dir)


def _registry(api: PluginApi):
    return getattr(api, "_registry", None)


def _http_prefix_owner(api: PluginApi, prefix: str) -> str | None:
    registry = _registry(api)
    if registry is None:
        return None
    for reg in registry.get_http_router_registrations():
        if reg.prefix == prefix:
            return reg.plugin_id
    return None





def _register_prompt_section(api: PluginApi) -> None:
    """Register the bridge guidance section (idempotent, best-effort)."""
    try:
        from qwen_code_bridge.prompt import bridge_prompt

        api.register_prompt_section(
            "qwen_code_bridge",
            after="workspace",
            provider=bridge_prompt,
            priority=60,
        )
        logger.info("prompt section qwen_code_bridge registered")
    except Exception as exc:
        logger.warning("prompt section registration skipped: %s", exc)


def _register_runtime_hook(api: PluginApi) -> None:
    """Register the PRE_DISPATCH bridge-chat hook (idempotent)."""
    registry = _registry(api)
    if registry is not None:
        try:
            if any(
                hook.plugin_id == PLUGIN_ID
                and "qwen_code_bridge_chat"
                in str(getattr(hook, "hook_name", ""))
                for hook in registry.get_startup_hooks()
            ):
                return
        except Exception:
            pass
    try:
        from qwen_code_bridge.runtime_hook import BridgeChatHook

        api.register_runtime_hook(BridgeChatHook())
        logger.info("runtime hook %s registered", BridgeChatHook.name)
    except Exception as exc:  # framework setup incomplete
        logger.warning("runtime hook registration skipped: %s", exc)


def _register_slash(api: PluginApi) -> None:
    """Register ``/qcode`` (alias ``/qwen``); idempotent, best-effort."""
    registry = _registry(api)
    if registry is not None:
        try:
            if any(
                hook.plugin_id == PLUGIN_ID
                and (
                    "slash_cmd_" in str(getattr(hook, "hook_name", ""))
                    or "slash_command_" in str(getattr(hook, "hook_name", ""))
                )
                for hook in registry.get_startup_hooks()
            ):
                logger.debug("slash command already registered; skipping")
                return
        except Exception:
            pass
    try:
        from qwen_code_bridge.slash import make_handler

        api.register_slash_command(
            "qcode",
            make_handler(),
            aliases=("qwen",),
            help_text="Talk to Qwen Code in this chat (per-session continuity)",
            category="plugin",
        )
        logger.info("slash command /qcode registered")
    except Exception as exc:  # older builds may lack slash support
        logger.warning("slash command registration skipped: %s", exc)


def _hook_owner(api: PluginApi, hooks, hook_name: str) -> str | None:
    for hook in hooks:
        if hook.hook_name == hook_name:
            return hook.plugin_id
    return None


def _ensure_qwen_link() -> None:
    """Keep /usr/local/bin/qwen pointing at the persisted CLI (if configured)."""
    from qwen_code_bridge.state import qwen_cli

    target = qwen_cli()
    if not target or not os.path.exists(target):
        return
    link = "/usr/local/bin/qwen"
    try:
        if os.path.islink(link) or not os.path.exists(link):
            if os.path.realpath(link) == os.path.realpath(target):
                return
            if os.path.lexists(link):
                os.unlink(link)
            os.symlink(target, link)
            logger.info("linked %s -> %s", link, target)
        else:
            logger.warning("%s exists and is not a symlink; leaving it", link)
    except OSError as exc:
        logger.warning("cannot link %s -> %s: %s", link, target, exc)


def _ensure_state_dir() -> None:
    from qwen_code_bridge.state import state_dir

    state_dir().mkdir(parents=True, exist_ok=True)
    _ensure_qwen_link()
    # B4: finalize tasks the previous process left running/cancelling
    # with a dead child pid (idempotent, never kills live processes).
    try:
        from qwen_code_bridge.dispatch import orphan_cleanup

        closed = orphan_cleanup()
        if closed:
            logger.info(
                "orphan cleanup closed %d stale dispatch task(s): %s",
                len(closed),
                ", ".join(str(t.get("task_id")) for t in closed),
            )
    except Exception as exc:
        logger.warning("orphan cleanup skipped: %s", exc)


async def _start_wake_queue() -> None:
    """Drain persisted terminal wakes (409-safe) as soon as the app is up.

    Startup hooks are awaited per plugin load, so a hot reload also resumes
    any wakes queued by the previous process.
    """
    from qwen_code_bridge import wake_queue
    from qwen_code_bridge.dispatch import (
        mark_wake_delivered,
        wake_is_delivered,
    )

    wake_queue.ensure_running(
        on_delivered=mark_wake_delivered,
        is_delivered=wake_is_delivered,
    )
    logger.info(
        "wake queue started (pending=%d)", len(wake_queue.pending())
    )


def _on_shutdown() -> None:
    try:
        from qwen_code_bridge import wake_queue

        wake_queue.stop()
    except Exception:
        pass
    logger.info(
        "%s shutdown (no persistent state to release in P1)",
        PLUGIN_ID,
    )


class QwenCodeBridgePlugin:
    """Plugin entry: P1 management surface (probe/settings/install)."""

    def register(self, api: PluginApi) -> None:
        _ensure_importable()
        from qwen_code_bridge.middleware import (
            session_middleware_factory,
        )
        from qwen_code_bridge.router import build_router

        registry = _registry(api)

        # 1) Management API under /api/qwen-code-bridge (idempotent).
        if registry is None:
            # Degraded path (framework setup incomplete): plain register;
            # the framework's own duplicate check still applies.
            api.register_http_router(
                build_router(),
                prefix=HTTP_PREFIX,
                tags=["qwen-code-bridge"],
            )
            api.register_startup_hook(
                STARTUP_HOOK_NAME,
                _ensure_state_dir,
                priority=200,
            )
            api.register_startup_hook(
                WAKE_QUEUE_HOOK_NAME,
                _start_wake_queue,
                priority=210,
            )
            api.register_shutdown_hook(
                SHUTDOWN_HOOK_NAME,
                _on_shutdown,
                priority=200,
            )
            api.register_middleware(session_middleware_factory, priority=50)
            _register_slash(api)
            _register_runtime_hook(api)
            _register_prompt_section(api)
            self._probe_new_framework(api)
            return

        owner = _http_prefix_owner(api, HTTP_PREFIX)
        if owner is None:
            api.register_http_router(
                build_router(),
                prefix=HTTP_PREFIX,
                tags=["qwen-code-bridge"],
            )
        elif owner == PLUGIN_ID:
            logger.debug(
                "HTTP prefix %s already registered; skipping",
                HTTP_PREFIX,
            )
        else:
            raise RuntimeError(
                f"HTTP prefix {HTTP_PREFIX} is owned by plugin {owner!r}; "
                f"refusing to register {PLUGIN_ID!r} on the same prefix",
            )

        # 2) Startup: pre-create the file-backed state directory (§3).
        owner = _hook_owner(
            api, registry.get_startup_hooks(), STARTUP_HOOK_NAME
        )
        if owner is None:
            api.register_startup_hook(
                STARTUP_HOOK_NAME,
                _ensure_state_dir,
                priority=200,
            )
        elif owner == PLUGIN_ID:
            logger.debug(
                "startup hook %s already registered; skipping",
                STARTUP_HOOK_NAME,
            )
        else:
            raise RuntimeError(
                f"startup hook {STARTUP_HOOK_NAME!r} is owned by plugin "
                f"{owner!r}",
            )

        owner = _hook_owner(
            api, registry.get_startup_hooks(), WAKE_QUEUE_HOOK_NAME
        )
        if owner is None:
            api.register_startup_hook(
                WAKE_QUEUE_HOOK_NAME,
                _start_wake_queue,
                priority=210,
            )
        elif owner == PLUGIN_ID:
            logger.debug(
                "startup hook %s already registered; skipping",
                WAKE_QUEUE_HOOK_NAME,
            )
        else:
            raise RuntimeError(
                f"startup hook {WAKE_QUEUE_HOOK_NAME!r} is owned by plugin "
                f"{owner!r}",
            )

        # 3) Shutdown: log only (file-backed state needs no release).
        owner = _hook_owner(
            api, registry.get_shutdown_hooks(), SHUTDOWN_HOOK_NAME
        )
        if owner is None:
            api.register_shutdown_hook(
                SHUTDOWN_HOOK_NAME,
                _on_shutdown,
                priority=200,
            )
        elif owner == PLUGIN_ID:
            logger.debug(
                "shutdown hook %s already registered; skipping",
                SHUTDOWN_HOOK_NAME,
            )
        else:
            raise RuntimeError(
                f"shutdown hook {SHUTDOWN_HOOK_NAME!r} is owned by plugin "
                f"{owner!r}",
            )

        # 4) Middleware: session env injection (design §5), priority 50.
        if any(
            reg.plugin_id == PLUGIN_ID
            for reg in registry.get_middleware_factories()
        ):
            logger.debug(
                "middleware factory already registered; skipping",
            )
        else:
            api.register_middleware(session_middleware_factory, priority=50)

        _register_slash(api)
        _register_runtime_hook(api)
        _register_prompt_section(api)
        self._probe_new_framework(api)
        logger.info(
            "%s registered: management API at /api%s",
            PLUGIN_ID,
            HTTP_PREFIX,
        )

    @staticmethod
    def _probe_new_framework(api: PluginApi) -> None:
        """Probe new-framework-only capabilities via hasattr (design §2).

        P1 declares no provision dependencies; keep the probe shape so
        the P2 dispatch surface can declare its dependencies here.
        """
        if hasattr(api, "provision"):
            logger.debug(
                "new-framework provision API detected (unused in P1)",
            )


plugin = QwenCodeBridgePlugin()
