"""Credential guard: credagent.json → CoPaw File Guard integration.

Reads HiClaw's ``config/credagent.json`` protocol and injects the declared
credential paths into CoPaw's ``security.file_guard.sensitive_files`` config.
Also installs a monkey-patch hook on ``CoPawAgent._decide_guard_action`` so
that ``SENSITIVE_FILE_ACCESS`` findings are auto-denied (no user approval).
"""

from __future__ import annotations

import json
import logging
import tempfile
from pathlib import Path

logger = logging.getLogger(__name__)

_GUARD_HOOK_INSTALLED = False


def apply_credential_guard(standard_dir: Path, runtime_dir: Path) -> int:
    """Read credagent.json and inject paths into CoPaw's config.json security section.

    Returns the number of protected paths applied.
    """
    credagent_path = standard_dir / "config" / "credagent.json"
    if not credagent_path.exists():
        return 0

    try:
        spec = json.loads(credagent_path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError) as exc:
        logger.warning("Failed to read credagent.json: %s", exc)
        return 0

    credentials = spec.get("credentials", [])

    # Normalize each entry: expand path, preserve all protocol fields
    normalized: list[dict] = []
    paths: list[str] = []
    for entry in credentials:
        raw = entry.get("path", "")
        if not raw:
            continue
        expanded = str(Path(raw).expanduser())
        if raw.endswith("/") and not expanded.endswith("/"):
            expanded += "/"
        paths.append(expanded)
        norm_entry: dict = {"path": expanded}
        permit = entry.get("programPermit", [])
        if isinstance(permit, str):
            permit = [permit]
        norm_entry["programPermit"] = permit
        norm_entry["writable"] = bool(entry.get("writable", False))
        normalized.append(norm_entry)

    config_path = runtime_dir / "config.json"
    try:
        config = json.loads(config_path.read_text(encoding="utf-8")) if config_path.exists() else {}
    except (json.JSONDecodeError, OSError):
        config = {}

    security = config.setdefault("security", {})

    # Read old credagent paths BEFORE overwriting, so we can diff
    prev_credagent_paths = set(
        e.get("path", "") for e in security.get("credagent", {}).get("credentials", [])
    )

    # Store full credagent entries for future FUSE migration
    security["credagent"] = {"credentials": normalized}

    fg = security.setdefault("file_guard", {})
    fg["enabled"] = True
    # Remove previously injected credagent paths, then add current ones.
    # This ensures deleted credagent entries don't linger.
    existing = set(fg.get("sensitive_files", [])) - prev_credagent_paths
    existing.update(paths)
    fg["sensitive_files"] = sorted(existing)

    tg = security.setdefault("tool_guard", {})
    tg["enabled"] = True

    # Atomic write via temp file + rename to avoid partial reads
    tmp_fd = tempfile.NamedTemporaryFile(
        mode="w", dir=config_path.parent, suffix=".tmp", delete=False, encoding="utf-8",
    )
    try:
        tmp_fd.write(json.dumps(config, indent=2, ensure_ascii=False))
        tmp_fd.close()
        Path(tmp_fd.name).replace(config_path)
    except BaseException:
        Path(tmp_fd.name).unlink(missing_ok=True)
        raise
    # Load user-defined output sanitize rules
    output_sanitize = spec.get("output_sanitize", [])
    if isinstance(output_sanitize, list) and output_sanitize:
        from copaw_worker.hooks.output_sanitizer import get_sanitizer

        get_sanitizer().load_user_rules(output_sanitize)
        logger.info("credential guard: loaded %d user sanitize rules", len(output_sanitize))

    logger.info(
        "credential guard: injected %d paths into %s (total sensitive_files=%d)"
        " [programPermit/writable stored but not enforced in app-layer mode]",
        len(paths),
        config_path,
        len(fg["sensitive_files"]),
    )
    return len(paths)


def install_credential_guard_hook() -> None:
    """Make SENSITIVE_FILE_ACCESS findings auto-denied (no user approval)."""
    global _GUARD_HOOK_INSTALLED
    if _GUARD_HOOK_INSTALLED:
        return

    from copaw.agents.react_agent import CoPawAgent

    if not hasattr(CoPawAgent, "_decide_guard_action"):
        logger.warning("credential guard: CoPawAgent._decide_guard_action not found, skipping hook")
        _GUARD_HOOK_INSTALLED = True
        return

    original = CoPawAgent._decide_guard_action
    if getattr(original, "_hiclaw_credential_guard", False):
        _GUARD_HOOK_INSTALLED = True
        return

    async def _decide_with_credential_block(self, tool_call):  # type: ignore[override]
        action = await original(self, tool_call)
        if action is None:
            return None
        if (
            action.kind == "needs_approval"
            and action.guard_result
            and action.guard_result.findings
        ):
            from copaw.security.tool_guard.models import GuardThreatCategory

            if any(
                f.category == GuardThreatCategory.SENSITIVE_FILE_ACCESS
                for f in action.guard_result.findings
            ):
                action.kind = "auto_denied"
                logger.warning(
                    "credential guard: auto-denied %s (SENSITIVE_FILE_ACCESS)",
                    action.tool_name,
                )
        return action

    _decide_with_credential_block._hiclaw_credential_guard = True  # type: ignore[attr-defined]
    CoPawAgent._decide_guard_action = _decide_with_credential_block  # type: ignore[assignment]
    _GUARD_HOOK_INSTALLED = True
    logger.info("Installed HiClaw credential guard hook (SENSITIVE_FILE_ACCESS → auto_denied)")
