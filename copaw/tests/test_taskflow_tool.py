import json

import pytest

from copaw_worker.hooks.tools.taskflow import taskflow
from copaw_worker.task import (
    FileSystemTaskStore,
    TaskflowError,
    add_tasks,
    create_project,
    parse_dag_tasks,
)


def _response_json(response):
    return json.loads(response.content[0].text)


@pytest.mark.asyncio
async def test_taskflow_project_assignment_and_completion(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    response = await taskflow(
        action="create_project",
        payload={
            "projectId": "tp-01",
            "title": "Research project",
            "source": "team-admin",
            "requester": "@admin:domain",
        },
    )
    assert _response_json(response)["ok"] is True

    response = await taskflow(
        action="add_tasks",
        payload={
            "projectId": "tp-01",
            "tasks": [
                {
                    "taskId": "st-01",
                    "title": "Collect sources",
                    "assignedTo": "@worker-a:domain",
                    "dependsOn": [],
                },
                {
                    "taskId": "st-02",
                    "title": "Summarize findings",
                    "assignedTo": "@worker-b:domain",
                    "dependsOn": ["st-01"],
                },
            ],
        },
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert [task["task_id"] for task in payload["readyTasks"]] == ["st-01"]

    response = await taskflow(
        action="assign_task",
        payload={
            "projectId": "tp-01",
            "taskId": "st-01",
            "roomId": "room:!worker-room:domain",
            "spec": "Collect sources and write shared/tasks/st-01/result.md.",
        },
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["task"]["status"] == "assigned"
    assert (workspace / "shared" / "tasks" / "st-01" / "spec.md").exists()

    response = await taskflow(action="ack_task", payload={"taskId": "st-01"})
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["task"]["status"] == "in_progress"

    result_path = workspace / "shared" / "tasks" / "st-01" / "result.md"
    result_path.write_text(
        "STATUS: SUCCESS\n"
        "SUMMARY: Sources collected.\n\n"
        "DELIVERABLES:\n"
        "- shared/tasks/st-01/sources.md\n",
    )

    response = await taskflow(action="submit_task", payload={"taskId": "st-01"})
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["task"]["status"] == "submitted"

    response = await taskflow(
        action="complete_task",
        payload={
            "projectId": "tp-01",
            "taskId": "st-01",
        },
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert [task["task_id"] for task in payload["readyTasks"]] == ["st-02"]

    plan = (workspace / "shared" / "projects" / "tp-01" / "plan.md").read_text()
    tasks = {task.task_id: task for task in parse_dag_tasks(plan)}
    assert tasks["st-01"].status == "completed"
    assert tasks["st-02"].status == "pending"


@pytest.mark.asyncio
async def test_submit_task_writes_structured_result(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )

    response = await taskflow(
        action="submit_task",
        payload={
            "taskId": "st-01",
            "status": "SUCCESS",
            "summary": "API design completed.",
            "deliverables": [
                "shared/tasks/st-01/workspace/api-design.md",
            ],
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["status"] == "submitted"
    assert payload["result"] == {
        "status": "SUCCESS",
        "summary": "API design completed.",
        "deliverables": ["shared/tasks/st-01/workspace/api-design.md"],
        "notes": [],
    }
    assert (task_dir / "result.md").read_text() == (
        "STATUS: SUCCESS\n"
        "SUMMARY: API design completed.\n\n"
        "DELIVERABLES:\n"
        "- shared/tasks/st-01/workspace/api-design.md\n"
    )


@pytest.mark.asyncio
async def test_taskflow_add_tasks_accepts_json_string(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    response = await taskflow(
        action="create_project",
        payload={
            "projectId": "tp-json",
            "title": "JSON tasks project",
        },
    )
    assert _response_json(response)["ok"] is True

    response = await taskflow(
        action="add_tasks",
        payload={
            "projectId": "tp-json",
            "tasks": json.dumps(
                [
                    {
                        "taskId": "st-01",
                        "title": "Design API",
                        "assignedTo": "@worker:domain",
                        "dependsOn": [],
                    },
                ],
            ),
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["tasks"][0]["task_id"] == "st-01"
    assert payload["readyTasks"][0]["task_id"] == "st-01"


def test_add_tasks_rejects_unknown_dependency(tmp_path):
    store = FileSystemTaskStore(tmp_path)
    create_project(store, project_id="tp-01", title="Bad graph")

    with pytest.raises(TaskflowError, match="unknown task"):
        add_tasks(
            store,
            project_id="tp-01",
            tasks=[
                {
                    "taskId": "st-02",
                    "title": "Blocked task",
                    "assignedTo": "@worker:domain",
                    "dependsOn": ["st-01"],
                },
            ],
        )


@pytest.mark.asyncio
async def test_submit_task_rejects_invalid_deliverable_path(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )
    (task_dir / "result.md").write_text(
        "STATUS: SUCCESS\n"
        "SUMMARY: Done.\n\n"
        "DELIVERABLES:\n"
        "- shared/projects/tp-01/result.md\n",
    )

    response = await taskflow(action="submit_task", payload={"taskId": "st-01"})
    payload = _response_json(response)
    assert payload["ok"] is False
    assert "deliverable must be under shared/tasks/st-01/" in payload["error"]
