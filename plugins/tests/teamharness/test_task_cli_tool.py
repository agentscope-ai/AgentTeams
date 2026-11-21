"""The shared tool delegates to argv-only CLI calls, never another state machine."""
import importlib.util
import json
import os
import sys
from pathlib import Path
from types import SimpleNamespace
from types import ModuleType

import pytest

SOURCE=Path(__file__).resolve().parents[3]/"plugins/teamharness/mcp/common/task_cli.py"


def load(monkeypatch):
    spec=importlib.util.spec_from_file_location("task_cli_test",SOURCE)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    monkeypatch.setattr(module.shutil,"which",lambda name:"/usr/local/bin/agt")
    return module


def test_create_does_not_require_model_to_copy_original_message(monkeypatch):
    tool = load(monkeypatch)
    argv, env = tool.command({"action": "create", "leader": "lead", "title": "Request",
        "_nativeSource": {"eventId": "$actual", "userId": "@human:test", "roomId": "!source:test"}})
    assert argv == ["/usr/local/bin/agt", "task", "create", "--leader", "lead", "--title", "Request"]
    assert env["AGENTTEAMS_ORIGIN_EVENT_ID"] == "$actual"


def test_rejection_can_request_explicit_replacement_without_shell(monkeypatch):
    tool = load(monkeypatch)
    argv, _ = tool.command({"action":"reject", "taskId":"t1", "submissionId":"s1", "note":"QA failed", "replacementSpec":"Fix cents; literal $(text)"})
    assert argv == ["/usr/local/bin/agt", "task", "reject", "t1", "--submission", "s1", "--note", "QA failed", "--replacement-spec", "Fix cents; literal $(text)"]
    with pytest.raises(ValueError):
        tool.command({"action":"accept", "taskId":"t1", "submissionId":"s1", "note":"OK", "replacementSpec":"Fix"})


def test_create_uses_native_source_in_private_process_environment(monkeypatch):
    tool=load(monkeypatch)
    monkeypatch.setenv("AGENTTEAMS_ORIGIN_EVENT_ID","other-conversation")
    result={"sourceEventId":"$actual","task":{"id":"main"}}
    def execute(argv,**kwargs):
        assert argv==["/usr/local/bin/agt","task","create","--leader","lead","--title","Request","--spec","literal $(touch forbidden); `text`"]
        assert kwargs["env"]["AGENTTEAMS_ORIGIN_EVENT_ID"]=="$actual"
        assert kwargs["env"]["AGENTTEAMS_REQUESTER_ROOM_ID"]=="!source:test"
        assert "shell" not in kwargs
        return SimpleNamespace(returncode=0,stdout=json.dumps(result),stderr="")
    monkeypatch.setattr(tool.subprocess,"run",execute)
    assert tool.run({"action":"create","leader":"lead","title":"Request","spec":"literal $(touch forbidden); `text`",
                     "_nativeSource":{"eventId":"$actual","userId":"@human:test","roomId":"!source:test"}})==result
    assert os.environ["AGENTTEAMS_ORIGIN_EVENT_ID"]=="other-conversation"


def test_lifecycle_and_file_operations_preserve_cli_semantics(monkeypatch):
    tool=load(monkeypatch)
    argv,_=tool.command({"action":"plan","taskId":"main","nodes":[{"id":"A","assignedTo":"w","spec":"Work","dependsOn":[]}]})
    assert argv[:4]==["/usr/local/bin/agt","task","plan","main"]
    assert json.loads(argv[-1])=={"nodes":[{"id":"A","assignedTo":"w","spec":"Work","dependsOn":[]}]}
    argv,_=tool.command({"action":"accept","taskId":"a","submissionId":"s1","note":"Evidence"})
    assert argv[2:]==["accept","a","--submission","s1","--note","Evidence"]
    argv,_=tool.command({"action":"submit","taskId":"a","summary":"Done","files":["app.py","tests.log"]})
    assert argv[-4:]==["--file","app.py","--file","tests.log"]
    argv,_=tool.command({"action":"files_read","taskId":"a","artifactId":"opaque","version":"v1","offset":4,"limit":16})
    assert argv[2:5]==["files","read","a"] and "--artifact" in argv


