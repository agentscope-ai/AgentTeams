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
import time
import urllib.request
from urllib.parse import quote

if __package__:
    from . import phase_recovery, task_cli, task_context, task_roster, task_receipt as receipts
else:
    import task_cli
    import task_context
    import task_receipt as receipts
    import phase_recovery
    import task_roster


def controller_json(route, *, timeout=10):
    """Use operator-provided public API configuration, never model routing."""
    token = os.getenv("AGENTTEAMS_AUTH_TOKEN", "").strip()
    if not token:
        token = Path(os.getenv("AGENTTEAMS_AUTH_TOKEN_FILE") or "/var/run/secrets/agentteams/token").read_text().strip()
    url = os.environ["AGENTTEAMS_CONTROLLER_URL"].rstrip("/") + "/api/v1/" + route
    request = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.load(response)


def dispatched_receipts(result, actor, get_json, *, wait_for_send=False):
    """Confirm actual Matrix sends for the authenticated outgoing Task events."""
    task = result["task"]
    notices = [(item["eventId"], item["taskId"], "task_assigned")
               for item in result.get("assignments", []) if item.get("status") == "queued" and item.get("eventId")]
    notice = result.get("notification") or {}
    if notice.get("status") == "queued" and notice.get("eventId"):
        notices.append((notice["eventId"], task["id"], "task_submitted"))
    confirmed = []
    # One dispatcher interval for the whole batch; never wait for downstream execution.
    deadline = time.monotonic() + 2.25 if wait_for_send else 0
    for event, identity, kind in notices:
        while True:
            try:
                saved = get_json("coordination/events/" + event)
            except (OSError, ValueError):
                saved = {}  # The canonical fact remains queued, not sent.
            native = saved.get("native_id", "")
            scope_matches = (saved.get("task_id") == identity and saved.get("kind") == kind)
            if kind == "task_submitted" and saved.get("kind") == "main_task_ready":
                scope_matches = saved.get("main_task_id") == task.get("mainTaskId")
            if saved.get("event_id") != event or saved.get("actor") != actor or not scope_matches:
                break
            if isinstance(native, str) and native.startswith("matrix:$"):
                break
            if time.monotonic() >= deadline:
                break
            time.sleep(0.15)
        if (saved.get("event_id") != event or saved.get("actor") != actor or not scope_matches
                or not isinstance(native, str) or not native.startswith("matrix:$")):
            continue
        confirmed.append({"receiptId": json.dumps(["dispatch", event, native], separators=(",", ":")),
                          "fact": "[Task handoff receipt]\n" + json.dumps({
                              "taskId": task["id"], "submissionId": task["submissionId"],
                              "downstreamTaskId": saved.get("task_id") or saved.get("main_task_id"),
                              "recipient": saved["recipient"], "roomId": saved["room_id"],
                              "eventId": event, "matrixEventId": native[7:], "status": "sent"},
                              ensure_ascii=False, separators=(",", ":")) + "\nAlready notified. No action or further reasoning is needed."})
    return confirmed


def native_role(value):
    role = {"team_leader": "leader", "standalone": "worker"}.get(value, value)
    if role not in {"manager", "leader", "worker", "remote-member"}:
        raise ValueError("native runtime role required")
    return role


def model_result(outcome):
    """Keep native terminal facts, without another copy of the Task snapshot."""
    if not outcome.get("phaseComplete"):
        result = outcome.get("result", outcome)
        return {**result, **{key: outcome[key] for key in ("receiptId", "fact", "extraReceipts") if key in outcome}}
    task = (outcome.get("result") or {}).get("task") or {}
    return {"phaseComplete": True,
            "task": {key: task[key] for key in ("id", "kind", "status", "submissionId", "completionId", "planId") if key in task},
            **{key: outcome["result"][key] for key in ("sourceEventId",) if key in (outcome.get("result") or {})},
            **{key: outcome[key] for key in ("receiptId", "fact", "extraReceipts") if key in outcome}}


def terminal_display(action, arguments, result):
    """Business presentation is separate from durable control facts."""
    task = result.get("task") or {}
    if action == "create" and task.get("roomId"):
        link = "#workbench/tasks/" + quote(task["id"], safe="") + "/collaboration?room=" + quote(task["roomId"], safe="")
        return "已受理「" + task.get("title", task["id"]) + "」，接下来在任务房间规划和执行。[查看任务](" + link + ")"
    if action == "complete":
        return arguments.get("report") or task.get("report") or result.get("report")


