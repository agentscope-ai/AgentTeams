"""Authenticated Task notifications shared by the managed Matrix Harnesses.

The Controller owns routing and accepted state. A Matrix body is only a
projection: use the authenticated receipt's input, never the untrusted body.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.parse
import urllib.request


class IgnoredNotification(ValueError):
    """A malformed, stale or misrouted control packet must not start a turn."""


class NotificationUnavailable(RuntimeError):
    """The receipt could not be verified yet. Keep the source event pending."""


def controller_json(path):
    token = Path(os.environ["AGENTTEAMS_AUTH_TOKEN_FILE"]).read_text().strip()
    base = os.environ["AGENTTEAMS_CONTROLLER_URL"].rstrip("/")
    request = urllib.request.Request(base + "/api/v1/" + path,
        headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(request, timeout=5) as response:
        return json.load(response)


def notification(content, room, sender, own, *, fetch=controller_json, event_id=None):
    """Return canonical input/session, None for chat; reject invalid controls.

    Call before own-echo and peer-message suppression. Only a receipt verified
    for this exact recipient can admit a self-addressed closure notification.
    Normal channel sender/membership policies still apply to ordinary chat.
    """
    if content.get("org.agentteams.progress_only") is True:
        raise IgnoredNotification("progress is not a control notification")
    if "org.agentteams.coordination" not in content:
        return None
    def verified_get(path):
        try:
            return fetch(path)
        except urllib.error.HTTPError as exc:
            if exc.code in {403, 404}:
                raise IgnoredNotification("receipt absent or outside recipient scope") from exc
            raise NotificationUnavailable("Controller lookup failed") from exc
        except (OSError, json.JSONDecodeError) as exc:
            raise NotificationUnavailable("Controller lookup unavailable") from exc

    try:
        wire = content["org.agentteams.coordination"]
        meta = wire["channel_meta"]
        event = meta["teamharness_coordination"]
        identity = event["id"]
        if not isinstance(identity, str) or len(identity) != 32 or any(c not in "0123456789abcdef" for c in identity):
            raise IgnoredNotification("invalid receipt identity")
        receipt = verified_get("coordination/events/" + identity)
        if event_id is not None:
            if not receipt.get("native_id"):
                raise NotificationUnavailable("native delivery receipt is not committed yet")
            if receipt["native_id"] != "matrix:" + event_id:
                raise IgnoredNotification("native notification attempt was superseded")
        packet = receipt["control"]
        canonical = packet["channel_meta"]["teamharness_coordination"]
        # Completed transport means Matrix accepted the notification; native
        # consumption is fenced separately by the input ledger and Task state.
        if (receipt.get("status") not in {"queued", "dispatching", "running", "completed"}
                or receipt["actor"] != sender or receipt["recipient"] != own
                or receipt["room_id"] != room
                or meta.get("sender_id") != sender or meta.get("agent_matrix_user_id") != own
                or meta.get("room_id") != room or wire.get("session_id") != packet["session_id"]
                or any(event.get(k, "") != canonical.get(k, "") for k in
                    ("id", "kind", "task_id", "main_task_id", "project_id", "delivery_id", "submission_id"))):
            raise IgnoredNotification("notification scope changed")
        kind = canonical["kind"]
        if kind in {"task_assigned", "task_submitted"}:
            task = verified_get("tasks/" + urllib.parse.quote(canonical["task_id"], safe="") + "/meta")
            if any(task.get(k, "") != canonical.get(k, "") for k in ("main_task_id", "project_id")):
                raise IgnoredNotification("task membership changed")
            if kind == "task_assigned":
                owners = {value for value in (own, own.split(":", 1)[0].lstrip("@"), os.getenv("AGENTTEAMS_WORKER_NAME"), os.getenv("AGENTTEAMS_WORKER_CR_NAME")) if value}
                if (task.get("status") not in {"assigned", "in_progress"}
                        or task.get("notifyTxn") != canonical.get("delivery_id")
                        or task.get("assigned_to") not in owners):
                    raise IgnoredNotification("assignment changed")
            elif task.get("status") != "submitted" or task.get("submission_id") != canonical.get("submission_id"):
                raise IgnoredNotification("submission changed")
        elif kind in {"main_task_ready", "project_ready"}:
            main = canonical.get("main_task_id")
            path = ("tasks/" + urllib.parse.quote(main, safe="") if main else "projects/" + urllib.parse.quote(canonical["project_id"], safe="")) + "/meta"
            plan = verified_get(path)
            nodes = plan.get("tasks", [])
            parents = {n.get("parent_task_id") for n in nodes}
            leaves = [n for n in nodes if n.get("task_id") not in parents]
            if (plan.get("status") not in ({"in_progress", "submitted"} if main else {"active"})
                    or (main and plan.get("task_kind") != "main")
                    or not leaves or any(n.get("status") != "completed" for n in leaves)
                    or sender != own):
                raise IgnoredNotification("plan is not ready to close")
        elif kind != "handoff":
            raise IgnoredNotification("unsupported notification kind")
        text = packet["input"][0]["content"][0]["text"]
        if not isinstance(text, str) or not text.strip():
            raise IgnoredNotification("missing canonical input")
        return {"text": text, "session_id": packet["session_id"], "meta": packet["channel_meta"]}
    except IgnoredNotification:
        raise
    except (KeyError, TypeError, ValueError, OSError) as exc:
        raise IgnoredNotification("coordination receipt unavailable or invalid") from exc


if __name__ == "__main__":
    try:
        args = json.load(sys.stdin)
        print(json.dumps(notification(args["content"], args["room"], args["sender"], args["own"], event_id=args.get("event_id"))))
    except (IgnoredNotification, KeyError):
        print("null")
        sys.exit(1)
