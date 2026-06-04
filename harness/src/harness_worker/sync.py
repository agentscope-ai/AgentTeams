"""Thin re-export of hiclaw_common.sync for harness-worker.

The canonical implementation lives in hiclaw_common.sync.  FileSync is
configured for the harness runtime by passing runtime_home_dir=".harness"
(which is the default, so callers need no changes).
"""
from hiclaw_common.sync import (  # noqa: F401
    FileSync,
    _deep_merge,
    _mc,
    _merge_openclaw_config,
    push_loop,
    sync_loop,
)

# Legacy module-level function kept for backward compatibility.
# Prefer FileSync.push_local() directly.
def push_local(sync: FileSync, since: float = 0) -> list[str]:
    return sync.push_local(since)
