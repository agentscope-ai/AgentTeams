#!/usr/bin/env python3
"""Split TeamHarness MCP projectflow/taskflow/filesync into tool modules (Phase 5)."""

from __future__ import annotations

import ast
import textwrap
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
SERVER = REPO / "plugins" / "teamharness" / "mcp" / "server.py"
MCP = REPO / "plugins" / "teamharness" / "mcp"

COMMON_FUNCS = {
    "_payload",
    "_safe_id",
    "_slugify",
    "_project_timestamp",
    "_unique_project_id",
    "_project_id_from_payload",
    "_normalize_reply_route",
    "_reply_route_from_requester",
    "_source_room_id_from_payload",
    "_canonical_room_id",
    "_external_requester_channel",
    "_validate_assignment_room",
    "_validate_task_redelegation",
    "_read_json",
    "_write_json",
    "_project_dir",
    "_task_dir",
    "_project_state_path",
    "_task_state_path",
    "_normalize_task",
    "_validate_task_graph",
    "_positive_int",
    "_non_negative_int",
    "_safe_loop_status",
    "_safe_loop_decision",
    "_write_project_plan",
    "_ready_nodes",
    "_ready_loop_nodes",
    "_resolve_project",
    "_accepted_node_status",
    "_payload_bool",
    "_payload_bool_field",
    "_accept_task_result",
    "_mark_requester_report_sent",
    "_notification_needed",
    "_normalize_role",
    "_runtime_role",
    "_role",
    "_load_task",
    "_first_text",
    "_utc_timestamp",
    "_project_task_for_meta",
    "_preserve_task_meta_fields",
    "_ensure_console_task_meta",
    "_write_task",
    "_validate_task_deliverables",
    "_task_result_from_meta",
    "_sync_task",
    "_pull_task",
    "_terminal_task_status",
    "_require_task_mutable",
    "_update_project_task",
    "_workspace_dir",
    "_optional_workspace_dir",
    "_default_workspace_dir",
    "_default_shared_prefix",
    "_default_global_shared_prefix",
    "_remote_root",
    "_load_runtime_config",
    "_section",
    "_runtime_team_room_id",
}

FILESYNC_FUNCS = {
    "_normalize_exclude",
    "_normalize_shared_path",
    "_resolve_filesync",
    "_filesync",
    "_filesync_command_error",
    "_filesync_mc_env",
    "_remote_uses_mc_alias",
    "_mc_alias_configured",
    "_mc_host_url",
    "_storage_root_prefix",
}

SERVER_DELEGATES = {
    "_matrix_target",
    "_publish_task_artifacts",
    "_publish_project_artifacts",
    "_attachment_parent_event_id",
    "_normalize_workspace_artifact_path",
    "_path_is_under",
}


def _extract_functions(source: str, names: set[str]) -> dict[str, ast.FunctionDef]:
    tree = ast.parse(source)
    out: dict[str, ast.FunctionDef] = {}
    for node in tree.body:
        if isinstance(node, ast.FunctionDef) and node.name in names:
            out[node.name] = node
    missing = names - set(out)
    if missing:
        raise SystemExit(f"missing functions in server.py: {sorted(missing)}")
    return out


def _render_func(node: ast.FunctionDef, source: str) -> str:
    segment = ast.get_source_segment(source, node)
    if not segment:
        raise SystemExit(f"could not render {node.name}")
    return segment + "\n\n"