@pytest.mark.parametrize("arguments",[
    {"action":"raw_shell","command":"rm -rf /"},
    {"action":"get","taskId":"../other"},
    {"action":"get","taskId":"a","controllerURL":"http://other"},
    {"action":"complete","taskId":"a","report":"Done","roomId":"!foreign:test"},
    {"action":"create","leader":"lead","title":"Request","spec":"Work"},
    {"action":"accept","taskId":"a","submissionId":"s1","note":" "},
    {"action":"files_get","taskId":"a","artifactId":"opaque","output":"/outside"},
    {"action":"files_read","taskId":"a"},
    {"action":{"malformed":True}}, [],
])
def test_invalid_input_fails_before_process_execution(monkeypatch,arguments):
    tool=load(monkeypatch)
    monkeypatch.setattr(tool.subprocess,"run",lambda *args,**kwargs:pytest.fail("invalid input started a process"))
    assert tool.run(arguments)["ok"] is False


def test_file_publication_resolves_only_prepared_outputs(monkeypatch,tmp_path):
    tool=load(monkeypatch);monkeypatch.setenv("TEAMHARNESS_SHARED_DIR",str(tmp_path))
    outputs=tmp_path/"tasks/a/workspace";outputs.mkdir(parents=True)
    (outputs/"app.py").write_text("print('ok')")
    (outputs/"escape").symlink_to(tmp_path.parent)
    argv,_=tool.command({"action":"files_put","taskId":"a","source":"app.py","name":"app.py"})
    assert argv[argv.index("--source")+1]==str(outputs/"app.py")
    for source in ("../meta.json", "/etc/passwd", "escape/private"):
        assert tool.run({"action":"files_put","taskId":"a","source":source,"name":"app.py"})["ok"] is False


def test_cli_errors_and_invalid_receipts_are_not_success(monkeypatch):
    tool=load(monkeypatch)
    monkeypatch.setattr(tool.subprocess,"run",lambda *args,**kwargs:SimpleNamespace(returncode=1,stderr="HTTP 409 stale candidate",stdout=""))
    assert tool.run({"action":"get","taskId":"a"})["error"]=="task_operation_failed"
    monkeypatch.setattr(tool.subprocess,"run",lambda *args,**kwargs:SimpleNamespace(returncode=0,stderr="",stdout="prefix\n{}"))
    assert tool.run({"action":"get","taskId":"a"})["ok"] is False


def test_current_trace_is_copied_into_only_this_cli_process(monkeypatch):
    tool=load(monkeypatch)
    monkeypatch.setenv("AGENTTEAMS_OTEL_ENABLED","true")
    monkeypatch.setenv("AT_OTEL_TRACEPARENT","previous-process")
    root=ModuleType("opentelemetry");root.__path__=[]
    propagation=ModuleType("opentelemetry.propagate")
    propagation.inject=lambda carrier:carrier.update({"traceparent":"current-tool","baggage":"agentteams.task.id=a","unrelated":"ignored"})
    monkeypatch.setitem(sys.modules,"opentelemetry",root)
    monkeypatch.setitem(sys.modules,"opentelemetry.propagate",propagation)
    def execute(argv,**kwargs):
        assert kwargs["env"]["AT_OTEL_TRACEPARENT"]=="current-tool"
        assert kwargs["env"]["AT_OTEL_BAGGAGE"]=="agentteams.task.id=a"
        assert "AT_OTEL_UNRELATED" not in kwargs["env"]
        return SimpleNamespace(returncode=0,stdout='{"task":{"id":"a"}}',stderr="")
    monkeypatch.setattr(tool.subprocess,"run",execute)
    assert tool.run({"action":"get","taskId":"a"})["task"]["id"]=="a"
    assert os.environ["AT_OTEL_TRACEPARENT"]=="previous-process"


def test_file_identity_can_include_consistency_check_name(monkeypatch):
    tool = load(monkeypatch)
    argv, _ = tool.command({"action": "files_read", "taskId": "a", "name": "app.py", "artifactId": "opaque", "version": "v1"})
    assert argv == ["/usr/local/bin/agt", "task", "files", "read", "a", "--name", "app.py", "--artifact", "opaque", "--version", "v1"]


def test_execution_and_leader_schemas_require_task_identity(monkeypatch):
    tool = load(monkeypatch)
    for role in ["leader", "worker"]:
        assert set(tool.schema(role)["inputSchema"]["required"]) == {"action", "taskId"}
    assert tool.schema("manager")["inputSchema"]["required"] == ["action"]
