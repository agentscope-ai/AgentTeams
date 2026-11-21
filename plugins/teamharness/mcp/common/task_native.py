"""JSON-only native SDK bridge to the shared Task CLI and receipt validators.

Native bindings supply trusted context separately from model arguments. They
persist refreshed Human inputs and delivery acknowledgements in their own SDK.
"""
from __future__ import annotations

import json
import asyncio
import copy
import os
from pathlib import Path
import re
import sys
import urllib.request

if __package__:
    from . import phase_recovery, task_cli, task_context, task_receipt as receipts
else:
    import task_cli
    import task_context
    import task_receipt as receipts
    import phase_recovery


def controller_json(route):
    """Use operator-provided public API configuration, never model routing."""
    token = os.getenv("AGENTTEAMS_AUTH_TOKEN", "").strip()
    if not token:
        token = Path(os.getenv("AGENTTEAMS_AUTH_TOKEN_FILE") or "/var/run/secrets/agentteams/token").read_text().strip()
    url = os.environ["AGENTTEAMS_CONTROLLER_URL"].rstrip("/") + "/api/v1/" + route
    request = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(request, timeout=10) as response:
        return json.load(response)


def native_role(value):
    role = {"team_leader": "leader", "standalone": "worker"}.get(value, value)
    if role not in {"manager", "leader", "worker", "remote-member"}:
        raise ValueError("native runtime role required")
    return role


def invoke(payload, *, call=None, get_json=None, notify=None):
    """Execute one bound operation without process-global context mutation."""
    if not isinstance(payload, dict):
        raise ValueError("native bridge request must be an object")
    call = call or task_cli.run
    operation = payload.get("operation")
    if operation == "admit_event":
        event, native, action = payload.get("eventId"), payload.get("nativeEventId"), payload.get("action", "begin")
        if (not isinstance(event, str) or not re.fullmatch(r"[0-9a-f]{32}", event)
                or not isinstance(native, str) or not native or action not in {"begin", "retry"}):
            raise ValueError("verified notification and native delivery identity required")
        return asyncio.run((notify or phase_recovery.notify)(event, native, action))
    if operation == "sources":
        meta = payload.get("meta")
        if not isinstance(meta, dict):
            raise ValueError("trusted native source metadata required")
        return receipts.creation_sources(meta)
    if operation == "schema":
        return task_cli.schema(native_role(payload.get("role")))
    if operation == "prepare_event":
        # Paths and tuples remain native in prepare(); JSON is this bridge's IO.
        return json.loads(json.dumps(task_context.prepare_event(payload.get("event"), call), default=str))
    if operation == "delivery_context":
        room, middle = payload.get("room"), payload.get("middle")
        if not isinstance(room, str) or not room.startswith("!") or not isinstance(middle, dict):
            raise ValueError("native room and persisted delivery context required")
        facts, acknowledgements = receipts.delivery_context(middle, room, get_json or controller_json)
        return {"facts": facts, "acknowledgements": acknowledgements}
    if operation == "ack_delivery_context":
        middle, acknowledgements = payload.get("middle"), payload.get("acknowledgements")
        if (not isinstance(middle, dict) or not isinstance(acknowledgements, dict)
                or any(not isinstance(key, str) or not key or not isinstance(room, str) or not room.startswith("!")
                       for key, room in acknowledgements.items())):
            raise ValueError("persisted native context and exact delivery acknowledgements required")
        updated = copy.deepcopy(middle)
        receipts.ack_delivery_context(updated, acknowledgements)
        return updated
    if operation != "task":
        raise ValueError("unknown native bridge operation")
    arguments, context = payload.get("arguments"), payload.get("context")
    if not isinstance(arguments, dict) or not isinstance(context, dict):
        raise ValueError("model arguments and trusted native context required")
    if "_nativeSource" in arguments:
        raise ValueError("model arguments cannot supply _nativeSource")
    arguments = dict(arguments)
    action, identity = arguments.get("action"), arguments.get("taskId")
    allowed = task_cli.schema(native_role(context.get("role")))["inputSchema"]["properties"]["action"]["enum"]
    if action not in allowed:
        raise ValueError("task action unavailable to this runtime role")
    review = context.get("review")
    if action in {"begin", "submit"} and (not context.get("execution") or identity != context["execution"]):
        raise ValueError("task operation does not match the current execution")
    if action == "plan" and identity not in (context.get("planning") or []):
        raise ValueError("task plan does not match the admitted handoff")
    if action == "complete" and (not context.get("completion") or identity != context["completion"]):
        raise ValueError("task completion does not match the accepted main task")
    if action in {"accept", "reject"} and (not isinstance(review, (list, tuple)) or len(review) != 2
            or [identity, arguments.get("submissionId")] != list(review)):
        raise ValueError("task review does not match the exact prepared candidate")
    source = ""
    sources = context.get("sources") or []
    if action == "create":
        if context.get("inputRefreshPending"):
            raise ValueError("native Human input checkpoint required before another task creation")
        if not isinstance(sources, list) or not sources:
            raise ValueError("native original-conversation context unavailable")
        selected = receipts.select_creation_source(sources, arguments.get("inputId"), arguments.get("relatedInputIds"))
        source = selected["eventId"]
        arguments["_nativeSource"] = selected
    result = call(arguments)
    response = {"result": result, "phaseComplete": False}
    if action == "create":
        refreshed = receipts.refresh_creation_inputs(result, sources, source)
        if refreshed:
            updated = [*sources, *[{key: value for key, value in entry.items() if key != "body"} for entry in refreshed]]
            response.update(fact=receipts.input_refresh_context(result, refreshed, updated),
                            refreshedInputs=refreshed, requiresInputCheckpoint=True,
                            contextUpdate={"sources": updated, "inputRefreshPending": True})
            return response
        receipt = receipts.creation_receipt(result, source)
    elif action == "plan":
        receipt = receipts.planning_receipt(result, identity)
    elif action in {"accept", "reject"}:
        receipt = receipts.review_receipt(result, identity, review[1], action)
    else:
        receipt = receipts.task_receipt(result, identity, action)
    if receipt:
        response.update(receiptId=receipt[0], fact=receipt[1])
        if action == "plan":
            planned = sorted(set(context.get("planned") or []) | {identity})
            response["contextUpdate"] = {"planned": planned}
            response["phaseComplete"] = set(context["planning"]).issubset(planned)
        elif action != "create":
            response["phaseComplete"] = True
    return response


if __name__ == "__main__":
    try:
        print(json.dumps(invoke(json.load(sys.stdin)), ensure_ascii=False))
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(json.dumps({"ok": False, "error": "native_task_context_unavailable", "message": str(error)[:2048]}))
        sys.exit(1)