def _common_header() -> str:
    return textwrap.dedent(
        '''\
        """Shared TeamHarness MCP project/task protocol helpers."""

        from __future__ import annotations

        import datetime
        import json
        import os
        import re
        import time
        from pathlib import Path
        from typing import Any

        from common.runtime_config import load_runtime_config, section as _runtime_section
        from protocol_bridge import validate_task_graph as _protocol_validate_task_graph

        MC_ALIAS = "agentteams"
        ALLOWED_TASK_RESULT_STATUSES = {"SUCCESS", "SUCCESS_WITH_NOTES", "REVISION_NEEDED", "BLOCKED", "FAILED", "PARTIAL"}
        TERMINAL_TASK_STATUSES = {"completed", "revision", "blocked", "cancelled"}


        def _matrix_target(target: str) -> tuple[str, str]:
            import server as _server

            return _server._matrix_target(target)


        def _attachment_parent_event_id(*sources: dict[str, Any]) -> str:
            import server as _server

            return _server._attachment_parent_event_id(*sources)


        def _publish_task_artifacts(
            arguments: dict[str, Any],
            task: dict[str, Any],
            task_id: str,
            deliverables: list[Any],
            parent_event_id: str,
        ) -> list[dict[str, Any]]:
            import server as _server

            return _server._publish_task_artifacts(arguments, task, task_id, deliverables, parent_event_id)


        def _publish_project_artifacts(
            arguments: dict[str, Any],
            project: dict[str, Any],
            project_id: str,
            task_id: str,
            parent_event_id: str,
        ) -> list[dict[str, Any]]:
            import server as _server

            return _server._publish_project_artifacts(arguments, project, project_id, task_id, parent_event_id)


        def _normalize_workspace_artifact_path(raw_path: str) -> tuple[str, bool]:
            import server as _server

            return _server._normalize_workspace_artifact_path(raw_path)


        def _path_is_under(normalized: str, prefix: str) -> bool:
            import server as _server

            return _server._path_is_under(normalized, prefix)


        '''
    )


def _filesync_header() -> str:
    return textwrap.dedent(
        '''\
        """TeamHarness MCP filesync tool (mc subprocess; Phase 6 deferral for FileSync)."""

        from __future__ import annotations

        import json
        import os
        import subprocess
        import urllib.parse
        from pathlib import Path
        from typing import Any

        MC_ALIAS = "agentteams"

        DEFERRAL_NOTE = (
            "P5.7: MCP filesync still builds mc argv locally. "
            "Delegation to agentteams_sync.FileSync is deferred until SyncContract "
            "covers TeamHarness workspaceDir + global-shared read-only semantics."
        )


        '''
    )


