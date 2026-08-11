#!/usr/bin/env python3
"""External Task Trigger - Bridge structured external jobs into AgentTeams.

This is an integration example, not a stable AgentTeams REST API.
It demonstrates how an external backend can bridge structured jobs
into the current Manager-centered Matrix workflow.

Usage:
    python3 scripts/external-task-trigger.py --task example-task.json
    python3 scripts/external-task-trigger.py --task example-task.json --dry-run
    cat example-task.json | python3 scripts/external-task-trigger.py --stdin
"""

import argparse
import json
import os
import subprocess
import sys
import uuid
from datetime import datetime, timezone
from pathlib import Path


REQUIRED_FIELDS = ["team", "skill", "params"]


def generate_task_id():
    return f"task-{uuid.uuid4().hex[:12]}"


def generate_trace_id():
    return f"trace-{uuid.uuid4().hex[:12]}"


def load_task(input_file=None, use_stdin=False):
    if use_stdin:
        raw = sys.stdin.read()
        if not raw.strip():
            raise ValueError("No input received from stdin")
        return json.loads(raw)

    if input_file:
        path = Path(input_file)
        if not path.exists():
            raise FileNotFoundError(f"Task file not found: {input_file}")
        with open(path) as f:
            return json.load(f)

    raise ValueError("Provide --task FILE or --stdin")


def validate_task(task):
    missing = [f for f in REQUIRED_FIELDS if f not in task]
    if missing:
        raise ValueError(f"Missing required field(s): {', '.join(missing)}")
    if not isinstance(task.get("params"), dict):
        raise ValueError("'params' must be a JSON object")
    return True


def build_manager_message(task, task_id, trace_id):
    metadata = task.get("metadata", {})
    external_job_id = metadata.get("external_job_id", "unknown")

    header = (
        f"[EXTERNAL_TASK]\n"
        f"task_id: {task_id}\n"
        f"trace_id: {trace_id}\n"
        f"external_job_id: {external_job_id}\n"
        f"team: {task['team']}\n"
        f"skill: {task['skill']}\n"
        f"---"
    )

    params_section = f"params: {json.dumps(task['params'], indent=2)}"

    context = metadata.get("context", "")
    context_section = f"\ncontext: {context}" if context else ""

    return f"{header}\n{params_section}{context_section}"


def run_replay(message, dry_run=False, no_wait=False):
    if dry_run:
        return json.dumps({
            "status": "completed",
            "result": f"[DRY RUN] Would send task to Manager: {message[:100]}...",
        })

    project_root = Path(__file__).resolve().parent.parent
    replay_script = project_root / "scripts" / "replay-task.sh"

    if not replay_script.exists():
        raise FileNotFoundError(f"replay-task.sh not found at {replay_script}")

    env = os.environ.copy()
    if no_wait:
        env["REPLAY_WAIT"] = "0"

    try:
        result = subprocess.run(
            [str(replay_script), message],
            capture_output=True,
            text=True,
            timeout=600,
            env=env,
            cwd=str(project_root),
        )
        if result.returncode != 0:
            error_msg = result.stderr.strip() or result.stdout.strip()
            raise RuntimeError(f"replay-task.sh failed: {error_msg}")
        return result.stdout.strip()
    except subprocess.TimeoutExpired:
        raise RuntimeError("replay-task.sh timed out after 600s")


def run_task(task_file=None, use_stdin=False, dry_run=False, no_wait=False):
    try:
        task = load_task(task_file, use_stdin)
        validate_task(task)
    except (ValueError, FileNotFoundError, json.JSONDecodeError) as e:
        return {
            "status": "error",
            "error": str(e),
        }

    task_id = generate_task_id()
    trace_id = generate_trace_id()
    submitted_at = datetime.now(timezone.utc).isoformat()

    manager_message = build_manager_message(task, task_id, trace_id)

    try:
        raw_result = run_replay(manager_message, dry_run=dry_run, no_wait=no_wait)
    except Exception as e:
        return {
            "task_id": task_id,
            "trace_id": trace_id,
            "status": "error",
            "error": str(e),
            "external_job_id": task.get("metadata", {}).get("external_job_id"),
            "submitted_at": submitted_at,
        }

    if no_wait:
        status = "submitted"
        result_text = None
    elif dry_run:
        status = "completed"
        result_text = raw_result
    else:
        status = "completed"
        result_text = raw_result

    return {
        "task_id": task_id,
        "trace_id": trace_id,
        "status": status,
        "result": result_text,
        "external_job_id": task.get("metadata", {}).get("external_job_id"),
        "submitted_at": submitted_at,
    }


def main():
    parser = argparse.ArgumentParser(
        description="External Task Trigger - Bridge structured jobs into AgentTeams",
    )
    input_group = parser.add_mutually_exclusive_group()
    input_group.add_argument(
        "--task",
        metavar="FILE",
        help="Path to JSON task file",
    )
    input_group.add_argument(
        "--stdin",
        action="store_true",
        help="Read task JSON from stdin",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Simulate submission without calling Matrix",
    )
    parser.add_argument(
        "--no-wait",
        action="store_true",
        help="Submit without waiting for Manager reply",
    )

    args = parser.parse_args()

    if not args.task and not args.stdin:
        parser.print_help()
        print("\nError: Provide --task FILE or --stdin", file=sys.stderr)
        sys.exit(1)

    try:
        output = run_task(
            task_file=args.task,
            use_stdin=args.stdin,
            dry_run=args.dry_run,
            no_wait=args.no_wait,
        )
    except (ValueError, FileNotFoundError) as e:
        output = {
            "status": "error",
            "error": str(e),
        }
        print(json.dumps(output, indent=2))
        sys.exit(1)

    print(json.dumps(output, indent=2))

    if output.get("status") == "error":
        sys.exit(1)


if __name__ == "__main__":
    main()