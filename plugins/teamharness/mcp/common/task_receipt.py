"""Validate public Task receipts and render facts for native tool context."""
from __future__ import annotations

import json
import re
import shlex

_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")


def task_submit_command(command: str) -> str | None:
    # Only a single CLI operation has an unambiguous result-to-call binding.
    if not isinstance(command, str) or any(c in command for c in ";&|<>`$()\n\r"):
        return None
    try:
        words = shlex.split(command)
    except ValueError:
        return None
    if (len(words) < 4 or words[0] not in {"agt", "/usr/local/bin/agt"}
            or words[1:3] != ["task", "submit"] or not _ID.fullmatch(words[3])):
        return None
    return words[3]


def submission_receipt(result: object, task_id: str) -> tuple[str, str] | None:
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
    submission = task.get("submissionId")
    if (task.get("id") != task_id or task.get("status") != "submitted"
            or not isinstance(submission, str) or not submission):
        return None
    status = notification.get("status")
    recipient, event = notification.get("recipient", ""), notification.get("eventId", "")
    if status == "queued":
        if not isinstance(recipient, str) or not _ID.fullmatch(recipient) or not isinstance(event, str) or not event:
            return None
        fact = f'Task system has queued an automatic notification to "{recipient}" for review. Delivery and acceptance are not yet confirmed.'
    elif status == "pending":
        fact = "Task system has taken responsibility for automatically notifying your Team Leader; notification is still pending."
    else:
        return None
    identity = json.dumps([task_id, submission, status, event, recipient], separators=(",", ":"))
    return identity, (f"[Task system receipt]\nTask {task_id} result is submitted for review. "
                      + fact + " Do not resend the notification; wait for acceptance or revision.")