def main() -> None:
    source = SERVER.read_text(encoding="utf-8")
    funcs = _extract_functions(source, COMMON_FUNCS | FILESYNC_FUNCS | {"_projectflow", "_taskflow"})

    common_parts = [_common_header()]
    for name in sorted(COMMON_FUNCS):
        common_parts.append(_render_func(funcs[name], source))
    (MCP / "mcp_common.py").write_text("".join(common_parts), encoding="utf-8")

    filesync_parts = [_filesync_header()]
    for name in sorted(FILESYNC_FUNCS):
        filesync_parts.append(_render_func(funcs[name], source))
    filesync_parts.append(
        textwrap.dedent(
            '''\

            def filesync(arguments: dict[str, Any]) -> dict[str, Any]:
                """MCP filesync entry; mc-based until Phase 6 shared sync cutover."""
                return _filesync(arguments)


            def filesync_deferral_note() -> str:
                return DEFERRAL_NOTE
            '''
        )
    )
    (MCP / "tools" / "filesync.py").write_text("".join(filesync_parts), encoding="utf-8")

    projectflow_body = _render_func(funcs["_projectflow"], source).replace(
        "def _projectflow(", "def projectflow("
    )
    projectflow_src = textwrap.dedent(
        '''\
        """TeamHarness MCP projectflow tool."""

        from __future__ import annotations

        from typing import Any

        import mcp_common as common
        from protocol_bridge import protocol_core_enabled


        def _maybe_dual_run(action: str, arguments: dict[str, Any], payload: dict[str, Any]) -> None:
            if not protocol_core_enabled():
                return
            # Phase 5: flag enables extra protocol validation hooks; full store dual-run in characterization.
            if action in {"plan_dag", "plan_loop"} and isinstance(payload.get("tasks"), list):
                common._validate_task_graph(
                    [
                        common._normalize_task(task)
                        for task in payload["tasks"]
                        if isinstance(task, dict)
                    ]
                )


        '''
    ) + projectflow_body.replace("common.", "common.").replace(
        "_payload(", "common._payload("
    )
    # Replace bare helper calls with common.* — mechanical rename for moved helpers
    replacements = sorted(COMMON_FUNCS, key=len, reverse=True)
    for name in replacements:
        bare = name + "("
        projectflow_src = projectflow_src.replace(bare, f"common.{bare}")
    projectflow_src = projectflow_src.replace("common.common.", "common.")
    projectflow_src = projectflow_src.replace(
        "def projectflow(arguments: dict[str, Any]) -> dict[str, Any]:",
        "def projectflow(arguments: dict[str, Any]) -> dict[str, Any]:",
        1,
    )
    # Fix double common on first lines inside projectflow
    for token in ("action =", "payload = common.common._payload"):
        pass
    projectflow_src = projectflow_src.replace("common.common.", "common.")
    (MCP / "tools" / "projectflow.py").write_text(projectflow_src, encoding="utf-8")

    taskflow_body = _render_func(funcs["_taskflow"], source).replace(
        "def _taskflow(", "def taskflow("
    )
    taskflow_src = textwrap.dedent(
        '''\
        """TeamHarness MCP taskflow tool."""

        from __future__ import annotations

        from typing import Any

        import mcp_common as common


        '''
    ) + taskflow_body
    for name in replacements:
        taskflow_src = taskflow_src.replace(name + "(", f"common.{name}(")
    taskflow_src = taskflow_src.replace("common.common.", "common.")
    (MCP / "tools" / "taskflow.py").write_text(taskflow_src, encoding="utf-8")

    # Patch server.py: remove moved functions, add thin re-exports
    tree = ast.parse(source)
    keep_nodes = []
    remove = COMMON_FUNCS | FILESYNC_FUNCS | {"_projectflow", "_taskflow"}
    for node in tree.body:
        if isinstance(node, ast.FunctionDef) and node.name in remove:
            continue
        keep_nodes.append(node)
    new_source = ast.unparse(ast.Module(body=keep_nodes, type_ignores=[]))
    # ast.unparse loses comments/formatting — use line-based removal instead
    lines = source.splitlines(keepends=True)
    new_lines: list[str] = []
    skip = False
    for line in lines:
        stripped = line.lstrip()
        if stripped.startswith("def ") and stripped[4:].split("(")[0] in remove:
            skip = True
            continue
        if skip:
            if stripped.startswith("def ") and not line.startswith(" " * 4):
                skip = False
            elif stripped.startswith("def ") and line.startswith("def "):
                skip = False
            else:
                continue
        if not skip:
            new_lines.append(line)

    # Fix botched skip: re-read and use AST line numbers
    func_lines: dict[str, tuple[int, int]] = {}
    for node in ast.parse(source).body:
        if isinstance(node, ast.FunctionDef) and node.name in remove:
            start = node.lineno - 1
            end = node.end_lineno or start + 1
            func_lines[node.name] = (start, end)

    skip_ranges = sorted(func_lines.values())
    merged: list[str] = []
    idx = 0
    for start, end in skip_ranges:
        merged.extend(lines[idx:start])
        idx = end
    merged.extend(lines[idx:])
    new_lines = merged

    text = "".join(new_lines)
    # Update imports at top
    if "from tools.projectflow import projectflow as _projectflow" not in text:
        text = text.replace(
            "from tools.projectflow import projectflow as _projectflow\n",
            "",
        )
    import_block = (
        "from tools.filesync import filesync as _filesync, filesync_deferral_note as _filesync_deferral_note\n"
        "from tools.projectflow import projectflow as _projectflow\n"
        "from tools.taskflow import taskflow as _taskflow\n"
    )
    if "from tools.filesync import filesync" not in text:
        text = text.replace(
            "from tools.filesync import filesync as _filesync\n",
            import_block,
        )
        if "from tools.filesync import filesync" not in text:
            text = text.replace(
                "from tools.projectflow import projectflow as _projectflow\nfrom tools.taskflow import taskflow as _taskflow\n",
                import_block,
            )

    # Remove duplicate old imports if present
    text = text.replace(
        "from tools.filesync import filesync as _filesync\nfrom tools.artifact import artifact as _artifact\nfrom tools.projectflow import projectflow as _projectflow\nfrom tools.taskflow import taskflow as _taskflow\n",
        "from tools.filesync import filesync as _filesync, filesync_deferral_note as _filesync_deferral_note\nfrom tools.artifact import artifact as _artifact\nfrom tools.projectflow import projectflow as _projectflow\nfrom tools.taskflow import taskflow as _taskflow\n",
    )

    # Add thin wrappers if _projectflow/_taskflow defs were removed but imports missing
    if "def _projectflow" not in text and "_projectflow = projectflow" not in text:
        pass  # imports alias directly

    SERVER.write_text(text, encoding="utf-8")
    print("Wrote mcp_common.py, tools/filesync.py, tools/projectflow.py, tools/taskflow.py")
    print("Patched server.py")


if __name__ == "__main__":
    main()
