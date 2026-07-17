"""Background sync loops."""

from __future__ import annotations

import asyncio
import logging
import time
from collections.abc import Awaitable, Callable

from agentteams_sync.exceptions import BridgeRuntimeError
from agentteams_sync.filesync import FileSync
from agentteams_sync.push import push_local
from agentteams_sync.types import HealthStateProtocol

logger = logging.getLogger(__name__)


async def push_loop(
    sync: FileSync,
    check_interval: int = 5,
    health: HealthStateProtocol | None = None,
    *,
    push_fn: Callable[..., list[str]] | None = None,
) -> None:
    """Background task: push local changes to MinIO every ``check_interval`` seconds."""
    do_push = push_fn or push_local
    last_push_time: float = 0.0

    while True:
        await asyncio.sleep(check_interval)
        try:
            now = time.time()
            pushed = await asyncio.get_event_loop().run_in_executor(
                None, do_push, sync, last_push_time
            )
            last_push_time = now
            if pushed:
                logger.info("FileSync push: uploaded %s", pushed)
            if health is not None:
                health.update(
                    "bridge",
                    "healthy",
                    "runtime-to-standard bridge completed",
                    {"operation": "bridge_runtime_to_standard"},
                )
                health.update(
                    "sync",
                    "healthy",
                    "runtime file persistence completed",
                    {"operation": "push_loop"},
                )
        except asyncio.CancelledError:
            break
        except BridgeRuntimeError as exc:
            logger.warning("FileSync runtime bridge error: %s", exc)
            if health is not None:
                health.update(
                    "bridge",
                    "unhealthy",
                    f"runtime-to-standard bridge failed: {exc}",
                    {
                        "operation": "bridge_runtime_to_standard",
                        "error_type": type(exc).__name__,
                    },
                )
        except Exception as exc:
            logger.warning("FileSync push error: %s", exc)
            if health is not None:
                health.update(
                    "sync",
                    "unhealthy",
                    f"runtime file persistence failed: {exc}",
                    {
                        "operation": "push_loop",
                        "error_type": type(exc).__name__,
                    },
                )


async def sync_loop(
    sync: FileSync,
    interval: int = 60,
    on_pull: Callable[[list[str]], Awaitable[None]] | None = None,
    health: HealthStateProtocol | None = None,
) -> None:
    """Background task: pull controller-managed worker files."""
    while True:
        await asyncio.sleep(interval)
        try:
            changed = await asyncio.get_event_loop().run_in_executor(None, sync.pull_all)
            if changed:
                logger.info("FileSync pull: files changed: %s", changed)
                if on_pull is not None:
                    await on_pull(changed)
            if health is not None:
                health.update(
                    "sync",
                    "healthy",
                    "runtime config pull completed",
                    {"operation": "sync_loop"},
                )
        except asyncio.CancelledError:
            break
        except Exception as exc:
            logger.warning("FileSync pull error: %s", exc)
            if health is not None:
                health.update(
                    "sync",
                    "unhealthy",
                    f"runtime config pull failed: {exc}",
                    {
                        "operation": "sync_loop",
                        "error_type": type(exc).__name__,
                    },
                )
