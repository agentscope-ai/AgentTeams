# policyEffects: governance change impact-surface computation (TH-19)
from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from mcp.common.constants import TERMINAL_TASK_STATUSES
from mcp.common.constants import TOOL_NAMES as _ALL_TOOLS

# Governance files of one agent workspace: a change to any of them can
# change what the owning runtime is allowed or expected to do, so it
# carries a policy version (policy_fingerprint) and an impact surface
# (classify_change).
GOVERNANCE_FILES = ("SOUL.md", "AGENTS.md", "allowlist.json", "settings.json")

# Team runtime roles served by the teamharness plugin (same vocabulary as
# the taskflow role checks and MESSAGE_TOOL_BLOCKED_ROLES).
TEAM_RUNTIME_ROLES = ("leader", "worker", "remote-member")

# Full MCP tool surface (single source of truth: common.constants).
TOOL_NAMES = tuple(_ALL_TOOLS)

# Path segments that name a runtime role (per-agent directories such as
# `worker/SOUL.md`, prompt groups such as `prompts/leader/`).
_ROLE_BY_SEGMENT = {
    "leader": "leader",
    "worker": "worker",
    "remote-member": "remote-member",
    "remoteMember": "remote-member",
    "manager": "manager",
}

# Cross-language contract files every team runtime reads.
_CONTRACT_FILES = ("signal-mapping.json", "task-transitions.json", "decision-classes.json")

# The runtime role each in-flight task status is waiting on: an
# assigned/in_progress task blocks on its worker; a submitted task blocks
# on the leader's decision. planned/prepared are not in flight yet;
# terminal statuses are filtered out separately.
_IN_FLIGHT_ROLE_BY_STATUS = {
    "assigned": "worker",
    "in_progress": "worker",
    "submitted": "leader",
}


def policy_fingerprint(root: Path | str) -> int:
    """Version the workspace governance state (the TH-21 policy version).

    Deterministic and clock-free: sha256 over the sorted
    ``name=absent`` / ``name=<sha256(bytes)>`` parts of GOVERNANCE_FILES
    under ``root``. An unchanged workspace always fingerprints equal; any
    add / remove / modify of a governance file moves the version. 0 when
    no governance file exists (the pre-fence baseline — records dispatched
    before the fence read as version 0 and are never rejected).
    """
    base = Path(root)
    parts: list[str] = []
    present = 0
    for name in sorted(GOVERNANCE_FILES):
        path = base / name
        try:
            data = path.read_bytes() if path.is_file() else None
        except OSError:
            data = None
        if data is None:
            parts.append(f"{name}=absent")
        else:
            present += 1
            parts.append(f"{name}={hashlib.sha256(data).hexdigest()}")
    if present == 0:
        return 0
    digest = hashlib.sha256("\n".join(parts).encode("utf-8")).hexdigest()
    return int(digest[:16], 16)


def _governance_runtimes(parent_segments: list[str]) -> tuple[str, ...]:
    roles = tuple(
        dict.fromkeys(_ROLE_BY_SEGMENT[s] for s in parent_segments if s in _ROLE_BY_SEGMENT)
    )
    # No role segment: the owning runtime is not identifiable from the
    # path, so over-report (all team roles) rather than under-report.
    return roles if roles else TEAM_RUNTIME_ROLES


def _plugin_code_tools(rest: str) -> tuple[str, ...]:
    if rest.startswith("lifecycle/"):
        return ("taskflow",)
    if rest.startswith("project/"):
        return ("projectflow",)
    if rest.startswith("room/"):
        return ("roomflow",)
    if rest.startswith("artifact/"):
        return ("artifact", "filesync")
    # Shared assembly (server.py, common/, transport/...): the whole
    # tool surface can move.
    return TOOL_NAMES


def classify_change(path: str) -> dict[str, Any] | None:
    """Classify one changed path by the runtime impact it carries.

    Returns ``{"area", "runtimes", "tools"}`` (runtimes/tools as tuples)
    or ``None``. ``None`` means the path carries no runtime impact
    (docs, tests, CI, templates): it must not contribute any affected
    runtime, tool, or in-flight task. Paths may be repo-relative or
    workspace-relative; both plugin tree names are accepted.
    """
    p = str(path or "").strip().replace("\\", "/").lstrip("/")
    if not p:
        return None
    segments = p.split("/")
    name = segments[-1]

    # Governance files of one agent workspace (SOUL / AGENTS / allowlist /
    # settings): the allowlist gates tool availability, so it changes the
    # whole tool surface of its runtime.
    if name in GOVERNANCE_FILES:
        tools = TOOL_NAMES if name == "allowlist.json" else ()
        return {
            "area": "governance",
            "runtimes": _governance_runtimes(segments[:-1]),
            "tools": tools,
        }

    # Cross-language contracts: every team runtime reads them.
    if (
        segments[0] == "plugins"
        and len(segments) >= 4
        and segments[2] == "contracts"
        and name in _CONTRACT_FILES
    ):
        return {
            "area": "contract",
            "runtimes": TEAM_RUNTIME_ROLES,
            "tools": ("taskflow", "projectflow"),
        }

    # Plugin code: the tool surface of the module group that changed.
    if segments[0] == "plugins" and len(segments) >= 4 and segments[2] == "mcp":
        return {
            "area": "plugin-code",
            "runtimes": TEAM_RUNTIME_ROLES,
            "tools": _plugin_code_tools("/".join(segments[3:])),
        }

    # Role prompts: only the role the prompt drives.
    if segments[0] == "plugins" and len(segments) >= 5 and segments[2] == "prompts":
        role = _ROLE_BY_SEGMENT.get(segments[3])
        if role in TEAM_RUNTIME_ROLES:
            return {"area": "prompt", "runtimes": (role,), "tools": ()}

    return None


