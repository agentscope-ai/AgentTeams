#!/usr/bin/env python3
"""Tests for WorkerFlow MCP server filesystem utilities."""

from __future__ import annotations

import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[4]
SERVER_PATH = REPO_ROOT / "plugins" / "workerflow" / "mcp" / "server.py"


def load_server():
    spec = importlib.util.spec_from_file_location("workerflow_mcp_server", SERVER_PATH)
    assert spec is not None
    assert spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class WorkerFlowSharedDirTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory(prefix="workerflow-shared-")
        self.root = Path(self.tmp.name)
        self.old_env = {
            "QWENPAW_WORKING_DIR": os.environ.get("QWENPAW_WORKING_DIR"),
            "QWENPAW_DEFAULT_WORKSPACE_DIR": os.environ.get("QWENPAW_DEFAULT_WORKSPACE_DIR"),
            "QWENPAW_API_BASE_URL": os.environ.get("QWENPAW_API_BASE_URL"),
            "QWENPAW_BASE_URL": os.environ.get("QWENPAW_BASE_URL"),
            "TEAMHARNESS_RUNTIME_CONFIG": os.environ.get("TEAMHARNESS_RUNTIME_CONFIG"),
            "AGENTTEAMS_MEMBER_RUNTIME_CONFIG": os.environ.get("AGENTTEAMS_MEMBER_RUNTIME_CONFIG"),
            "AGENTTEAMS_MATRIX_URL": os.environ.get("AGENTTEAMS_MATRIX_URL"),
            "AGENTTEAMS_MATRIX_SERVER": os.environ.get("AGENTTEAMS_MATRIX_SERVER"),
            "AGENTTEAMS_MATRIX_HOMESERVER": os.environ.get("AGENTTEAMS_MATRIX_HOMESERVER"),
            "AGENTTEAMS_WORKER_MATRIX_TOKEN": os.environ.get("AGENTTEAMS_WORKER_MATRIX_TOKEN"),
            "AGENTTEAMS_MATRIX_TOKEN": os.environ.get("AGENTTEAMS_MATRIX_TOKEN"),
            "WORKERFLOW_MATRIX_TOKEN": os.environ.get("WORKERFLOW_MATRIX_TOKEN"),
            "AGENTTEAMS_MATRIX_USER_ID": os.environ.get("AGENTTEAMS_MATRIX_USER_ID"),
        }
        os.environ["QWENPAW_WORKING_DIR"] = str(self.root / ".qwenpaw")
        os.environ.pop("QWENPAW_DEFAULT_WORKSPACE_DIR", None)
        os.environ.pop("QWENPAW_API_BASE_URL", None)
        os.environ.pop("QWENPAW_BASE_URL", None)
        os.environ.pop("TEAMHARNESS_RUNTIME_CONFIG", None)
        os.environ.pop("AGENTTEAMS_MEMBER_RUNTIME_CONFIG", None)
        os.environ.pop("AGENTTEAMS_MATRIX_URL", None)
        os.environ.pop("AGENTTEAMS_MATRIX_SERVER", None)
        os.environ.pop("AGENTTEAMS_MATRIX_HOMESERVER", None)
        os.environ.pop("AGENTTEAMS_WORKER_MATRIX_TOKEN", None)
        os.environ.pop("AGENTTEAMS_MATRIX_TOKEN", None)
        os.environ.pop("WORKERFLOW_MATRIX_TOKEN", None)
        os.environ.pop("AGENTTEAMS_MATRIX_USER_ID", None)
        self.server = load_server()

    def tearDown(self) -> None:
        for key, value in self.old_env.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        self.tmp.cleanup()

    def test_api_base_argument_must_be_loopback(self) -> None:
        self.assertEqual(
            self.server._api_base("http://127.0.0.1:8088"),
            "http://127.0.0.1:8088/api",
        )
        self.assertEqual(
            self.server._api_base("http://localhost:8088/api/"),
            "http://localhost:8088/api",
        )

        with self.assertRaisesRegex(ValueError, "loopback"):
            self.server._api_base("http://169.254.169.254/latest")

    def test_shared_dir_survives_temp_workspace_cleanup(self) -> None:
        agent_id = "tmp-workerflow-test"
        workspace = self.root / ".qwenpaw" / "workspaces" / agent_id
        shared = self.server._resolve_shared_dir("", "run-abc", agent_id)

        result = self.server._setup_shared_dir(agent_id, workspace, shared)

        self.assertTrue((shared / "inputs").is_dir())
        self.assertTrue((shared / "outputs" / agent_id).is_dir())
        self.assertEqual(result["path"], str(shared.resolve()))

        metadata = json.loads((workspace / ".workerflow" / "shared.json").read_text(encoding="utf-8"))
        self.assertEqual(metadata["agentId"], agent_id)
        self.assertEqual(metadata["sharedPath"], str(shared.resolve()))
        self.assertEqual(metadata["inputsPath"], str((shared / "inputs").resolve()))
        self.assertEqual(metadata["outputPath"], str((shared / "outputs" / agent_id).resolve()))

        link = workspace / "shared"
        self.assertTrue(link.is_symlink())
        self.assertEqual(link.resolve(), shared.resolve())

        cleanup = self.server._safe_cleanup_workspace(agent_id, str(workspace))

        self.assertTrue(cleanup["removed"])
        self.assertFalse(workspace.exists())
        self.assertTrue(shared.exists())

    def test_cleanup_shared_only_removes_workerflow_shared_root_children(self) -> None:
        outside = self.root / "outside"
        outside.mkdir()

        rejected = self.server._safe_cleanup_shared(outside)

        self.assertFalse(rejected["removed"])
        self.assertEqual(rejected["reason"], "shared_dir_outside_workerflow_root")
        self.assertTrue(outside.exists())

        shared = self.server._resolve_shared_dir("", "run-clean", "tmp-workerflow-test")
        (shared / "inputs").mkdir(parents=True)

        removed = self.server._safe_cleanup_shared(shared)

        self.assertTrue(removed["removed"])
        self.assertFalse(shared.exists())

    def test_workflow_start_and_update_send_matrix_card(self) -> None:
        runtime = self.root / "runtime.yaml"
        runtime.write_text(
            "\n".join(
                [
                    "team:",
                    "  teamRoomId: '!team:example.test'",
                    "member:",
                    "  matrixUserId: '@worker:example.test'",
                    "  personalRoomId: '!personal:example.test'",
                ],
            )
            + "\n",
            encoding="utf-8",
        )
        os.environ["TEAMHARNESS_RUNTIME_CONFIG"] = str(runtime)
        os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"] = "test-token"

        captured: list[dict[str, object]] = []

        def fake_send(room_id: str, content: dict[str, object]) -> str:
            captured.append({"roomId": room_id, "content": content})
            return f"$event{len(captured)}"

        self.server._matrix_send_content = fake_send

        started = self.server._agentflow(
            {
                "action": "workflow_start",
                "runId": "run-ui",
                "roomId": "!dm:example.test",
                "title": "WorkerFlow Review",
                "summary": "creating subagents",
                "subagents": [
                    {"agentId": "tmp-a", "role": "review", "status": "running", "summary": "started"},
                ],
            },
        )

        self.assertTrue(started["ok"])
        self.assertEqual(started["eventId"], "$event1")
        self.assertEqual(started["roomId"], "!dm:example.test")
        first = captured[0]
        self.assertEqual(first["roomId"], "!dm:example.test")
        first_body = first["content"]
        self.assertEqual(first_body["msgtype"], "m.notice")
        self.assertEqual(first_body["hiclaw.workflow"]["coordinator"], "@worker:example.test")
        self.assertEqual(first_body["hiclaw.workflow"]["subagents"][0]["agentId"], "tmp-a")
        self.assertNotIn("helpers", first_body["hiclaw.workflow"])
        self.assertIn("<table>", first_body["formatted_body"])

        state_path = Path(started["statePath"])
        state = json.loads(state_path.read_text(encoding="utf-8"))
        self.assertEqual(state["eventId"], "$event1")
        self.assertEqual(state["roomId"], "!dm:example.test")

        updated = self.server._agentflow(
            {
                "action": "workflow_update",
                "runId": "run-ui",
                "status": "merging",
                "summary": "subagent result ready",
                "subagents": [
                    {"agentId": "tmp-a", "role": "review", "status": "done", "summary": "ok"},
                ],
            },
        )

        self.assertTrue(updated["ok"])
        self.assertEqual(updated["eventId"], "$event1")
        self.assertEqual(updated["editEventId"], "$event2")
        second_body = captured[1]["content"]
        self.assertEqual(second_body["m.relates_to"]["rel_type"], "m.replace")
        self.assertEqual(second_body["m.relates_to"]["event_id"], "$event1")
        self.assertEqual(second_body["m.new_content"]["hiclaw.workflow"]["status"], "merging")
        self.assertNotIn("helpers", second_body["m.new_content"]["hiclaw.workflow"])
        updated_state = json.loads(state_path.read_text(encoding="utf-8"))
        self.assertEqual(updated_state["lastEditEventId"], "$event2")

    def test_workflow_update_syncs_step_status_to_subagents_and_renders_step_id(self) -> None:
        runtime = self.root / "runtime-subagents.yaml"
        runtime.write_text(
            "\n".join(
                [
                    "member:",
                    "  matrixUserId: '@worker:example.test'",
                ],
            )
            + "\n",
            encoding="utf-8",
        )
        os.environ["TEAMHARNESS_RUNTIME_CONFIG"] = str(runtime)
        os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"] = "test-token"

        captured: list[dict[str, object]] = []

        def fake_send(room_id: str, content: dict[str, object]) -> str:
            captured.append({"roomId": room_id, "content": content})
            return f"$event{len(captured)}"

        self.server._matrix_send_content = fake_send

        started = self.server._agentflow(
            {
                "action": "workflow_start",
                "runId": "run-sync",
                "roomId": "!dm:example.test",
                "title": "Pig diagnosis",
                "summary": "subagents ready",
                "subagents": [
                    {
                        "id": "disease_analysis",
                        "agentId": "tmp-workerflow-run-sync-disease_analysis",
                        "role": "disease-analysis-agent",
                        "status": "waiting",
                        "summary": "waiting for base_info",
                    },
                ],
            },
        )

        self.assertTrue(started["ok"])
        state_path = Path(started["statePath"])

        updated = self.server._agentflow(
            {
                "action": "workflow_update",
                "runId": "run-sync",
                "status": "running",
                "summary": "disease analysis done",
                "steps": [
                    {
                        "id": "disease_analysis",
                        "status": "done",
                        "summary": "风险HIGH",
                    },
                ],
            },
        )

        self.assertTrue(updated["ok"])
        workflow = captured[1]["content"]["m.new_content"]["hiclaw.workflow"]
        self.assertEqual(workflow["subagents"][0]["status"], "done")
        self.assertEqual(workflow["subagents"][0]["summary"], "风险HIGH")
        self.assertNotIn("helpers", workflow)
        formatted = captured[1]["content"]["m.new_content"]["formatted_body"]
        self.assertIn("<h4>Subagents</h4>", formatted)
        self.assertIn("<td>disease_analysis</td><td>done</td><td>风险HIGH</td>", formatted)
        updated_state = json.loads(state_path.read_text(encoding="utf-8"))
        self.assertEqual(updated_state["subagents"][0]["status"], "done")
        self.assertNotIn("helpers", updated_state)

    def test_workflow_run_creates_subagents_and_submit_instructions(self) -> None:
        subagent = self.root / ".qwenpaw" / "workspaces" / "default" / "subagents" / "security-reviewer"
        subagent.mkdir(parents=True)
        (subagent / "AGENTS.md").write_text("Review security only.\n", encoding="utf-8")

        captured_matrix: list[dict[str, object]] = []
        requests: list[dict[str, object]] = []

        def fake_send(room_id: str, content: dict[str, object]) -> str:
            captured_matrix.append({"roomId": room_id, "content": content})
            return f"$event{len(captured_matrix)}"

        def fake_json_request(method: str, _base_url: str, path: str, payload: dict[str, object] | None = None) -> dict[str, object]:
            requests.append({"method": method, "path": path, "payload": payload})
            if method == "POST" and path == "/agents":
                assert payload is not None
                return dict(payload)
            raise AssertionError(f"unexpected request: {method} {path}")

        self.server._matrix_send_content = fake_send
        self.server._json_request = fake_json_request

        result = self.server._agentflow(
            {
                "action": "workflow_run",
                "runId": "run-dyn",
                "roomId": "!dm:example.test",
                "title": "Security review",
                "input": "Review auth.py",
                "subagents": [
                    {
                        "id": "security",
                        "subagent": "security-reviewer",
                        "task": "检查鉴权和权限边界风险",
                    },
                ],
            },
        )

        self.assertTrue(result["ok"])
        self.assertEqual(result["action"], "workflow_run")
        self.assertEqual(result["runId"], "run-dyn")
        self.assertEqual(len(requests), 1)
        payload = requests[0]["payload"]
        self.assertEqual(payload["id"], "tmp-workerflow-run-dyn-security")
        self.assertEqual(result["subagents"][0]["agentId"], "tmp-workerflow-run-dyn-security")
        self.assertEqual(result["subagents"][0]["status"], "ready")
        self.assertIn("submitPrompt", result["subagents"][0])
        self.assertIn("Review auth.py", result["subagents"][0]["submitPrompt"])
        self.assertIn("检查鉴权和权限边界风险", result["subagents"][0]["submitPrompt"])
        self.assertNotIn("helpers", result)

        self.assertEqual(captured_matrix[0]["roomId"], "!dm:example.test")
        self.assertEqual(captured_matrix[0]["content"]["hiclaw.workflow"]["status"], "spawning")
        workflow = captured_matrix[1]["content"]["m.new_content"]["hiclaw.workflow"]
        self.assertEqual(workflow["status"], "ready")
        self.assertEqual(workflow["subagents"][0]["agentId"], "tmp-workerflow-run-dyn-security")
        self.assertNotIn("helpers", workflow)

        state = json.loads(Path(result["statePath"]).read_text(encoding="utf-8"))
        self.assertEqual(state["status"], "ready")
        self.assertEqual(state["subagents"][0]["status"], "ready")
        self.assertEqual(state["subagents"][0]["subagent"], "security-reviewer")
        self.assertNotIn("helpers", state)

    def test_workflow_finish_deletes_workflow_temp_agents_without_workspace_cleanup(self) -> None:
        subagents_root = self.root / ".qwenpaw" / "workspaces" / "default" / "subagents"
        for name in ("base-info-agent", "report-agent"):
            path = subagents_root / name
            path.mkdir(parents=True)
            (path / "AGENTS.md").write_text(f"{name}\n", encoding="utf-8")

        captured_matrix: list[dict[str, object]] = []
        requests: list[dict[str, object]] = []

        def fake_send(room_id: str, content: dict[str, object]) -> str:
            captured_matrix.append({"roomId": room_id, "content": content})
            return f"$event{len(captured_matrix)}"

        def fake_json_request(method: str, _base_url: str, path: str, payload: dict[str, object] | None = None) -> dict[str, object]:
            requests.append({"method": method, "path": path, "payload": payload})
            if method == "POST" and path == "/agents":
                assert payload is not None
                return dict(payload)
            if method == "DELETE" and path.startswith("/agents/"):
                return {"deleted": path.rsplit("/", 1)[-1]}
            raise AssertionError(f"unexpected request: {method} {path}")

        self.server._matrix_send_content = fake_send
        self.server._json_request = fake_json_request

        started = self.server._agentflow(
            {
                "action": "workflow_run",
                "runId": "run-cleanup",
                "roomId": "!dm:example.test",
                "title": "Cleanup check",
                "input": "Generate report",
                "nodes": [
                    {
                        "id": "base_info",
                        "subagent": "base-info-agent",
                        "task": "整理基础信息",
                    },
                    {
                        "id": "report",
                        "subagent": "report-agent",
                        "task": "生成报告",
                        "dependsOn": ["base_info"],
                    },
                ],
            },
        )

        finished = self.server._agentflow(
            {
                "action": "workflow_finish",
                "runId": "run-cleanup",
                "summary": "done",
            },
        )

        self.assertTrue(started["ok"])
        self.assertTrue(finished["ok"])
        deleted_paths = [request["path"] for request in requests if request["method"] == "DELETE"]
        self.assertEqual(
            deleted_paths,
            [
                "/agents/tmp-workerflow-run-cleanup-base_info",
                "/agents/tmp-workerflow-run-cleanup-report",
            ],
        )
        cleanup = finished["cleanupTempAgents"]
        self.assertEqual(cleanup["deleted"], 2)
        self.assertEqual(cleanup["failed"], 0)
        self.assertEqual([item["cleanup"]["reason"] for item in cleanup["agents"]], ["cleanup_disabled", "cleanup_disabled"])
        state = json.loads(Path(finished["statePath"]).read_text(encoding="utf-8"))
        self.assertEqual(state["cleanupTempAgents"]["deleted"], 2)
        self.assertEqual(captured_matrix[-1]["content"]["m.new_content"]["hiclaw.workflow"]["status"], "done")

    def test_workflow_fail_deletes_workflow_temp_agents(self) -> None:
        shared_dir = self.server._resolve_shared_dir("", "run-fail-cleanup", "run-fail-cleanup")
        self.server._write_json(
            self.server._workflow_state_path(shared_dir),
            {
                "runId": "run-fail-cleanup",
                "roomId": "!dm:example.test",
                "eventId": "$original",
                "title": "Fail cleanup",
                "status": "running",
                "sharedPath": str(shared_dir),
                "subagents": [
                    {
                        "agentId": "tmp-workerflow-run-fail-cleanup-report",
                        "status": "running",
                    },
                ],
            },
        )

        deleted_paths: list[str] = []

        def fake_send(_room_id: str, _content: dict[str, object]) -> str:
            return "$edit"

        def fake_json_request(method: str, _base_url: str, path: str, _payload: dict[str, object] | None = None) -> dict[str, object]:
            if method == "DELETE" and path.startswith("/agents/"):
                deleted_paths.append(path)
                return {}
            raise AssertionError(f"unexpected request: {method} {path}")

        self.server._matrix_send_content = fake_send
        self.server._json_request = fake_json_request

        failed = self.server._agentflow(
            {
                "action": "workflow_fail",
                "runId": "run-fail-cleanup",
                "summary": "failed",
            },
        )

        self.assertTrue(failed["ok"])
        self.assertEqual(deleted_paths, ["/agents/tmp-workerflow-run-fail-cleanup-report"])
        self.assertEqual(failed["cleanupTempAgents"]["deleted"], 1)
        state = json.loads(Path(failed["statePath"]).read_text(encoding="utf-8"))
        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["cleanupTempAgents"]["failed"], 0)

    def test_workflow_run_failure_marks_failed_and_deletes_created_temp_agents(self) -> None:
        subagents_root = self.root / ".qwenpaw" / "workspaces" / "default" / "subagents"
        for name in ("base-info-agent", "report-agent"):
            path = subagents_root / name
            path.mkdir(parents=True)
            (path / "AGENTS.md").write_text(f"{name}\n", encoding="utf-8")

        captured_matrix: list[dict[str, object]] = []
        requests: list[dict[str, object]] = []

        def fake_send(room_id: str, content: dict[str, object]) -> str:
            captured_matrix.append({"roomId": room_id, "content": content})
            return f"$event{len(captured_matrix)}"

        def fake_json_request(method: str, _base_url: str, path: str, payload: dict[str, object] | None = None) -> dict[str, object]:
            requests.append({"method": method, "path": path, "payload": payload})
            if method == "POST" and path == "/agents":
                assert payload is not None
                if payload["id"] == "tmp-workerflow-run-spawn-fail-report":
                    raise RuntimeError("create failed")
                return dict(payload)
            if method == "DELETE" and path.startswith("/agents/"):
                return {}
            raise AssertionError(f"unexpected request: {method} {path}")

        self.server._matrix_send_content = fake_send
        self.server._json_request = fake_json_request

        with self.assertRaisesRegex(RuntimeError, "create failed"):
            self.server._agentflow(
                {
                    "action": "workflow_run",
                    "runId": "run-spawn-fail",
                    "roomId": "!dm:example.test",
                    "title": "Spawn failure",
                    "input": "Generate report",
                    "nodes": [
                        {
                            "id": "base_info",
                            "subagent": "base-info-agent",
                            "task": "整理基础信息",
                        },
                        {
                            "id": "report",
                            "subagent": "report-agent",
                            "task": "生成报告",
                        },
                    ],
                },
            )

        deleted_paths = [request["path"] for request in requests if request["method"] == "DELETE"]
        self.assertEqual(deleted_paths, ["/agents/tmp-workerflow-run-spawn-fail-base_info"])
        self.assertEqual(captured_matrix[0]["content"]["hiclaw.workflow"]["status"], "spawning")
        failed_workflow = captured_matrix[-1]["content"]["m.new_content"]["hiclaw.workflow"]
        self.assertEqual(failed_workflow["status"], "failed")
        self.assertEqual(failed_workflow["subagents"][0]["agentId"], "tmp-workerflow-run-spawn-fail-base_info")
        state_path = self.server._workflow_state_path(
            self.server._resolve_shared_dir("", "run-spawn-fail", "run-spawn-fail")
        )
        state = json.loads(state_path.read_text(encoding="utf-8"))
        self.assertEqual(state["status"], "failed")
        self.assertEqual(state["cleanupTempAgents"]["deleted"], 1)

    def test_workflow_run_failure_after_remote_create_deletes_current_temp_agent(self) -> None:
        subagent = self.root / ".qwenpaw" / "workspaces" / "default" / "subagents" / "base-info-agent"
        subagent.mkdir(parents=True)
        (subagent / "AGENTS.md").write_text("base-info-agent\n", encoding="utf-8")

        captured_matrix: list[dict[str, object]] = []
        requests: list[dict[str, object]] = []

        def fake_send(room_id: str, content: dict[str, object]) -> str:
            captured_matrix.append({"roomId": room_id, "content": content})
            return f"$event{len(captured_matrix)}"

        def fake_json_request(method: str, _base_url: str, path: str, payload: dict[str, object] | None = None) -> dict[str, object]:
            requests.append({"method": method, "path": path, "payload": payload})
            if method == "POST" and path == "/agents":
                assert payload is not None
                return dict(payload)
            if method == "DELETE" and path.startswith("/agents/"):
                return {}
            raise AssertionError(f"unexpected request: {method} {path}")

        def fail_copy_template(_template_path: str, _workspace_dir: Path) -> dict[str, object]:
            raise RuntimeError("copy failed")

        self.server._matrix_send_content = fake_send
        self.server._json_request = fake_json_request
        self.server._copy_template = fail_copy_template

        with self.assertRaisesRegex(RuntimeError, "copy failed"):
            self.server._agentflow(
                {
                    "action": "workflow_run",
                    "runId": "run-init-fail",
                    "roomId": "!dm:example.test",
                    "title": "Init failure",
                    "input": "Collect info",
                    "nodes": [
                        {
                            "id": "base_info",
                            "subagent": "base-info-agent",
                            "task": "整理基础信息",
                        },
                    ],
                },
            )

        deleted_paths = [request["path"] for request in requests if request["method"] == "DELETE"]
        self.assertEqual(deleted_paths, ["/agents/tmp-workerflow-run-init-fail-base_info"])
        failed_workflow = captured_matrix[-1]["content"]["m.new_content"]["hiclaw.workflow"]
        self.assertEqual(failed_workflow["status"], "failed")
        self.assertEqual(failed_workflow["subagents"][0]["agentId"], "tmp-workerflow-run-init-fail-base_info")
        state_path = self.server._workflow_state_path(
            self.server._resolve_shared_dir("", "run-init-fail", "run-init-fail")
        )
        state = json.loads(state_path.read_text(encoding="utf-8"))
        self.assertEqual(state["cleanupTempAgents"]["deleted"], 1)

    def test_workflow_run_supports_dag_nodes_with_waiting_dependencies(self) -> None:
        subagents_root = self.root / ".qwenpaw" / "workspaces" / "default" / "subagents"
        for name in ("base-info-agent", "pathogen-agent", "disease-analysis-agent"):
            path = subagents_root / name
            path.mkdir(parents=True)
            (path / "AGENTS.md").write_text(f"{name}\n", encoding="utf-8")

        captured_matrix: list[dict[str, object]] = []
        requests: list[dict[str, object]] = []

        def fake_send(room_id: str, content: dict[str, object]) -> str:
            captured_matrix.append({"roomId": room_id, "content": content})
            return f"$event{len(captured_matrix)}"

        def fake_json_request(method: str, _base_url: str, path: str, payload: dict[str, object] | None = None) -> dict[str, object]:
            requests.append({"method": method, "path": path, "payload": payload})
            if method == "POST" and path == "/agents":
                assert payload is not None
                return dict(payload)
            raise AssertionError(f"unexpected request: {method} {path}")

        self.server._matrix_send_content = fake_send
        self.server._json_request = fake_json_request

        result = self.server._agentflow(
            {
                "action": "workflow_run",
                "runId": "pig-diagnosis",
                "roomId": "!dm:example.test",
                "title": "猪病诊断报告生成",
                "input": "批次号 B001",
                "nodes": [
                    {
                        "id": "base_info",
                        "name": "基础信息梳理",
                        "role": "基础信息梳理",
                        "subagent": "base-info-agent",
                        "task": "处理基础信息",
                    },
                    {
                        "id": "pathogen",
                        "name": "病原线索分析",
                        "role": "病原线索分析",
                        "subagent": "pathogen-agent",
                        "task": "分析病原",
                    },
                    {
                        "id": "disease_analysis",
                        "name": "疾病综合研判",
                        "role": "疾病综合研判",
                        "subagent": "disease-analysis-agent",
                        "task": "综合诊断猪病",
                        "dependsOn": ["base_info", "pathogen"],
                    },
                ],
            },
        )

        self.assertTrue(result["ok"])
        self.assertEqual(len(requests), 3)
        self.assertEqual(requests[0]["payload"]["name"], "基础信息梳理")
        self.assertEqual(
            [request["payload"]["id"] for request in requests],
            [
                "tmp-workerflow-pig-diagnosis-base_info",
                "tmp-workerflow-pig-diagnosis-pathogen",
                "tmp-workerflow-pig-diagnosis-disease_analysis",
            ],
        )
        self.assertEqual([node["status"] for node in result["nodes"]], ["ready", "ready", "waiting"])
        self.assertEqual([item["id"] for item in result["submitInstructions"]], ["base_info", "pathogen"])
        self.assertEqual([item["id"] for item in result["waitingInstructions"]], ["disease_analysis"])
        waiting_prompt = result["waitingInstructions"][0]["submitPrompt"]
        self.assertIn("Upstream outputs:", waiting_prompt)
        self.assertIn("base_info", waiting_prompt)
        self.assertIn("pathogen", waiting_prompt)

        workflow = captured_matrix[1]["content"]["m.new_content"]["hiclaw.workflow"]
        self.assertEqual(workflow["subagents"][0]["name"], "基础信息梳理")
        self.assertEqual(workflow["subagents"][0]["role"], "基础信息梳理")
        self.assertEqual(workflow["subagents"][2]["status"], "waiting")
        self.assertNotIn("helpers", workflow)
        formatted = captured_matrix[1]["content"]["m.new_content"]["formatted_body"]
        self.assertIn("<td>基础信息梳理</td><td>基础信息梳理</td><td>ready</td>", formatted)

        state = json.loads(Path(result["statePath"]).read_text(encoding="utf-8"))
        self.assertEqual(state["nodes"][2]["dependsOn"], ["base_info", "pathogen"])
        self.assertEqual(len(state["submitInstructions"]), 2)
        self.assertEqual(len(state["waitingInstructions"]), 1)

    def test_workflow_update_unblocks_dag_nodes_after_dependency_steps_done(self) -> None:
        subagents_root = self.root / ".qwenpaw" / "workspaces" / "default" / "subagents"
        for name in ("base-info-agent", "pathogen-agent", "disease-analysis-agent"):
            path = subagents_root / name
            path.mkdir(parents=True)
            (path / "AGENTS.md").write_text(f"{name}\n", encoding="utf-8")

        captured_matrix: list[dict[str, object]] = []

        def fake_send(room_id: str, content: dict[str, object]) -> str:
            captured_matrix.append({"roomId": room_id, "content": content})
            return f"$event{len(captured_matrix)}"

        def fake_json_request(method: str, _base_url: str, path: str, payload: dict[str, object] | None = None) -> dict[str, object]:
            if method == "POST" and path == "/agents":
                assert payload is not None
                return dict(payload)
            raise AssertionError(f"unexpected request: {method} {path}")

        self.server._matrix_send_content = fake_send
        self.server._json_request = fake_json_request

        started = self.server._agentflow(
            {
                "action": "workflow_run",
                "runId": "run-advance",
                "roomId": "!dm:example.test",
                "title": "猪病诊断报告生成",
                "input": "批次号 B001",
                "nodes": [
                    {
                        "id": "base_info",
                        "subagent": "base-info-agent",
                        "task": "处理基础信息",
                    },
                    {
                        "id": "pathogen",
                        "subagent": "pathogen-agent",
                        "task": "分析病原",
                    },
                    {
                        "id": "disease_analysis",
                        "subagent": "disease-analysis-agent",
                        "task": "综合诊断猪病",
                        "dependsOn": ["base_info", "pathogen"],
                    },
                ],
            },
        )

        self.assertEqual([item["id"] for item in started["waitingInstructions"]], ["disease_analysis"])

        updated = self.server._agentflow(
            {
                "action": "workflow_update",
                "runId": "run-advance",
                "status": "running",
                "summary": "upstream done",
                "steps": [
                    {"id": "base_info", "status": "done", "summary": "基础信息完成"},
                    {"id": "pathogen", "status": "done", "summary": "病原分析完成"},
                ],
            },
        )

        self.assertTrue(updated["ok"])
        self.assertEqual([item["id"] for item in updated["readyInstructions"]], ["disease_analysis"])
        ready_prompt = updated["readyInstructions"][0]["submitPrompt"]
        self.assertIn("Upstream summaries:", ready_prompt)
        self.assertIn("base_info: 基础信息完成", ready_prompt)
        self.assertIn("pathogen: 病原分析完成", ready_prompt)

        workflow = captured_matrix[2]["content"]["m.new_content"]["hiclaw.workflow"]
        self.assertEqual(workflow["subagents"][2]["status"], "ready")
        self.assertEqual(workflow["subagents"][2]["summary"], "dependencies done; ready for submit")

        state = json.loads(Path(updated["statePath"]).read_text(encoding="utf-8"))
        self.assertEqual([item["id"] for item in state["waitingInstructions"]], [])
        self.assertEqual([item["id"] for item in state["readyInstructions"]], ["disease_analysis"])
        self.assertEqual([node["status"] for node in state["nodes"]], ["done", "done", "ready"])

    def test_workflow_run_dry_run_returns_plan_without_side_effects(self) -> None:
        subagent = self.root / ".qwenpaw" / "workspaces" / "default" / "subagents" / "test-reviewer"
        subagent.mkdir(parents=True)
        (subagent / "AGENTS.md").write_text("Review tests only.\n", encoding="utf-8")

        def fail_send(_room_id: str, _content: dict[str, object]) -> str:
            raise AssertionError("dry run should not send matrix messages")

        def fail_json_request(_method: str, _base_url: str, _path: str, _payload: dict[str, object] | None = None) -> dict[str, object]:
            raise AssertionError("dry run should not call qwenpaw api")

        self.server._matrix_send_content = fail_send
        self.server._json_request = fail_json_request

        result = self.server._agentflow(
            {
                "action": "workflow_run",
                "runId": "run-plan",
                "roomId": "!dm:example.test",
                "title": "Test review",
                "input": "Review tests/test_auth.py",
                "subagents": [
                    {
                        "id": "tests",
                        "subagent": "test-reviewer",
                        "task": "检查测试覆盖缺口",
                    },
                ],
                "dryRun": True,
            },
        )

        self.assertTrue(result["ok"])
        self.assertTrue(result["dryRun"])
        self.assertEqual(result["subagents"][0]["agentId"], "tmp-workerflow-run-plan-tests")
        self.assertNotIn("helpers", result)
        self.assertIn("Review tests/test_auth.py", result["submitInstructions"][0]["submitPrompt"])
        self.assertFalse(Path(result["statePath"]).exists())

    def test_workflow_start_requires_explicit_room_id_without_fallback(self) -> None:
        runtime = self.root / "runtime-with-rooms.yaml"
        runtime.write_text(
            "\n".join(
                [
                    "team:",
                    "  teamRoomId: '!team:example.test'",
                    "member:",
                    "  personalRoomId: '!personal:example.test'",
                ],
            )
            + "\n",
            encoding="utf-8",
        )
        os.environ["TEAMHARNESS_RUNTIME_CONFIG"] = str(runtime)

        with self.assertRaisesRegex(ValueError, "roomId is required"):
            self.server._agentflow(
                {
                    "action": "workflow_start",
                    "runId": "run-no-room",
                    "dryRun": True,
                },
            )

    def test_workflow_update_dry_run_builds_replace_content(self) -> None:
        dry = self.server._agentflow(
            {
                "action": "workflow_update",
                "runId": "run-dry",
                "roomId": "!room:example.test",
                "eventId": "$original",
                "status": "running",
                "summary": "checking",
                "dryRun": True,
            },
        )

        self.assertTrue(dry["ok"])
        self.assertTrue(dry["dryRun"])
        self.assertEqual(dry["content"]["msgtype"], "m.notice")
        self.assertEqual(dry["content"]["m.relates_to"]["event_id"], "$original")
        self.assertEqual(dry["content"]["m.new_content"]["hiclaw.workflow"]["runId"], "run-dry")

    def test_matrix_env_aliases_and_runtime_token_env(self) -> None:
        runtime = self.root / "runtime-token.yaml"
        runtime.write_text(
            "\n".join(
                [
                    "credentials:",
                    "  matrixTokenEnv: WORKERFLOW_MATRIX_TOKEN",
                ],
            )
            + "\n",
            encoding="utf-8",
        )
        os.environ["TEAMHARNESS_RUNTIME_CONFIG"] = str(runtime)
        os.environ["AGENTTEAMS_MATRIX_SERVER"] = "http://matrix.example.test"
        os.environ["WORKERFLOW_MATRIX_TOKEN"] = "custom-token"

        self.assertEqual(self.server._matrix_homeserver(), "http://matrix.example.test")
        self.assertEqual(self.server._matrix_token(), "custom-token")


if __name__ == "__main__":
    unittest.main(verbosity=2)
