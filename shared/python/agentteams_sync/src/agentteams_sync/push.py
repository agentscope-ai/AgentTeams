"""Local -> Remote push using PushPolicy."""

from __future__ import annotations

import hashlib
import logging
from collections.abc import Callable
from pathlib import Path

from agentteams_sync.filesync import FileSync
from agentteams_sync import mc as mc_ops
from agentteams_sync.policy import PushPolicy

logger = logging.getLogger(__name__)


def push_local(
    sync: FileSync,
    since: float = 0,
    *,
    policy: PushPolicy | None = None,
    pre_push: Callable[[Path], None] | None = None,
    extra_skip: Callable[[Path], bool] | None = None,
    compare_bytes: bool = False,
    max_compare_bytes: int | None = None,
) -> list[str]:
    """Push locally-changed files back to MinIO. Returns list of pushed keys."""
    effective_policy = policy or PushPolicy.copaw()
    pushed: list[str] = []
    local_dir = sync.local_dir
    if not local_dir.exists():
        return pushed

    if pre_push is not None:
        pre_push(local_dir)

    sync._ensure_alias()

    for path in local_dir.rglob("*"):
        if not path.is_file():
            continue
        try:
            if path.stat().st_mtime <= since:
                continue
        except OSError:
            continue
        rel = path.relative_to(local_dir)
        if effective_policy.should_skip(rel):
            continue
        if extra_skip is not None and extra_skip(rel):
            continue

        key = f"{sync._prefix}/{rel.as_posix()}"
        try:
            mtime = path.stat().st_mtime
            if compare_bytes:
                size = path.stat().st_size
                if max_compare_bytes is None or size <= max_compare_bytes:
                    local_bytes = path.read_bytes()
                    cat_bytes = getattr(sync, "_cat_bytes", None)
                    if cat_bytes is not None:
                        remote_bytes = cat_bytes(key)
                    else:
                        remote = sync._cat(key)
                        remote_bytes = remote.encode() if remote is not None else None
                    if remote_bytes == local_bytes:
                        continue
            else:
                local_content = path.read_text(errors="replace")
                content_hash = hashlib.sha256(
                    local_content.encode("utf-8", errors="replace")
                ).hexdigest()

                cached = sync._push_content_cache.get(key)
                if cached is not None and cached[0] == mtime and cached[1] == content_hash:
                    continue

                remote = sync._cat(key)
                if remote == local_content:
                    sync._push_content_cache[key] = (mtime, content_hash)
                    continue

            dest = sync._object_path(key)
            mc_ops.mc("cp", str(path), dest, check=True)
            if not compare_bytes:
                sync._push_content_cache[key] = (
                    mtime,
                    hashlib.sha256(path.read_text(errors="replace").encode("utf-8", errors="replace")).hexdigest(),
                )
            pushed.append(rel.as_posix())
            logger.debug("Pushed %s -> %s", rel, dest)
        except Exception as exc:
            logger.warning("push_local: failed for %s: %s", rel, exc)

    return pushed
