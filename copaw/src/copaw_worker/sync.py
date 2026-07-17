"""MinIO file sync for copaw-worker.

Re-exports ``agentteams_sync`` with CoPaw-specific runtime bridge hooks.
Import paths remain ``copaw_worker.sync`` for hooks, tests, and worker startup.
"""
from __future__ import annotations

import logging
import shutil
import subprocess
from pathlib import Path

import agentteams_sync.mc as mc_ops
from agentteams_sync.exceptions import BridgeRuntimeError
from agentteams_sync.filesync import FileSync
from agentteams_sync.helpers import team_storage_name_from_worker_team as _team_storage_name_from_worker_team
from agentteams_sync.loops import push_loop as _push_loop
from agentteams_sync.loops import sync_loop as _sync_loop_impl
from agentteams_sync.mc import (
    bind_active_mc,
    preview_text,
    redacted_mc_command,
)
from agentteams_sync.policy import PushPolicy
from agentteams_sync.push import push_local as _push_local_impl
from agentteams_sync.types import HealthStateProtocol, SharedPath

from copaw_worker.bridge import bridge_runtime_to_standard

__all__ = [
    "BridgeRuntimeError",
    "FileSync",
    "HealthStateProtocol",
    "PushPolicy",
    "SharedPath",
    "push_local",
    "push_loop",
    "sync_loop",
    "_mc",
    "_team_storage_name_from_worker_team",
    "shutil",
    "subprocess",
]


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
    logger = logging.getLogger(__name__)
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
    """Delegate to the current ``copaw_worker.sync._mc`` (supports test monkeypatch)."""
    import copaw_worker.sync as sync_mod

    return sync_mod._mc(
        *args,
        check=check,
        warn_on_error=warn_on_error,
        log_output=log_output,
    )


bind_active_mc(_mc_dispatch)

_COPAW_PUSH_POLICY = PushPolicy.copaw()


def _copaw_pre_push(local_dir: Path) -> None:
    try:
        bridge_runtime_to_standard(local_dir)
    except Exception as exc:
        raise BridgeRuntimeError(str(exc)) from exc


def push_local(sync: FileSync, since: float = 0) -> list[str]:
    """Push local changes with CoPaw inner→outer bridge before upload."""
    return _push_local_impl(
        sync,
        since,
        policy=_COPAW_PUSH_POLICY,
        pre_push=_copaw_pre_push,
    )


async def push_loop(
    sync: FileSync,
    check_interval: int = 5,
    health: HealthStateProtocol | None = None,
) -> None:
    """Background push loop using CoPaw ``push_local`` (includes bridge hook)."""
    await _push_loop(sync, check_interval=check_interval, health=health, push_fn=push_local)


sync_loop = _sync_loop_impl
