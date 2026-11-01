# json/atomic-file helpers and timestamps (W1-a2: real module)
from __future__ import annotations

import datetime
import json
import os
import tempfile
import time
from pathlib import Path
from typing import Any

from . import refs


def _project_timestamp() -> str:
    return time.strftime("%Y%m%d-%H%M%S")


def _read_json(path: Path, default: dict[str, Any] | None = None) -> dict[str, Any]:
    if not path.exists():
        return dict(default or {})
    return json.loads(path.read_text(encoding="utf-8"))


def _atomic_write_text(path: Path, text: str) -> None:
    """Replace one state projection without exposing a partially written file."""
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temporary_path = Path(temporary_name)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as handle:
            handle.write(text)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_path, path)
    except BaseException:
        temporary_path.unlink(missing_ok=True)
        raise


def _write_json(path: Path, data: dict[str, Any]) -> None:
    _atomic_write_text(path, json.dumps(data, ensure_ascii=False, indent=2, sort_keys=True) + "\n")


refs.define("_write_json", _write_json)


def _utc_timestamp() -> str:
    return (
        datetime.datetime.now(datetime.timezone.utc)
        .replace(microsecond=0)
        .isoformat()
        .replace("+00:00", "Z")
    )


def _safe_read_bytes(path: Path) -> bytes:
    try:
        return path.read_bytes()
    except OSError:
        return b""


