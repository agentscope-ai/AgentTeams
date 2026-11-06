#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""PreToolUse command hook: enforce the worker-proxy approval policy.

Installed by :func:`qwen_code_bridge.state.prepare_cli_home` into the
dispatch CLI home (``settings.json`` → ``hooks.PreToolUse``, matcher
``*``).  The CLI runs it before **every** tool call — including the
read-only calls it would otherwise auto-approve without a
``can_use_tool`` control request — so one deterministic policy
(:mod:`qwen_code_bridge.approval`) covers the whole tool surface.

Protocol (Qwen Code command hook):

- stdin: one JSON object — at least ``tool_name`` and ``tool_input``,
  plus the session ``cwd``.
- stdout: one JSON object with ``hookSpecificOutput.permissionDecision``
  (``allow`` / ``deny``); exit code 0.

Fail closed: a missing/empty ``AGENTTEAMS_APPROVAL_ROOTS`` (JSON array
of run roots), malformed hook input, or any internal error → deny.  The
reason names the failure class, never the input content.

The run roots arrive through the environment because they are per-task
(the dispatch sets ``AGENTTEAMS_APPROVAL_ROOTS`` for every spawned CLI —
the same list the ``can_use_tool`` path passes to ``approval.decide``).
The settings-level hook entry is static and shared by concurrent tasks,
so it must not carry roots.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

# Standalone entry point (``<sys.executable> .../hook_policy.py``): make
# the package importable without an install step.
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

try:
    from qwen_code_bridge.state import APPROVAL_ROOTS_ENV as ROOTS_ENV
except Exception:  # pragma: no cover - package import failure must not
    ROOTS_ENV = "AGENTTEAMS_APPROVAL_ROOTS"  # fail open: literal fallback

HOOK_EVENT = "PreToolUse"


def _output(decision: str, rule: str, reason: str) -> dict:
    return {
        "continue": True,
        "decision": decision,
        "hookSpecificOutput": {
            "hookEventName": HOOK_EVENT,
            "permissionDecision": decision,
            "permissionDecisionReason": (
                f"worker-proxy approval: {rule} — {reason}"
            ),
        },
    }


def _load_roots() -> list:
    """Run roots from ``AGENTTEAMS_APPROVAL_ROOTS`` (JSON array).

    Empty means *missing* — the caller fails closed; it must never fall
    back to "no root check" (without roots the policy auto-approves
    writes and reads alike).
    """
    raw = os.environ.get(ROOTS_ENV, "").strip()
    if not raw:
        return []
    roots = json.loads(raw)
    if not isinstance(roots, list):
        raise ValueError("roots is not a JSON array")
    return [str(r) for r in roots if isinstance(r, (str, int)) and str(r)]


def _tool_input_for_policy(payload: dict) -> dict:
    """Copy of the hook ``tool_input`` with the effective cwd surfaced.

    The shell policy checks ``tool_input["cwd"]``; shell tool calls
    report the working directory under ``directory``.  Surface the
    effective cwd (never overriding an explicit tool-level value) so the
    cwd boundary check applies to every tool.
    """
    tool_input = payload.get("tool_input")
    ti = dict(tool_input) if isinstance(tool_input, dict) else {}
    session_cwd = payload.get("cwd")
    if isinstance(session_cwd, str) and session_cwd and not ti.get("cwd"):
        ti["cwd"] = ti.get("directory") or session_cwd
    return ti


def _write_receipt(payload: dict, decision: dict) -> None:
    """Append one ``source=hook`` receipt line (best-effort, never raises).

    The dispatch spawn environment carries ``QWEN_CODE_BRIDGE_TASK_ID``
    and ``QWEN_CODE_BRIDGE_RECEIPTS`` (the same receipts file the
    ``can_use_tool`` path writes), so a hook denial — which the host never
    sees as a control request — still leaves an audit line and can wake
    the origin.  Missing env (e.g. the hook ran outside a dispatch) or
    any write failure is silently dropped: the decision stands either way.
    """
    task_id = str(os.environ.get("QWEN_CODE_BRIDGE_TASK_ID", "") or "").strip()
    receipts = str(
        os.environ.get("QWEN_CODE_BRIDGE_RECEIPTS", "") or ""
    ).strip()
    if not task_id or not receipts:
        return
    try:
        from qwen_code_bridge import approval

        tool_input = (
            payload.get("tool_input") if isinstance(payload, dict) else None
        )
        approval.append_receipt(
            Path(receipts),
            approval.receipt_entry(
                task_id,
                str(payload.get("tool_name") or ""),
                tool_input,
                decision,
                "",
                source="hook",
            ),
        )
    except Exception:
        pass


def evaluate(payload: dict) -> dict:
    """One hook call → one PreToolUse output.  Raises on faults."""
    from qwen_code_bridge import approval
    from qwen_code_bridge.interaction import INTERACTION_TOOLS

    roots = _load_roots()
    if not roots:
        reason = f"{ROOTS_ENV} missing or empty (fail closed)"
        _write_receipt(
            payload,
            {
                "risk": "deny",
                "rule": "hook-config",
                "reason": reason,
                "behavior": "deny",
                "requires_human": False,
            },
        )
        return _output("deny", "hook-config", reason)
    tool_name = str(payload.get("tool_name") or "")
    if tool_name in INTERACTION_TOOLS:
        return _output("ask", "human-interaction", "defer to host interaction")
    decision = approval.decide(
        tool_name,
        _tool_input_for_policy(payload),
        roots=roots,
    )
    if decision.get("behavior") == "allow":
        return _output(
            "allow",
            str(decision.get("rule") or "policy"),
            str(decision.get("reason") or "allowed"),
        )
    _write_receipt(payload, decision)
    return _output(
        "deny",
        str(decision.get("rule") or "policy"),
        str(decision.get("reason") or "denied by policy"),
    )


def main() -> int:
    try:
        raw = sys.stdin.read()
        payload = json.loads(raw) if raw.strip() else {}
        if not isinstance(payload, dict):
            raise ValueError("hook input is not a JSON object")
        output = evaluate(payload)
    except Exception as exc:  # fail closed — never echo the input
        output = _output(
            "deny", "hook-fault", f"hook fault: {type(exc).__name__}"
        )
    sys.stdout.write(json.dumps(output, ensure_ascii=False) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