def _invoke(payload, *, call=None, get_json=None, notify=None):
    """Execute one bound operation without process-global context mutation."""
    if not isinstance(payload, dict):
        raise ValueError("native bridge request must be an object")
    call = call or task_cli.run
    operation = payload.get("operation")
    if operation == "canonical_receipts":
        if __package__:
            from . import task_canonical
        else:
            import task_canonical
        return {'receipts': task_canonical.admission_receipts(payload.get('admission'), payload.get('taskId'))}
    if operation == "canonical_admit":
        if __package__:
            from . import task_canonical
        else:
            import task_canonical
        return task_canonical.admit(payload.get("context") or {})
    if operation == "roster":
        return task_roster.roster(native_role(payload.get("role")), get_json or controller_json, payload.get("team"))
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
    if operation == "model_context":
        if not isinstance(payload.get("context"), dict):
            raise ValueError("prepared Task context required")
        return task_context.model_context(payload["context"])
    if operation == "model_result":
        return model_result(payload["outcome"])
    if operation == "prepare_event":
        # Paths and tuples remain native in prepare(); JSON is this bridge's IO.
        return json.loads(json.dumps(task_context.prepare_event(payload.get("event"), call), default=str))
    if operation == "delivery_context":
        room, middle = payload.get("room"), payload.get("middle")
        input_text = payload.get("inputText", "")
        if (not isinstance(room, str) or not room.startswith("!") or not isinstance(middle, dict)
                or not isinstance(input_text, str)):
            raise ValueError("native room and persisted delivery context required")
        facts, acknowledgements = receipts.delivery_context(middle, room, get_json or controller_json, input_text=input_text)
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
    if action == "complete" and not arguments.get("artifacts"):
        names = set()
        for accepted in (context.get("context") or {}).get("acceptedResults", []):
            for artifact in (accepted.get("candidate") or {}).get("artifacts", []):
                name = artifact.get("name")
                if name in names:
                    return {"result": {"ok": False, "effect": "none", "error": "final_artifact_selection_required",
                            "message": "Accepted results contain repeated output filenames. Select the final deliverable set explicitly with artifactId values from acceptedResults; exclude superseded intermediate versions."},
                            "phaseComplete": False}
                names.add(name)
    if action in {"accept", "reject"}:
        arguments["submissionId"] = receipts.prepared_submission_id(arguments, review)
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
    if __package__:
        from . import task_canonical
    else:
        import task_canonical
    admission = task_canonical.admit(context)
    if admission.get('ok') is not True:
        return admission
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
            response["phaseComplete"] = (set(context["planning"]).issubset(planned)
                                         and all(item["eventId"] in (context.get("registeredInputs") or []) for item in sources))
        elif action == "create":
            registered = sorted(set(context.get("registeredInputs") or []) | {source, *selected.get("relatedSources", [])})
            response["fact"] += receipts.creation_coverage_context(sources, set(registered))
            response["contextUpdate"] = {"registeredInputs": registered}
            inline = (result.get("notification") or {}).get("status") == "inline"
            task = result.get("task") or {}
            if inline and task.get("status") == "planned" and not task.get("planId"):
                response["contextUpdate"]["planning"] = sorted(set(context.get("planning") or []) | {task["id"]})
                return response
            # Release your intake turn before consuming the queued handoff.
            # Unhandled Human inputs must remain available in this turn.
            response["phaseComplete"] = (native_role(context.get("role")) == "leader"
                                         and all(item["eventId"] in registered for item in sources))
        else:
            response["phaseComplete"] = True
    if (receipt and response["phaseComplete"] and action == "submit" and context.get("actor")
            and (get_json or os.getenv("AGENTTEAMS_CONTROLLER_URL"))):
        response["extraReceipts"] = dispatched_receipts(result, context["actor"],
            get_json or (lambda route: controller_json(route, timeout=2)), wait_for_send=True)
    if receipt and response["phaseComplete"]:
        display = terminal_display(action, arguments, result)
        if display is not None:
            response["displayText"] = display
        elif action == "plan" and result.get("progress") != "ready":
            response["displayText"] = "任务规划已保存，调度尚未完成，请查看任务状态。"
    return response


class NativeTaskValidationError(ValueError):
    """A rejected native binding before any CLI/API call, never an unknown write."""
    effect = "none"


def invoke(payload, *, call=None, get_json=None, notify=None):
    started = False
    def invoke_cli(arguments):
        nonlocal started
        started = True
        return (call or task_cli.run)(arguments)
    try:
        return _invoke(payload, call=invoke_cli, get_json=get_json, notify=notify)
    except (ValueError, KeyError, TypeError) as error:
        if isinstance(payload, dict) and payload.get("operation") == "task" and not started:
            raise NativeTaskValidationError(str(error)) from error
        raise


if __name__ == "__main__":
    try:
        print(json.dumps(invoke(json.load(sys.stdin)), ensure_ascii=False))
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(json.dumps({"ok": False, "error": "native_task_context_unavailable", "message": str(error)[:2048],
                          "effect": getattr(error, "effect", "unknown")}))
        sys.exit(1)
