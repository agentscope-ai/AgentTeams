"""Drop ledger for the AgentTeams Matrix bundle (delivery-outcome trace).

The runtime adapters silently drop inbound events and outbound sends:
mention-gated group chatter, the NO_REPLY protocol, self-trigger
suppression, exhausted sends and missing Matrix configuration.  Without
a ledger those drops leave nothing behind but in-process log lines.
This module is the shared, runtime-neutral drop ledger every adapter
writes through — no adapter may invent its own reason strings,
:data:`DROP_REASONS` is the single vocabulary.

On-disk format: one JSON object per line, appended to
``<workspace>/shared/matrix/drops.jsonl``::

    {"ts": 1700000000.0, "reason": "not_mentioned",
     "room_id": "!room:hs", "event_id": "$abc", "detail": "..."}

``workspace`` is the runtime root that owns the ``shared/`` tree
(QwenPaw: the workspace directory; Hermes: the adapter state dir).
The file is append-only; rotation is deliberately out of scope here.

Write failures are warn-only and never raised: recording a drop must
not disturb the message path (same policy as event emission).

Vocabulary contract: the shared codes (``matrix_env_missing``,
``room_send_failed``) must stay verbatim identical to the TeamHarness
side's ``NOTIFICATION_GAP_REASONS``; both test suites assert their
shared subset.  ``allowlist`` is a *reporting entry only*: the
allowlist interception happens in the QwenPaw runtime (agent.json),
not in this bundle, so the runtime must call
``record_drop(reason="allowlist", ...)`` itself — without that call
the code stays empty.
"""

from __future__ import annotations

import json
import logging
import time
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional, Union

logger = logging.getLogger("agentteams.matrix.outcomes")

#: Frozen delivery-outcome vocabulary.  Adapters import this instead of
#: inventing their own strings; the TeamHarness side mirrors the shared
#: codes (``matrix_env_missing``, ``room_send_failed``) in its
#: ``NOTIFICATION_GAP_REASONS`` constant and both suites assert the
#: shared subset verbatim.
DROP_REASONS = (
    "not_mentioned",
    "no_reply",
    "ping_pong",
    "room_send_failed",
    "matrix_env_missing",
    "allowlist",
)

#: Ledger location relative to the workspace root.
DROPS_RELATIVE_PATH = Path("shared") / "matrix" / "drops.jsonl"

#: ``detail`` is capped so a single line stays small.
DETAIL_LIMIT = 200

DEFAULT_LIMIT = 200


def drops_path(workspace: Union[str, Path, None]) -> Path:
    """Ledger file for *workspace*: ``<workspace>/shared/matrix/drops.jsonl``.

    A missing *workspace* falls back to the current working directory;
    adapters always pass their own root explicitly.
    """
    root = Path(workspace).expanduser() if workspace else Path.cwd()
    return root / DROPS_RELATIVE_PATH


def _parse_since(
    since: Union[str, int, float, None],
) -> Optional[float]:
    """Parse a *since* filter into a unix timestamp (or None = no filter)."""
    if since is None or since == "":
        return None
    if isinstance(since, (int, float)):
        return float(since)
    text = str(since).strip()
    try:
        return float(text)
    except ValueError:
        pass
    try:
        return datetime.fromisoformat(text).timestamp()
    except ValueError:
        logger.warning(
            "read_drops: unparseable since=%r; ignoring filter", since
        )
        return None


def record_drop(
    workspace: Union[str, Path, None],
    *,
    reason: str,
    room_id: str,
    event_id: str = "",
    detail: str = "",
) -> None:
    """Append one drop record to the workspace ledger.

    Warn-only on any failure (unwritable directory, JSON error, ...):
    the message path must continue.  Unknown *reason* values are
    recorded as well (data must not be lost) but warn, so vocabulary
    drift stays visible.
    """
    if reason not in DROP_REASONS:
        logger.warning(
            "record_drop: unknown reason %r (vocabulary: %s)",
            reason,
            ", ".join(DROP_REASONS),
        )
    record = {
        "ts": time.time(),
        "reason": reason,
        "room_id": str(room_id or ""),
        "event_id": str(event_id or ""),
        "detail": str(detail or "")[:DETAIL_LIMIT],
    }
    path = drops_path(workspace)
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("a", encoding="utf-8") as fh:
            fh.write(json.dumps(record, ensure_ascii=False) + "\n")
    except Exception as exc:  # noqa: BLE001 - warn-only by contract
        logger.warning("record_drop: could not append to %s: %s", path, exc)


def read_drops(
    workspace: Union[str, Path, None],
    *,
    since: Union[str, int, float, ""] = "",
    limit: int = DEFAULT_LIMIT,
) -> List[Dict[str, Any]]:
    """Read up to the last *limit* ledger records (oldest first).

    *since* accepts a unix timestamp (number or numeric string) or an
    ISO-8601 string; records with ``ts`` before it are filtered out.
    Malformed lines are skipped.  A missing or unreadable ledger yields
    an empty list (warn-only).
    """
    path = drops_path(workspace)
    if not path.is_file():
        return []
    rows: List[Dict[str, Any]] = []
    try:
        with path.open("r", encoding="utf-8") as fh:
            for raw in fh:
                raw = raw.strip()
                if not raw:
                    continue
                try:
                    row = json.loads(raw)
                except json.JSONDecodeError:
                    continue
                if isinstance(row, dict):
                    rows.append(row)
    except Exception as exc:  # noqa: BLE001 - warn-only by contract
        logger.warning("read_drops: could not read %s: %s", path, exc)
        return []
    cutoff = _parse_since(since)
    if cutoff is not None:
        rows = [row for row in rows if float(row.get("ts") or 0) >= cutoff]
    limit = max(0, int(limit))
    return rows[-limit:] if limit else []
