# -*- coding: utf-8 -*-
"""Worker-side approval mapping for headless coding-CLI runs.

The CLI (``qwen --input-format stream-json``) asks the host to decide
every would-ask tool call through the control protocol::

    {"type":"control_request","request_id":"...",
     "request":{"subtype":"can_use_tool","tool_name":"...",
                "tool_use_id":"...","input":{...},
                "permission_suggestions":null,"blocked_path":null}}

and waits for::

    {"type":"control_response",
     "response":{"subtype":"success","request_id":"...",
                 "response":{"behavior":"allow","updatedInput":{...}}}}
    # or {"behavior":"deny","message":"...","interrupt":false}

This module is the **deterministic policy** the worker uses to answer on
the human's behalf ("worker 代审批").  No LLM in the loop: the same
tool call always maps to the same decision, every decision is written to
a receipt, and anything unrecognised fails closed (deny).
"""

from __future__ import annotations

import json
import os
import re
from pathlib import Path
from typing import Any, Iterable

#: Risk classes.  ``allow`` = auto-approved, ``deny`` = auto-denied,
#: ``escalate`` = needs a human (the host denies by default and records
#: the escalation; a caller may override with ``escalate_behavior``).
RISK_ALLOW = "allow"
RISK_DENY = "deny"
RISK_ESCALATE = "escalate"

#: Tools the CLI uses for writes (edit family).
_WRITE_TOOLS = {
    "write_file",
    "edit",
    "multi_edit",
    "apply_patch",
    "notebook_edit",
}
_READ_TOOLS = {
    "read_file",
    "list_directory",
    "glob",
    "grep",
    "search_file_content",
    "read_many_files",
}
_EXEC_TOOLS = {"shell", "run_shell_command", "bash", "exec", "execute"}
_FETCH_TOOLS = {"web_fetch", "fetch", "web_search"}

#: Path-like keys in tool inputs.
_PATH_KEYS = ("file_path", "path", "target_file", "notebook_path", "cwd")

#: Secret-ish path fragments that must never be written or read out.
_SECRET_MARKERS = (
    ".env",
    "id_rsa",
    "id_ed25519",
    ".pem",
    ".key",
    "credentials",
    ".aws",
    ".ssh",
    ".gnupg",
    ".netrc",
    ".npmrc",
    ".pypirc",
)

#: Command patterns auto-denied regardless of path.
_DENY_COMMANDS = (
    r"\bgit\s+push\b",
    r"\bgit\s+reset\s+--hard\b",
    r"\bgit\s+clean\s+-[a-z]*f",
    r"\brm\s+(-[a-zA-Z]*\s+)*-?[rf]",
    r"\bsudo\b",
    r"\bsu\b",
    r"\bchmod\s+777\b",
    r"\bchown\b",
    r"\bdd\s+if=",
    r"\bmkfs\.",
    r"\b(shutdown|reboot|halt|poweroff)\b",
    r"\bkill\s+-9\s+1\b",
    r"\bcurl\b",
    r"\bwget\b",
    r"\bssh\b",
    r"\bscp\b",
    r"\bcurl\s+.*\|\s*(sh|bash)\b",
    r"\/etc\/",
    r"~\/\.ssh",
)

#: Commands auto-allowed inside the run root (tests / builds / VCS reads).
_ALLOW_COMMANDS = (
    r"^(pytest|python3?\s+-m\s+pytest)\b",
    r"^(npm|pnpm|yarn)\s+(test|run\s+(test|lint|build|typecheck))\b",
    r"^(go\s+(test|build|vet))\b",
    r"^(cargo\s+(test|build|check))\b",
    r"^(ruff|black\s+--check|mypy|eslint|prettier\s+--check)\b",
    r"^(git\s+(status|diff|log|show|rev-parse|branch|checkout\s+--|add|commit))\b",
    r"^(ls|pwd|cat|head|tail|wc|sort|uniq|grep|rg|find|sed\s+-n|awk)\b",
    r"^(bash\s+-n|sh\s+-n|node\s+--check)\b",
)


def _norm(text: Any) -> str:
    return str(text or "").strip()


def _roots(roots: Iterable[str] | None) -> list[str]:
    out: list[str] = []
    for raw in roots or ():
        try:
            out.append(os.path.realpath(str(raw)))
        except OSError:
            continue
    return out


def _inside(path: str, roots: list[str]) -> bool:
    try:
        real = os.path.realpath(path)
    except OSError:
        return False
    return any(real == root or real.startswith(root + os.sep) for root in roots)


def _looks_secret(path: str) -> bool:
    low = path.lower()
    return any(marker in low for marker in _SECRET_MARKERS)


def _paths_from_input(tool_input: Any) -> list[str]:
    if not isinstance(tool_input, dict):
        return []
    paths: list[str] = []
    for key in _PATH_KEYS:
        value = tool_input.get(key)
        if isinstance(value, str) and value:
            paths.append(value)
    for key in ("files", "file_paths", "paths"):
        value = tool_input.get(key)
        if isinstance(value, list):
            paths.extend(str(v) for v in value if isinstance(v, str) and v)
    return paths


def _command_from_input(tool_input: Any) -> str:
    if not isinstance(tool_input, dict):
        return ""
    for key in ("command", "cmd", "script"):
        value = tool_input.get(key)
        if isinstance(value, str):
            return value
    return ""