def _as_path(workspace: Path | str | dict[str, Any] | None) -> Path | None:
    if workspace is None:
        return None
    if isinstance(workspace, dict):
        value = str(workspace.get("workspaceDir") or "").strip()
    else:
        value = str(workspace).strip()
    return Path(value).expanduser() if value else None


def _in_flight_tasks(workspace: Path, runtimes: frozenset[str]) -> list[dict[str, Any]]:
    tasks_root = workspace / "shared" / "tasks"
    found: list[dict[str, Any]] = []
    if not tasks_root.is_dir() or not runtimes:
        return found
    for meta_path in sorted(tasks_root.glob("*/meta.json")):
        try:
            task = json.loads(meta_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            continue
        if not isinstance(task, dict):
            continue
        status = str(task.get("status") or "")
        if status in TERMINAL_TASK_STATUSES:
            continue
        waiting_on = _IN_FLIGHT_ROLE_BY_STATUS.get(status)
        if waiting_on is None or waiting_on not in runtimes:
            continue
        found.append(
            {
                "task_id": str(task.get("task_id") or meta_path.parent.name),
                "status": status,
                "waiting_on": waiting_on,
            }
        )
    return found


def compute_policy_effects(
    changes: list[str] | list[dict[str, Any]],
    workspace: Path | str | dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Compute the affected surface of a governance change set (TH-19).

    ``changes`` is the changed-path list — strings, or dicts with a
    ``path`` key (``git diff --name-only`` shape either way).
    ``workspace`` (a path or a taskflow-style arguments dict carrying
    ``workspaceDir``) enables the in-flight task scan; omit it for the
    PR-template use (change set only, no in-flight listing).

    Returns ``{"areas", "affected_runtimes", "tool_surface",
    "in_flight_tasks", "summary"}``. All lists are empty when the change
    set carries no runtime impact (docs/tests/CI-only): an empty list,
    not a guess.
    """
    runtimes: set[str] = set()
    tools: set[str] = set()
    areas: set[str] = set()
    for change in changes or []:
        path = str(change.get("path") or "") if isinstance(change, dict) else str(change or "")
        classified = classify_change(path)
        if classified is None:
            continue
        areas.add(classified["area"])
        runtimes.update(classified["runtimes"])
        tools.update(classified["tools"])
    affected = sorted(runtimes)
    surface = sorted(tools)
    in_flight: list[dict[str, Any]] = []
    if affected:
        root = _as_path(workspace)
        if root is not None:
            in_flight = _in_flight_tasks(root, frozenset(affected))
    effects: dict[str, Any] = {
        "areas": sorted(areas),
        "affected_runtimes": affected,
        "tool_surface": surface,
        "in_flight_tasks": in_flight,
    }
    effects["summary"] = format_policy_effects(effects)
    return effects


def format_policy_effects(effects: dict[str, Any]) -> str:
    """The human/leader-readable list for the change-PR policyEffects section."""
    if not (
        effects.get("affected_runtimes") or effects.get("tool_surface") or effects.get("in_flight_tasks")
    ):
        return "no impact (无影响): no runtime, tool surface, or in-flight task is affected"
    lines = ["policyEffects 影响面:"]
    lines.append(f"- runtimes: {', '.join(effects.get('affected_runtimes') or [])}")
    lines.append(f"- tool surface: {', '.join(effects.get('tool_surface') or []) or '—'}")
    tasks = effects.get("in_flight_tasks") or []
    if tasks:
        lines.append(f"- in-flight tasks: {len(tasks)}")
        for item in tasks:
            lines.append(
                f"  - {item['task_id']} ({item['status']}, waiting on {item['waiting_on']})"
            )
    else:
        lines.append("- in-flight tasks: none (无在跑任务)")
    return "\n".join(lines)
