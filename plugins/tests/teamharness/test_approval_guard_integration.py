# -*- coding: utf-8 -*-
"""Cross-module regression: TH-20 writes the trust markers, the main
protocol guard reads them — one contract, two sides.

After the th-batch-b merge, ``lifecycle/approval.py`` (the TH-20
approval lifecycle) and ``lifecycle/review.py`` (the #1467 protocol
guard) are two modules that must agree on one marker vocabulary:

- a human-channel resolve (``im_reply`` / ``im_command`` /
  ``dashboard``) executed by a human runtime
  (``AGENTTEAMS_AGENT_ROLE=human``) writes the trust markers
  ``resolvedRole="human"`` + ``resolvedActor="human:<account>"`` on the
  attention record — the account comes from the trusted runtime
  (``AGENTTEAMS_WORKER_NAME``), never from the payload's decided_by;

- the guard's ``_has_approval_record`` accepts ONLY a resolved
  approval record carrying exactly that server-written marker set
  (resolvedRole "human", a human close channel, and a server-written
  decider whose account differs from the submitter's).

These three tests pin the marker side of the contract end to end —
one resolve that MUST satisfy the guard (human runtime + human
channel) and two that MUST NOT (a human runtime through the tool
channel, and a non-human runtime through a human channel). A
regression on either side — markers written too eagerly, or the guard
too strict/too permissive about them — fails here before it can
distort the delegate acceptance path.
"""

from __future__ import annotations

import sys
from pathlib import Path

MCP_DIR = Path(__file__).resolve().parents[2] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402

approval = server.approval
review = server.review

T0 = "2026-11-04T10:00:00Z"
T_DECIDE = "2026-11-04T10:05:00Z"  # well inside the 30-minute default bound

# Display provenance only (a raw MXID spelling with a leading space —
# the guard must never read resolvedBy, only the server-written
# resolvedActor marker).
DECIDER_PAYLOAD = " @admin:example.test"

TASK_ID = "t-guard"


def _ask_and_resolve(
    tmp_path: Path, monkeypatch, *, role: str, via: str
) -> tuple[dict, dict]:
    """Ask an approval, then resolve it from the given trusted-runtime
    role/channel; return (task meta, the attention record)."""
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", role)
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "luo")
    task = {"task_id": TASK_ID, "status": "in_progress"}
    asked = approval.ask_approval(
        task, tmp_path, TASK_ID, question="may I delegate this task?", now=T0
    )
    approval.resolve_approval(
        task, tmp_path, TASK_ID, asked["call_id"], "approve",
        decided_by=DECIDER_PAYLOAD, via=via, now=T_DECIDE,
    )
    return task, task["attention"][-1]


def test_human_channel_resolve_satisfies_the_guard(tmp_path, monkeypatch):
    """The contract's positive case: a human runtime closing through a
    human channel writes the markers, and the guard honors them."""
    task, record = _ask_and_resolve(tmp_path, monkeypatch, role="human", via="im_reply")
    # TH-20 writes the markers from the trusted runtime identity only
    # (AGENTTEAMS_WORKER_NAME=luo — never the payload's decided_by):
    assert record["resolvedRole"] == "human"
    assert record["resolvedActor"] == "human:luo"
    # The main guard accepts the record: the decider (luo) is a human
    # channel decider who is NOT the submitter (alice) — no
    # self-approval.
    assert review._has_approval_record(task, "alice") is True


def test_tool_channel_resolve_is_not_an_approval(tmp_path, monkeypatch):
    """A human runtime is not enough: the same decision closed through
    the tool channel writes no markers and the guard rejects it."""
    task, record = _ask_and_resolve(tmp_path, monkeypatch, role="human", via="tool")
    assert "resolvedRole" not in record
    assert "resolvedActor" not in record
    assert review._has_approval_record(task, "alice") is False


def test_non_human_runtime_role_writes_no_marker(tmp_path, monkeypatch):
    """A human channel is not enough either: a non-human runtime
    closing through im_reply writes no markers and the guard rejects
    it (fail closed — no verified human, no approval)."""
    task, record = _ask_and_resolve(tmp_path, monkeypatch, role="worker", via="im_reply")
    assert "resolvedRole" not in record
    assert "resolvedActor" not in record
    assert review._has_approval_record(task, "alice") is False
