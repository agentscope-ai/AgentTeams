"""OpenClaw Worker sync — PULL_MARKER push guard and fallback pull loop."""

from __future__ import annotations

import asyncio
import logging
import os
from pathlib import Path
from typing import Optional

from agentteams_openclaw_merge import merge_openclaw_config

from agentteams_sync.contract import OPENCLAW
from agentteams_sync.filesync import FileSync as _BaseFileSync
from agentteams_sync import mc as mc_ops
from agentteams_sync.mc import storage_alias

logger = logging.getLogger(__name__)

PULL_MARKER_NAME = ".last-pull"
PROMPT_FILES = ("SOUL.md", "AGENTS.md", "HEARTBEAT.md")

_BULK_MIRROR_EXCLUDES = (
    "openclaw.json",
    "config/mcporter.json",
    "mcporter-servers.json",
    ".agents/**",
    "credentials/**",
    ".cache/**",
    ".npm/**",
    ".local/**",
    ".mc/**",
    "*.lock",
    PULL_MARKER_NAME,
    ".openclaw/matrix/**",
    ".openclaw/canvas/**",
    "SOUL.md",
    "AGENTS.md",
    "HEARTBEAT.md",
)


class FileSync(_BaseFileSync):
    """OpenClaw MinIO sync — workspace at ``/root/hiclaw-fs/agents/{name}``."""

    def __init__(
        self,
        endpoint: str,
        access_key: str,
        secret_key: str,
        bucket: str,
        worker_name: str,
        secure: bool = False,
        local_dir: Optional[Path] = None,
        shared_dir: Optional[Path] = None,
    ) -> None:
        agentteams_root = Path(os.environ.get("AGENTTEAMS_ROOT", "/root/hiclaw-fs"))
        workspace = local_dir or agentteams_root / "agents" / worker_name
        super().__init__(
            endpoint=endpoint,
            access_key=access_key,
            secret_key=secret_key,
            bucket=bucket,
            worker_name=worker_name,
            secure=secure,
            local_dir=workspace,
            shared_dir=shared_dir or agentteams_root / "shared",
            pull_includes_shared=True,
        )

    def _get_shared_remote(self) -> str:
        """OpenClaw contract uses global ``shared/`` (not team-scoped)."""
        return f"{storage_alias()}/{self.bucket}/shared/"

    @property
    def pull_marker(self) -> Path:
        return self.local_dir / PULL_MARKER_NAME

    def touch_pull_marker(self) -> None:
        self.pull_marker.touch(exist_ok=True)


def _has_files_newer_than_marker(local_dir: Path, marker: Path) -> bool:
    if not local_dir.is_dir():
        return False
    if not marker.exists():
        return True
    marker_mtime = marker.stat().st_mtime
    for path in local_dir.rglob("*"):
        if not path.is_file():
            continue
        if path == marker:
            continue
        try:
            if path.stat().st_mtime > marker_mtime:
                return True
        except OSError:
            continue
    return False


def push_local_openclaw(sync: FileSync) -> list[str]:
    """Change-triggered push matching ``worker-entrypoint.sh`` bulk mirror + per-file prompts."""
    marker = sync.pull_marker
    if not _has_files_newer_than_marker(sync.local_dir, marker):
        return []

    sync._ensure_alias()
    pushed: list[str] = []
    remote = sync._object_path(f"{sync._prefix}/")
    local = str(sync.local_dir) + "/"
    args: list[str] = ["mirror", local, remote, "--overwrite"]
    for exclude in _BULK_MIRROR_EXCLUDES:
        args.extend(["--exclude", exclude])

    try:
        result = mc_ops.mc(*args, check=False, warn_on_error=True)
        if result.returncode != 0:
            logger.warning("OpenClaw bulk push mirror failed: %s", result.stderr)
        else:
            pushed.append("bulk-mirror")
    except Exception as exc:
        logger.warning("OpenClaw bulk push mirror error: %s", exc)

    for name in PROMPT_FILES:
        local_file = sync.local_dir / name
        if not local_file.is_file():
            continue
        if not marker.exists() or local_file.stat().st_mtime <= marker.stat().st_mtime:
            continue
        key = f"{sync._prefix}/{name}"
        try:
            mc_ops.mc("cp", str(local_file), sync._object_path(key), check=False, warn_on_error=True)
            pushed.append(name)
        except Exception as exc:
            logger.debug("OpenClaw prompt push failed for %s: %s", name, exc)

    return pushed


