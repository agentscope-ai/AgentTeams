"""Public Task preparation validation shared by native runtime adapters."""
from pathlib import Path
import re

# These protocol phases coordinate work; only task_assigned executes it.
COORDINATION_PHASES = frozenset({"handoff", "task_submitted", "main_task_ready"})


def task_write_path_allowed(file_path, output_directory, scratch_directory):
    """Check a native file write against the prepared writable directories."""
    if not isinstance(file_path, str) or not file_path:
        return False
    try:
        output = Path(output_directory).resolve()
        scratch = Path(scratch_directory).resolve()
        target = (output / Path(file_path).expanduser()).resolve()
        return any(target != directory and target.is_relative_to(directory)
                   for directory in (output, scratch))
    except (OSError, RuntimeError, ValueError):
        return False


def prepare_event(event, call):
    """Prepare a trusted notification using the common Task operation once."""
    if not isinstance(event, dict):
        raise ValueError("verified task event required")
    kind = event.get("kind")
    if kind == "assistant_digest":
        if any(event.get(key) for key in ("task_id", "main_task_id", "project_id")):
            raise ValueError("assistant reminder cannot select a business Task")
        # Teams notification authority is checked before this boundary. This
        # control turn has no Task begin/get/accept/complete operation to prepare.
        return {}
    if kind == "handoff":
        identities = event.get("pending_task_ids")
        if (not isinstance(identities, list) or not 0 < len(identities) <= 50
                or any(not isinstance(identity, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", identity)
                       for identity in identities) or len(set(identities)) != len(identities)):
            raise ValueError("handoff requires the admitted pending task IDs")
        return {"planning": list(identities)}
    if kind not in {"task_assigned", "task_submitted", "main_task_ready"}:
        raise ValueError("unsupported task preparation event")
    identity = event.get("main_task_id") if kind == "main_task_ready" else event.get("task_id")
    if not isinstance(identity, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", identity):
        raise ValueError("verified task identity required")
    arguments = {"action": "begin" if kind == "task_assigned" else "get", "taskId": identity}
    result = call(arguments)
    if isinstance(result, dict) and result.get("ok") is False:
        return {"ok": False, "result": result, "retryable": result.get("retryable") is True}
    return prepare(kind, event, result)


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
        return {"fact": "Your assignment is acknowledged and inputs are prepared. Do not repeat begin or download supplied inputs. "
                        "Use the original request for context and produce only the deliverables assigned to your node. "
                        "Prefer native file tools for simple reads and writes. Use shell or patch tools when the assignment needs them. "
                        "Write each deliverable using the exact absolute outputDirectory below plus its filename. "
                        "Use scratchDirectory for temporary work. Pass files as a JSON array of paths relative to outputDirectory. "
                        "Once you have produced and checked your deliverables, use the native Task tool to submit before ending your turn.",
                "context": result, "execution": task_id,
                "ack": (task_id, root.resolve(), task["assignedTo"], task["assignmentId"])}
    if kind == "task_submitted":
        candidate = task.get("candidate") or {}
        submission = event.get("submission_id")
        if (task.get("status") != "submitted" or not submission or task.get("submissionId") != submission
                or candidate.get("id") != submission or not isinstance(candidate.get("artifacts"), list)
                or not isinstance(candidate.get("reportFile"), dict) or not candidate["reportFile"].get("artifactId")):
            raise ValueError("task candidate does not match this review")
        return {"fact": f"Review this exact candidate against its task spec. This is a public snapshot, not a completed business review. Your review is inspection of the published evidence, not another test execution. Read missing report or artifact evidence once. For accept or reject, set taskId to {task_id} and give a concrete note. Omit submissionId in the native tool to use the prepared candidate. Recorded test results belong to the Worker; do not claim you ran them. Accepting this step releases its dependencies, not final acceptance of the whole request: do not wait for downstream QA that depends on this step. This is a decision-only phase: only common Task operations are available. Do not download files to assemble a test environment, repeat unchanged reads, execute or edit outputs, or seek batch execution, subagents or another workflow. If required proof is missing or inconsistent, reject with explicit replacementSpec instead of trying to execute checks yourself; the task system dispatches the correction and dependencies.",
                "context": task, "review": (task_id, submission)}
    if kind == "main_task_ready":
        nodes = task.get("nodes")
        if (task.get("kind") != "main" or task.get("status") not in {"in_progress", "submitted"}
                or not isinstance(nodes, list) or not nodes
                or any(node.get("status") != "completed" for node in nodes)):
            raise ValueError("main task is not ready to close")
        accepted = task.get("acceptedResults")
        remaining = {node.get("taskId") for node in nodes}
        if (not isinstance(accepted, list) or len(accepted) != len(nodes)
                or len(remaining) != len(nodes) or not all(isinstance(identity, str) and identity for identity in remaining)):
            raise ValueError("accepted result evidence is incomplete")
        for child in accepted:
            if not isinstance(child, dict):
                raise ValueError("accepted result evidence is invalid")
            candidate, review = child.get("candidate"), child.get("review")
            submission = child.get("submissionId")
            if (child.get("id") not in remaining or child.get("mainTaskId") != task_id
                    or child.get("projectId", "") != task.get("projectId", "") or child.get("kind") != "execution"
                    or child.get("status") != "completed" or not isinstance(submission, str) or not submission
                    or not isinstance(candidate, dict) or candidate.get("id") != submission
                    or not isinstance(review, dict) or not review.get("id") or review.get("accepted") is not True
                    or review.get("status") != "completed" or review.get("submissionId") != submission):
                raise ValueError("accepted result identity or saved review changed")
            remaining.remove(child["id"])
        return {"fact": "All execution steps are accepted. Use acceptedResults below: each id is the actual execution Task ID, with its exact candidate, saved review, report and artifact versions. Verify whole-request coverage from this evidence; do not reread unchanged accepted files or rerun checks. Read more only for a concrete missing or inconsistent requirement, using that result's id, never a local step alias or the main Task ID for child files. Call task complete with your final report, distinguishing recorded tests from your own review. Omit artifacts to deliver all accepted outputs. When selecting a subset, copy each artifactId from acceptedResults. The task system returns report and artifacts to the original Human conversation using the first dispatch identity. Do not send another message or poll.",
                "context": task, "completion": task_id}
    raise ValueError("unsupported task preparation event")
