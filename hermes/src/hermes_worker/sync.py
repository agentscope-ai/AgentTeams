"""MinIO file sync for hermes-worker.

Re-exports ``agentteams_sync`` with Hermes-specific contract hooks.
Import paths remain ``hermes_worker.sync`` for worker startup and tests.
"""
from __future__ import annotations

import logging
import shutil
import subprocess
from pathlib import Path
from typing import Optional

import agentteams_sync.mc as mc_ops
from agentteams_sync.contract import HERMES
from agentteams_sync.filesync import FileSync as _BaseFileSync
from agentteams_sync.loops import push_loop as _push_loop
from agentteams_sync.loops import sync_loop as _sync_loop_impl
from agentteams_sync.mc import bind_active_mc, preview_text, redacted_mc_command
from agentteams_sync.policy import PushPolicy
from agentteams_sync.push import push_local as _push_local_impl

logger = logging.getLogger(__name__)

_HERMES_DERIVED_FILES = frozenset({"config.yaml", ".env"})
_INNER_OUTER_FILES = ("AGENTS.md", "SOUL.md")
_HERMES_PUSH_POLICY = PushPolicy.hermes()


class FileSync(_BaseFileSync):
    """Hermes MinIO sync — shared library with Hermes ``SyncContract`` presets."""

    def __init__(
        self,
        endpoint: str,
        access_key: str,
        secret_key: str,
        bucket: str,
        worker_name: str,
        secure: bool = False,
        local_dir: Optional[Path] = None,
    ) -> None:
        super().__init__(
            endpoint=endpoint,
            access_key=access_key,
            secret_key=secret_key,
            bucket=bucket,
            worker_name=worker_name,
            secure=secure,
            local_dir=local_dir or Path("/root/hiclaw-fs/agents") / worker_name,
            team_resolver="agents_md",
            pull_includes_shared=HERMES.pull_all_includes_shared,
            pull_includes_global_shared=HERMES.pull_all_includes_global_shared,
        )


def _mc(
    *args: str,
    check: bool = True,
    warn_on_error: bool = True,
    log_output: bool = True,
) -> subprocess.CompletedProcess:
    """Run mc using this module's ``shutil`` / ``subprocess`` (test patch target)."""
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


def _mc_dispatch(
    *args: str,
    check: bool = True,
    warn_on_error: bool = True,
    log_output: bool = True,
) -> subprocess.CompletedProcess:
    import hermes_worker.sync as sync_mod

    return sync_mod._mc(
        *args,
        check=check,
        warn_on_error=warn_on_error,
        log_output=log_output,
    )


bind_active_mc(_mc_dispatch)


def _hermes_pre_push(local_dir: Path) -> None:
    """Propagate Hermes inner copies (.hermes/) back to workspace root before push."""
    hermes_home = local_dir / ".hermes"
    for name in _INNER_OUTER_FILES:
        inner = hermes_home / name
        outer = local_dir / name
        if not inner.exists():
            continue
        try:
            inner_mtime = inner.stat().st_mtime
        except OSError:
            continue
        outer_mtime = outer.stat().st_mtime if outer.exists() else 0
        if inner_mtime <= outer_mtime:
            continue
        inner_content = inner.read_text(errors="replace")
        outer_content = outer.read_text(errors="replace") if outer.exists() else ""
        if inner_content != outer_content:
            outer.write_text(inner_content)
            logger.debug("Inner→Outer sync: .hermes/%s → %s", name, name)


def _hermes_extra_skip(rel: Path) -> bool:
    return (
        len(rel.parts) == 2
        and rel.parts[0] == ".hermes"
        and rel.name in _HERMES_DERIVED_FILES
    )


def push_local(sync: FileSync, since: float = 0) -> list[str]:
    """Push local changes with Hermes inner→outer bridge and byte-accurate compare."""
    return _push_local_impl(
        sync,
        since,
        policy=_HERMES_PUSH_POLICY,
        pre_push=_hermes_pre_push,
        extra_skip=_hermes_extra_skip,
        compare_bytes=True,
    )


async def push_loop(sync: FileSync, check_interval: int = HERMES.push_check_interval_seconds) -> None:
    """Background push loop using Hermes ``push_local``."""
    await _push_loop(sync, check_interval=check_interval, push_fn=push_local)


sync_loop = _sync_loop_impl

__all__ = [
    "FileSync",
    "push_local",
    "push_loop",
    "sync_loop",
    "_mc",
]
