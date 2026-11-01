# -*- coding: utf-8 -*-
"""AgentTeams coding-runs index writer (Q3: #1381 visibility).

The controller reads runs **from object storage**: one transcript plus one
``runs.jsonl`` line per run, under
``shared/tasks/{task-id}/workspace/coding-cli-logs/``. A local-only
projection is invisible to ``GET /api/v1/coding-runs``; this module writes
the same on-disk layout so a deployment-side ``mc mirror`` can surface it.

The index contract (``docs/design/coding-runs-api.md``):
one index line ⇔ one transcript; status = 0→ok / 124→timeout / 125→blocked /
else failed; empty values are omitted.
"""

from __future__ import annotations

import hashlib
import json
import os
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

#: Dispatch terminal state -> coding-runs status.
STATUS_MAP = {
    "done": "ok",
    "timeout": "timeout",
    "failed": "failed",
    "stale": "failed",
    "killed": "failed",
}


def parse_cli_version(text: str | None) -> str | None:
    """Extract a version from ``qwen --version`` output."""
    raw = str(text or "").strip()
    if not raw:
        return None
    for token in raw.split():
        candidate = token.lstrip("v")
        parts = candidate.split(".")
        if len(parts) >= 2 and all(p.isdigit() for p in parts[:2]):
            return candidate
    return raw.splitlines()[0][:40]


def _claim_transcript(log_dir: Path) -> Path:
    """Exclusive create ``run-YYYYMMDD-HHMMSS.log`` (step a second on collision)."""
    log_dir.mkdir(parents=True, exist_ok=True)
    stamp = time.time()
    for offset in range(120):
        moment = datetime.fromtimestamp(stamp + offset, tz=timezone.utc)
        path = log_dir / f"run-{moment.strftime('%Y%m%d-%H%M%S')}.log"
        try:
            fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o644)
        except FileExistsError:
            continue
        os.close(fd)
        return path
    raise RuntimeError("cannot claim a transcript name in coding-cli-logs/")


def write_run_record(
    task: dict[str, Any],
    log_text: str,
    log_dir: str | os.PathLike[str] | None = None,
) -> dict[str, Any] | None:
    """Write one transcript + one ``runs.jsonl`` line; return the record.

    No-op (returns ``None``) when no log dir is configured — worktrees must
    not be polluted with ``coding-cli-logs/`` unless this is an AgentTeams
    task workspace.
    """
    target = log_dir or task.get("runs_dir") or os.environ.get(
        "QWEN_CODE_BRIDGE_RUNS_DIR"
    )
    if not target:
        return None
    directory = Path(str(target)).expanduser()
    transcript = _claim_transcript(directory)
    payload = str(log_text or "").encode("utf-8", "replace")
    transcript.write_bytes(payload)

    state = str(task.get("state") or "")
    prompt = str(task.get("prompt") or "")
    record = {
        "v": 1,
        "run": transcript.name,
        "cli": task.get("cli"),
        "worker": task.get("worker"),
        "runner": task.get("runner") or None,
        "status": STATUS_MAP.get(state, "failed"),
        "reason": "stale" if state == "stale" else "",
        "exit_code": task.get("exit_code"),
        "timeout_secs": task.get("timeout"),
        "started_at": task.get("started_at"),
        "ended_at": task.get("finished_at"),
        "log_bytes": len(payload),
        "prompt_sha256": hashlib.sha256(prompt.encode("utf-8")).hexdigest()
        if prompt
        else None,
        "prompt_summary": prompt[:120] or None,
    }
    record = {k: v for k, v in record.items() if v not in (None, "")}
    with (directory / "runs.jsonl").open("a", encoding="utf-8") as fh:
        fh.write(json.dumps(record, ensure_ascii=False) + "\n")
    return {"run": transcript.name, "record": record, "dir": str(directory)}


__all__ = ["STATUS_MAP", "parse_cli_version", "write_run_record"]
