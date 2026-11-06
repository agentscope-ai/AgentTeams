# status/transition constants shared by all fragments (W1-a2: real module)
from __future__ import annotations

TOOL_NAMES = ["health", "message", "roomflow", "filesync", "artifact", "projectflow", "taskflow"]


MESSAGE_TOOL_BLOCKED_ROLES = {"worker", "remote-member"}


ALLOWED_TASK_RESULT_STATUSES = {
    "SUCCESS",
    "SUCCESS_WITH_NOTES",
    "REVISION_NEEDED",
    "BLOCKED",
    "INTERRUPTED",
}


TERMINAL_TASK_STATUSES = {"completed", "revision", "blocked", "cancelled"}


# Task transition table — single source of truth:
# plugins/teamharness/contracts/task-transitions.json (cross-language golden
# fixture; the MCP contract test asserts this constant equals the file).
# "prepared" never appears on project nodes; it only exists in task meta.
TRANSITIONS: dict[str, list[str]] = {
    "planned": ["prepared", "assigned", "in_progress", "submitted", "cancelled"],
    "prepared": ["assigned", "cancelled"],
    "assigned": ["in_progress", "submitted", "cancelled"],
    "in_progress": ["submitted", "cancelled"],
    "submitted": ["completed", "revision", "blocked", "cancelled"],
}


TASK_HISTORY_LIMIT = 50

# W2 protocol guard (issue #1451 W2): decision-class vocabulary and the
# decision_status sub-state. The nine runtime states above are untouched;
# decision_status overlays them as a protocol-layer marker. The
# vocabulary is mirrored in contracts/decision-classes.json and
# contracts/attention.json (test_protocol_guard.py asserts the two agree).
DECISION_CLASS_IDS = ("r1", "r2", "r3", "r4", "r5", "r6")
DEFAULT_DECISION_CLASS = "r4"

DECISION_STATUS_VALUES = ("open", "blocked", "frozen", "paused", "spec_conflict")
DEFAULT_DECISION_STATUS = "open"

# Attention stall chain (contracts/attention.json): re-requests of an
# unanswered attention record bump its stall_count; the task's
# decision_status escalates when the count reaches these thresholds.
ATTENTION_STALL_BLOCKED = 2
ATTENTION_STALL_FROZEN = 3

# TeamHarness loop budget ledger (project["loop"]["budget"]): the
# tier->iteration-budget mapping (simple=3 / standard=5 / full=10). The
# cost-model tiers only define human-touch budgets, so the mapping is
# declared here. A loop with neither a recognized tier nor an explicit
# iteration_budget has no budget at all (iteration_budget stays None).
LOOP_BUDGET_BY_TIER: dict[str, int] = {"simple": 3, "standard": 5, "full": 10}
# Per-loop provenance tail kept in meta.json (the newest entries survive).
LOOP_PROVENANCE_LIMIT = 5


