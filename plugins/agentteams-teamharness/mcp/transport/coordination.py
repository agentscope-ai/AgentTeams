"""Optional structured coordination transport; task state remains authoritative."""
from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any
import urllib.error
import urllib.request
import urllib.parse

from mcp.room.matrix import _matrix_user_id


def native_team_members(team_id: str) -> list[dict[str, Any]]:
    """Read registered runtime capabilities before storing an automatic plan."""
    try:
        token = Path(os.environ["AGENTTEAMS_AUTH_TOKEN_FILE"]).read_text().strip()
        base = os.environ["AGENTTEAMS_CONTROLLER_URL"].rstrip("/")
        request = urllib.request.Request(base + "/api/v1/teams/" + urllib.parse.quote(team_id, safe=""),
            headers={"Authorization": "Bearer " + token})
        with urllib.request.urlopen(request, timeout=5) as response:
            result = json.load(response)
    except (OSError, ValueError, KeyError) as exc:
        raise ValueError("registered native coordination capabilities unavailable") from exc
    members = result.get("members")
    if not isinstance(members, list):
        raise ValueError("registered native coordination capabilities unavailable")
    return members


def deliver_event(kind: str, **event: Any) -> dict[str, Any] | None:
    if kind == "handoff" and event.get("tasks") is not None:
        tasks = event["tasks"]
        if isinstance(tasks, str):
            try:
                tasks = json.loads(tasks)
            except ValueError:
                return {"sent": False, "error": "tasks must be an array of task objects, not invalid JSON"}
        if not isinstance(tasks, list) or any(not isinstance(task, dict) for task in tasks):
            return {"sent": False, "error": "tasks must be an array of task objects"}
        event["tasks"] = tasks
    actor = _matrix_user_id()
    endpoint = os.getenv("TEAMHARNESS_COORDINATION_URL", "").strip().rstrip("/")
    if not endpoint and ((kind == "handoff" and event.get("tasks")) or
                         (kind in {"task_assigned", "task_submitted"} and actor and
                          (event.get("main_task_id") or event.get("recipient") == actor))):
        controller = os.getenv("AGENTTEAMS_CONTROLLER_URL", "").strip().rstrip("/")
        if controller:
            endpoint = controller + "/api/v1/coordination"
    if not endpoint:
        return None
    try:
        token = Path(os.environ["AGENTTEAMS_AUTH_TOKEN_FILE"]).read_text().strip()
        headers = {"Content-Type": "application/json", "Authorization": "Bearer " + token}
        try:
            from opentelemetry.propagate import inject
            inject(headers)
        except ImportError:
            pass
        request = urllib.request.Request(endpoint + "/events", method="POST",
            data=json.dumps({**event, "kind": kind, "actor": actor}).encode(), headers=headers)
        with urllib.request.urlopen(request, timeout=5) as response:
            data = json.load(response)
        if isinstance(data, dict) and data.get("external"):
            return {"sent": False, "external": True}
        if not isinstance(data, dict) or not data.get("accepted"):
            return {"sent": False, "error": "coordination service did not accept the event"}
        return {"sent": True, "accepted": True, "transport": "agent-api",
                "eventId": data.get("matrix_event_id") or "api-" + data["event_id"],
                "deliveryId": data["event_id"], "reused": bool(data.get("reused"))}
    except urllib.error.HTTPError as exc:
        return {"sent": False, "transport": "agent-api", "httpStatus": exc.code,
                "error": f"coordination delivery failed: HTTP {exc.code}"}
    except (OSError, ValueError, KeyError, urllib.error.URLError) as exc:
        return {"sent": False, "transport": "agent-api", "error": f"coordination delivery failed: {type(exc).__name__}"}
