"""MinIO Client (mc) helpers."""

from __future__ import annotations

import logging
import os
import shutil
import subprocess

logger = logging.getLogger(__name__)


def storage_alias() -> str:
    explicit = os.environ.get("AGENTTEAMS_STORAGE_ALIAS")
    if explicit:
        return explicit
    prefix = os.environ.get("AGENTTEAMS_STORAGE_PREFIX") or ""
    if "/" in prefix:
        return prefix.split("/", 1)[0]
    return "agentteams"


def preview_text(value: str | None, limit: int = 2000) -> str:
    if not value:
        return ""
    if len(value) <= limit:
        return value
    return value[:limit] + "...<truncated>"


def redact_url_userinfo(value: str) -> str:
    if "://" not in value:
        return value
    scheme, rest = value.split("://", 1)
    if "@" not in rest:
        return value
    return f"{scheme}://<redacted>@{rest.split('@', 1)[1]}"


def redacted_mc_command(cmd: list[str]) -> list[str]:
    redacted = [redact_url_userinfo(part) for part in cmd]
    args = redacted[1:]
    if len(args) >= 6 and args[0] == "alias" and args[1] == "set":
        redacted[5] = "<redacted-access-key>"
        redacted[6] = "<redacted-secret-key>"
    return redacted


def looks_like_remote_directory_error(exc: subprocess.CalledProcessError) -> bool:
    stderr = str(exc.stderr or "")
    stdout = str(exc.stdout or "")
    text = f"{stderr}\n{stdout}"
    return "--recursive flag is required" in text


def looks_like_missing_object_error(stderr: str | None) -> bool:
    text = stderr or ""
    return "Object does not exist" in text or "The specified key does not exist" in text


def mc(
    *args: str,
    check: bool = True,
    warn_on_error: bool = True,
    log_output: bool = True,
) -> subprocess.CompletedProcess:
    """Run an mc command and return the result."""
    mc_bin = shutil.which("mc")
    if not mc_bin:
        raise RuntimeError("mc binary not found on PATH. Please install mc first.")
    cmd = [mc_bin, *args]
    redacted_cmd = redacted_mc_command(cmd)
    logger.info("mc cmd: %s", " ".join(redacted_cmd))
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=check)
    except subprocess.CalledProcessError as exc:
        exc.cmd = redacted_cmd
        log = logger.warning if warn_on_error else logger.debug
        log(
            "mc command failed returncode=%s cmd=%s stdout=%r stderr=%r",
            exc.returncode,
            " ".join(redacted_cmd),
            preview_text(exc.stdout),
            preview_text(exc.stderr),
        )
        raise
    if log_output:
        logger.info("mc stdout (%d chars): %r", len(result.stdout), result.stdout[:200])
        if result.stderr:
            logger.info("mc stderr: %r", result.stderr[:200])
    return result


def bind_active_mc(callable_mc) -> None:
    """Replace the module-level ``mc`` (used by FileSync after CoPaw shim wiring)."""
    global mc
    mc = callable_mc
