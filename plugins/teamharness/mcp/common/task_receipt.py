"""Validate public Task receipts and render facts for native tool context."""
from __future__ import annotations

import json
import re
import shlex

_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")


def task_operation_command(command: str) -> tuple[str, str] | None:
    # Only a single CLI operation has an unambiguous result-to-call binding.
    if not isinstance(command, str):
        return None
    try:
        lexer = shlex.shlex(command, posix=True, punctuation_chars=";&|<>()")
        lexer.whitespace_split = True
        words = list(lexer)
    except ValueError:
        return None
    if (len(words) < 4 or words[0] not in {"agt", "/usr/local/bin/agt"}
            or (words[1] != "task" or words[2] not in {"submit", "complete"}) or not _ID.fullmatch(words[3])):
        return None
    flags = {"--summary", "--report", "--status", "--file", "--team"} if words[2] == "submit" else {"--report", "--team", "--artifact"}
    tail = words[4:]
    if len(tail) % 2 or any(tail[i] not in flags for i in range(0, len(tail), 2)):
        return None
    if any(token and all(c in ";&|<>()" for c in token) for token in tail):
        return None
    return words[2], words[3]


def task_receipt(result: object, task_id: str, action: str) -> tuple[str, str] | None:
    """Return a deduplication identity and code-generated fact, or no receipt.

    Callers must supply a successful final result of the authenticated Task tool,
    bound to its invocation. Arbitrary shell output or streamed chunks are not
    authoritative merely because they contain similarly shaped JSON.
    """
    if not isinstance(result, dict) or result.get("ok") is False:
        return None
    task, notification = result.get("task"), result.get("notification")
    if not isinstance(task, dict) or not isinstance(notification, dict):
        return None
    if action not in {"submit", "complete"}:
        return None
    operation = task.get("submissionId" if action == "submit" else "completionId")
    if (task.get("id") != task_id or task.get("status") != ("submitted" if action == "submit" else "completed")
            or not isinstance(operation, str) or not operation):
        return None
    status = notification.get("status")
    recipient, event = notification.get("recipient", ""), notification.get("eventId", "")
    if status == "queued":
        if not isinstance(recipient, str) or not _ID.fullmatch(recipient) or not isinstance(event, str) or not event:
            return None
        fact = (f'Task system has queued an automatic notification to "{recipient}" for review. Delivery and acceptance are not yet confirmed.'
                if action == "submit" else f'Task system has queued the saved final report for return to the original conversation through "{recipient}". Delivery is not yet confirmed.')
    elif status == "pending":
        fact = ("Task system has taken responsibility for automatically notifying your Team Leader; notification is still pending."
                if action == "submit" else "Task system has taken responsibility for returning the saved final report to the original conversation; delivery is still pending.")
    else:
        return None
    identity = json.dumps([action, task_id, operation, status, event, recipient], separators=(",", ":"))
    state = "result is submitted for review" if action == "submit" else "is completed"
    next_action = ("Do not resend the notification; wait for acceptance or revision." if action == "submit"
                   else "Do not send another completion message.")
    return identity, f"[Task system receipt]\nTask {task_id} {state}. {fact} {next_action}"
