"""Public Task preparation validation shared by native runtime adapters."""
from pathlib import Path

# These protocol phases coordinate work; only task_assigned executes it.
COORDINATION_PHASES = frozenset({"handoff", "task_submitted", "main_task_ready"})


def prepare(kind, event, result):
    """Return code-generated context and native operation scope, or fail closed."""
    task = result.get("task") if isinstance(result, dict) else None
    if not isinstance(task, dict) or result.get("ok") is False:
        raise ValueError("task preparation unavailable")
    task_id = event.get("main_task_id") if kind == "main_task_ready" else event.get("task_id")
    if task.get("id") != task_id:
        raise ValueError("task preparation identity changed")
    if kind != "main_task_ready" and (task.get("mainTaskId", "") != event.get("main_task_id", "")
                                     or task.get("projectId", "") != event.get("project_id", "")):
        raise ValueError("task preparation scope changed")
    if kind == "task_assigned":
        root = Path(result.get("taskDirectory") or ".")
        output = Path(result.get("outputDirectory") or ".")
        scratch = Path(result.get("scratchDirectory") or ".")
        if (task.get("status") != "in_progress" or not event.get("delivery_id")
                or task.get("assignmentId") != event["delivery_id"] or not task.get("assignedTo")
                or not root.is_absolute() or root.name != task_id or root.parent.name != "tasks"
                or not output.is_absolute() or not scratch.is_absolute()
                or not output.resolve().is_relative_to(root.resolve()) or not scratch.resolve().is_relative_to(root.resolve())):
            raise ValueError("task begin receipt does not match this assignment")
        return {"fact": "Your assignment is acknowledged and inputs are prepared. Do not repeat begin or download supplied inputs. Execute from this task spec and the authoritative directories; submit only your output files.",
                "context": result, "execution": task_id,
                "ack": (task_id, root.resolve(), task["assignedTo"], task["assignmentId"])}
    if kind == "task_submitted":
        candidate = task.get("candidate") or {}
        submission = event.get("submission_id")
        if (task.get("status") != "submitted" or not submission or task.get("submissionId") != submission
                or candidate.get("id") != submission or not isinstance(candidate.get("artifacts"), list)
                or not isinstance(candidate.get("reportFile"), dict) or not candidate["reportFile"].get("artifactId")):
            raise ValueError("task candidate does not match this review")
        return {"fact": "Review this exact candidate against its task spec. This is a public snapshot, not a completed business review. Read reportFile or artifact IDs when evidence is insufficient, then accept or reject this submission with your independent evidence. This is a decision-only phase: use common Task operations, not batch execution, subagents or another workflow. Do not execute or edit the Worker's outputs. If proof is missing, reject with explicit replacementSpec; the task system dispatches the correction and dependencies.",
                "context": task, "review": (task_id, submission)}
    if kind == "main_task_ready":
        nodes = task.get("nodes")
        if (task.get("kind") != "main" or task.get("status") not in {"in_progress", "submitted"}
                or not isinstance(nodes, list) or not nodes
                or any(node.get("status") != "completed" for node in nodes)):
            raise ValueError("main task is not ready to close")
        return {"fact": "All execution steps are accepted. Verify the whole original request and call task complete with your final report. The task system returns the report and accepted artifacts to the original Human conversation using the first dispatch Agent identity. Do not send another message or poll.",
                "context": task, "completion": task_id}
    raise ValueError("unsupported task preparation event")
