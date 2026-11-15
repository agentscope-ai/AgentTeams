#!/usr/bin/env python3
"""Convert a legacy Project JSON plan into one main task and its subtasks.

Reads/writes local JSON files only. Callers own storage backups and concurrency
checks; existing execution task records and task dependencies are not changed.
"""
import argparse
from copy import deepcopy
import json
from pathlib import Path


def migrate_project(project):
    result = deepcopy(project)
    tasks = result.get("tasks", [])
    if not tasks or any(task.get("parent_task_id") for task in tasks):
        return result
    if result.get("plan_type") not in (None, "", "dag"):
        raise ValueError("only legacy DAG projects can be migrated")
    main_id = result["project_id"] + "-main"
    if any(task.get("task_id") == main_id for task in tasks):
        raise ValueError("main task id already exists")
    all_done = all(task.get("status") == "completed" for task in tasks)
    status = "completed" if all_done and result.get("status") == "completed" else "submitted" if all_done else "in_progress"
    main = {"task_id": main_id, "title": result.get("title") or result["project_id"], "assigned_to": "", "depends_on": [], "status": status}
    for task in tasks:
        task["parent_task_id"] = main_id
    result["tasks"] = [main, *tasks]
    return result


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    args.output.write_text(json.dumps(migrate_project(json.loads(args.input.read_text())), ensure_ascii=False, indent=2) + "\n")