def classify_tool(
    tool_name: str,
    tool_input: Any,
    roots: Iterable[str] | None = None,
) -> dict[str, Any]:
    """Map one tool call to a risk decision (pure function).

    Returns ``{"risk","rule","reason","target"}``; never raises.
    """
    name = _norm(tool_name)
    root_list = _roots(roots)
    lowered = name.lower()

    if name.startswith("mcp__") or name.startswith("mcp."):
        return _result(RISK_ESCALATE, "mcp", "MCP tool call needs review", name)

    if lowered in _FETCH_TOOLS or "fetch" in lowered:
        return _result(RISK_DENY, "network", "network access is not allowed", name)

    if lowered in _EXEC_TOOLS or "shell" in lowered or "exec" in lowered:
        command = _command_from_input(tool_input).strip()
        if not command:
            return _result(RISK_ESCALATE, "exec-empty", "empty command", name)
        for pattern in _DENY_COMMANDS:
            if re.search(pattern, command):
                return _result(
                    RISK_DENY,
                    f"deny-command:{pattern}",
                    f"command matches forbidden pattern: {pattern}",
                    command[:200],
                )
        if root_list:
            for pattern in _ALLOW_COMMANDS:
                if re.search(pattern, command):
                    return _result(
                        RISK_ALLOW,
                        f"allow-command:{pattern}",
                        "known-safe command",
                        command[:200],
                    )
        # Unknown command with no allowlist hit: escalate (fail closed).
        return _result(
            RISK_ESCALATE,
            "exec-unknown",
            "command not in the auto-approve allowlist",
            command[:200],
        )

    if lowered in _WRITE_TOOLS or "write" in lowered or "edit" in lowered:
        paths = _paths_from_input(tool_input)
        if not paths:
            return _result(RISK_ESCALATE, "write-no-path", "write without a path", name)
        for path in paths:
            if _looks_secret(path):
                return _result(
                    RISK_DENY,
                    "secret-path",
                    "refuses to write credential-like files",
                    path,
                )
            if root_list and not _inside(path, root_list):
                return _result(
                    RISK_DENY,
                    "outside-root",
                    "path is outside the run root",
                    path,
                )
        return _result(RISK_ALLOW, "write-inside", "write inside the run root", paths[0])

    if lowered in _READ_TOOLS or "read" in lowered or "list" in lowered:
        paths = _paths_from_input(tool_input)
        for path in paths:
            if _looks_secret(path):
                return _result(
                    RISK_DENY,
                    "secret-path",
                    "refuses to read credential-like files",
                    path,
                )
        return _result(RISK_ALLOW, "read", "read-only tool", paths[0] if paths else name)

    return _result(
        RISK_ESCALATE,
        "unknown-tool",
        "tool is not in the policy table",
        name,
    )


def _result(risk: str, rule: str, reason: str, target: str) -> dict[str, Any]:
    return {"risk": risk, "rule": rule, "reason": reason, "target": target}


def decide(
    tool_name: str,
    tool_input: Any,
    roots: Iterable[str] | None = None,
    escalate_behavior: str = "deny",
) -> dict[str, Any]:
    """Turn a classification into a control-protocol permission result."""
    info = classify_tool(tool_name, tool_input, roots)
    risk = info["risk"]
    behavior = "allow" if risk == RISK_ALLOW else "deny"
    if risk == RISK_ESCALATE:
        behavior = "allow" if escalate_behavior == "allow" else "deny"
    return {
        **info,
        "behavior": behavior,
        "message": (
            f"worker-proxy approval: {info['rule']} — {info['reason']}"
            if behavior == "deny"
            else f"worker-proxy approval: {info['rule']}"
        ),
        "requires_human": risk == RISK_ESCALATE and behavior == "deny",
    }


def control_response(request_id: str, decision: dict[str, Any]) -> dict[str, Any]:
    """Build the stream-json ``control_response`` for one approval request."""
    if decision.get("behavior") == "allow":
        payload: dict[str, Any] = {"behavior": "allow"}
        updated_input = decision.get("updated_input")
        if isinstance(updated_input, dict) and updated_input:
            # AskUserQuestionTool consumes ``updatedInput.answers`` (answerKey
            # = question index as a string). Passing the human's answer here
            # is how the tool receives it in SDK/stream-json mode.
            payload["updatedInput"] = updated_input
    else:
        payload = {
            "behavior": "deny",
            "message": str(decision.get("message") or "denied"),
            "interrupt": False,
        }
    return {
        "type": "control_response",
        "response": {
            "subtype": "success",
            "request_id": request_id,
            "response": payload,
        },
    }


def receipt_entry(
    task_id: str,
    tool_name: str,
    tool_input: Any,
    decision: dict[str, Any],
    request_id: str,
) -> dict[str, Any]:
    """One append-only audit record for a proxy-approval decision."""
    return {
        "ts": __import__("datetime").datetime.utcnow().isoformat() + "Z",
        "task_id": task_id,
        "kind": "approval",
        "request_id": request_id,
        "tool_name": tool_name,
        "input": tool_input,
        "risk": decision.get("risk"),
        "rule": decision.get("rule"),
        "reason": decision.get("reason"),
        "behavior": decision.get("behavior"),
        "requires_human": bool(decision.get("requires_human")),
    }


def append_receipt(path: Path, entry: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as fh:
        fh.write(json.dumps(entry, ensure_ascii=False) + "\n")