def pull_fallback_openclaw(sync: FileSync) -> list[str]:
    """Remote→Local fallback pull (Manager-managed paths + shared newer-than 5m)."""
    changed: list[str] = []
    sync._ensure_alias()

    remote_openclaw = sync._cat(f"{sync._prefix}/openclaw.json")
    local_openclaw = sync.local_dir / "openclaw.json"
    if remote_openclaw is not None:
        existing = local_openclaw.read_text(encoding="utf-8") if local_openclaw.exists() else None
        try:
            merged = (
                merge_openclaw_config(remote_openclaw, existing)
                if existing is not None
                else remote_openclaw
            )
        except Exception as exc:
            logger.warning("openclaw.json merge failed, using remote: %s", exc)
            merged = remote_openclaw
        if merged != existing:
            local_openclaw.parent.mkdir(parents=True, exist_ok=True)
            local_openclaw.write_text(merged, encoding="utf-8")
            changed.append("openclaw.json")

    for rel in ("config/mcporter.json",):
        content = sync._cat(f"{sync._prefix}/{rel}")
        if content is None:
            continue
        local_path = sync.local_dir / rel
        existing = local_path.read_text(encoding="utf-8") if local_path.exists() else None
        if existing != content:
            local_path.parent.mkdir(parents=True, exist_ok=True)
            local_path.write_text(content, encoding="utf-8")
            changed.append(rel)

    skills_remote = sync._object_path(f"{sync._prefix}/skills/")
    skills_local = sync.local_dir / "skills"
    skills_local.mkdir(parents=True, exist_ok=True)
    try:
        result = mc_ops.mc(
            "mirror",
            skills_remote,
            str(skills_local) + "/",
            "--overwrite",
            check=False,
        )
        if result.returncode == 0:
            for sh in skills_local.rglob("*.sh"):
                sh.chmod(sh.stat().st_mode | 0o111)
            changed.append("skills/")
    except Exception as exc:
        logger.warning("OpenClaw skills pull failed: %s", exc)

    shared_remote = sync._get_shared_remote()
    sync.shared_dir.mkdir(parents=True, exist_ok=True)
    try:
        result = mc_ops.mc(
            "mirror",
            shared_remote,
            str(sync.shared_dir) + "/",
            "--overwrite",
            "--newer-than",
            "5m",
            check=False,
        )
        if result.returncode == 0:
            changed.append("shared/")
    except Exception as exc:
        logger.warning("OpenClaw shared pull failed: %s", exc)

    sync.touch_pull_marker()
    return changed


async def openclaw_push_loop(
    sync: FileSync,
    check_interval: int = OPENCLAW.push_check_interval_seconds,
) -> None:
    """Background push loop — first check runs immediately (bash parity)."""
    while True:
        try:
            pushed = await asyncio.get_event_loop().run_in_executor(None, push_local_openclaw, sync)
            if pushed:
                logger.info("OpenClaw push: %s", pushed)
        except asyncio.CancelledError:
            break
        except Exception as exc:
            logger.warning("OpenClaw push loop error: %s", exc)
        await asyncio.sleep(check_interval)


async def openclaw_pull_loop(
    sync: FileSync,
    interval: int = OPENCLAW.pull_interval_seconds or 300,
) -> None:
    """Background pull loop — first pull after ``interval`` seconds (bash parity)."""
    while True:
        await asyncio.sleep(interval)
        try:
            changed = await asyncio.get_event_loop().run_in_executor(None, pull_fallback_openclaw, sync)
            if changed:
                logger.info("OpenClaw pull: %s", changed)
        except asyncio.CancelledError:
            break
        except Exception as exc:
            logger.warning("OpenClaw pull loop error: %s", exc)


async def run_openclaw_daemon(sync: FileSync) -> None:
    await asyncio.gather(
        openclaw_push_loop(sync),
        openclaw_pull_loop(sync),
    )
