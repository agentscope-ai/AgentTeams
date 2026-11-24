"""The common MCP surface keeps role/source configuration out of Agent input."""
import json
from pathlib import Path
import sys

sys.path.insert(0,str(Path(__file__).resolve().parents[3]/"teamharness/mcp"))
import server


def test_role_schemas_offer_only_relevant_task_operations(monkeypatch):
    for role,expected in {
        "manager":{"create","get","cancel","files_list","files_read","files_get"},
        "leader":{"create","plan","get","accept","reject","complete","cancel","files_list","files_read","files_get"},
        "worker":{"begin","get","submit","files_list","files_read","files_get","files_put"},
    }.items():
        monkeypatch.setattr(server,"_runtime_role",lambda role=role:role)
        tool=next(tool for tool in server.list_tools() if tool["name"]=="task")
        properties=tool["inputSchema"]["properties"]
        assert set(properties["action"]["enum"])==expected
        assert not {"_nativeSource","role","projectId","roomId","recipient","controllerURL","env","command"}.intersection(properties)
        assert tool["inputSchema"]["additionalProperties"] is False


def test_text_file_window_schema_uses_portable_integer_parameters(monkeypatch):
    monkeypatch.setattr(server, "_runtime_role", lambda: "worker")
    task = next(tool for tool in server.list_tools() if tool["name"] == "task")
    props = task["inputSchema"]["properties"]
    for field in ("offset", "limit"):
        assert props[field]["type"] == "integer"
        assert set(props[field]) <= {"type", "description"}


def test_task_result_is_preserved_and_errors_use_mcp_error_status(monkeypatch):
    monkeypatch.setattr(server,"_runtime_role",lambda:"leader")
    result={"task":{"id":"a","submissionId":"s1"},"review":{"id":"review"},"mainProjection":"pending"}
    calls=[]
    def task(arguments):calls.append(arguments);return result
    monkeypatch.setattr(server,"_task",task)
    arguments={"action":"accept","taskId":"a","submissionId":"s1","note":"Evidence"}
    response=server.call_tool("task",arguments)
    assert json.loads(response["content"][0]["text"])==result and calls==[arguments]
    monkeypatch.setattr(server,"_task",lambda args:{"ok":False,"error":"task_operation_failed"})
    assert server.call_tool("task",arguments)["isError"] is True
