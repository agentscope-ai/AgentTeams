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


def test_wrong_action_fields_name_the_action_and_valid_public_fields(monkeypatch):
    tool = load(monkeypatch)
    with pytest.raises(ValueError) as error:
        tool.command({"action": "accept", "taskId": "t1", "submissionId": "s1", "note": "OK", "replacementSpec": "wrong phase"})
    message = str(error.value)
    assert "accept" in message and "replacementSpec" in message
    assert "submissionId" in message and "note" in message
    assert "_nativeSource" not in message


def test_private_source_field_is_never_advertised_as_a_model_argument(monkeypatch):
    tool = load(monkeypatch)
    with pytest.raises(ValueError) as error:
        tool.command({"action": "create", "leader": "lead", "title": "Request", "bogus": True})
    assert "bogus" in str(error.value)
    assert "_nativeSource" not in str(error.value)


def test_cli_validation_and_unconfirmed_execution_have_distinct_effects(monkeypatch):
    tool = load(monkeypatch)
    assert tool.run({"action": "submit", "taskId": "t1"})["effect"] == "none"
    def timeout(*args, **kwargs):
        raise tool.subprocess.TimeoutExpired(args[0], 120)
    monkeypatch.setattr(tool.subprocess, "run", timeout)
    assert tool.run({"action": "submit", "taskId": "t1", "summary": "done"})["effect"] == "unknown"


@pytest.mark.parametrize("action", ["get", "files_list", "files_read", "files_get"])
@pytest.mark.parametrize("failure", ["http", "timeout", "invalid_receipt"])
def test_read_failures_leave_the_task_turn_usable(monkeypatch, action, failure):
    tool = load(monkeypatch)
    arguments = {"action": action, "taskId": "t1"}
    if action in {"files_read", "files_get"}:
        arguments["name"] = "output.txt"

    def run(argv, **kwargs):
        if failure == "timeout":
            raise tool.subprocess.TimeoutExpired(argv, 120)
        if failure == "invalid_receipt":
            return tool.subprocess.CompletedProcess(argv, 0, "[]", "")
        return tool.subprocess.CompletedProcess(argv, 1, "", "Error: HTTP 404: task artifact not found")

    monkeypatch.setattr(tool.subprocess, "run", run)
    result = tool.run(arguments)
    assert result["ok"] is False
    assert result["effect"] == ("unknown" if action == "files_get" else "none")
    assert result["retryable"] is (failure == "timeout")


def test_create_carries_a_framework_selected_input_reference(monkeypatch):
    tool = load(monkeypatch)
    argv, env = tool.command({"action": "create", "leader": "lead", "title": "Second request", "inputId": "input-2",
        "_nativeSource": {"eventId": "$second", "userId": "@human:test", "roomId": "!source:test"}})
    assert argv[-2:] == ["--input-id", "input-2"]
    assert env["AGENTTEAMS_INPUT_ID"] == "input-2"
    assert env["AGENTTEAMS_ORIGIN_EVENT_ID"] == "$second"
    assert "inputId" in tool.schema("manager")["inputSchema"]["properties"]
    assert "inputId" not in tool.schema("worker")["inputSchema"]["properties"]


def test_rejection_can_request_explicit_replacement_without_shell(monkeypatch):
    tool = load(monkeypatch)
    argv, _ = tool.command({"action":"reject", "taskId":"t1", "submissionId":"s1", "note":"QA failed", "replacementSpec":"Fix cents; literal $(text)"})
    assert argv == ["/usr/local/bin/agt", "task", "reject", "t1", "--submission", "s1", "--note", "QA failed", "--replacement-spec", "Fix cents; literal $(text)"]
    with pytest.raises(ValueError):
        tool.command({"action":"accept", "taskId":"t1", "submissionId":"s1", "note":"OK", "replacementSpec":"Fix"})


def test_cancellation_uses_the_public_cli_and_preserves_its_receipt(monkeypatch):
    tool = load(monkeypatch)
    receipt = {"task": {"id": "main", "status": "cancelled"},
               "cancellation": {"id": "cancel-one", "reason": "Stop; literal $(text)"},
               "projection": "pending"}
    def execute(argv, **kwargs):
        assert argv == ["/usr/local/bin/agt", "task", "cancel", "main", "--reason", "Stop; literal $(text)", "--team", "alpha"]
        assert "shell" not in kwargs
        return SimpleNamespace(returncode=0, stdout=json.dumps(receipt), stderr="")
    monkeypatch.setattr(tool.subprocess, "run", execute)
    assert tool.run({"action": "cancel", "taskId": "main", "reason": "Stop; literal $(text)", "team": "alpha"}) == receipt
    for role in ("manager", "leader"):
        assert "cancel" in tool.schema(role)["inputSchema"]["properties"]["action"]["enum"]
    for role in ("worker", "remote-member"):
        assert "cancel" not in tool.schema(role)["inputSchema"]["properties"]["action"]["enum"]


@pytest.mark.parametrize("arguments", [
    {"action": "cancel", "taskId": "main"},
    {"action": "cancel", "taskId": "main", "reason": " "},
    {"action": "cancel", "taskId": "../main", "reason": "Stop"},
])
def test_invalid_cancellation_does_not_start_a_cli_process(monkeypatch, arguments):
    tool = load(monkeypatch)
    monkeypatch.setattr(tool.subprocess, "run", lambda *args, **kwargs: pytest.fail("invalid cancellation executed"))
    assert tool.run(arguments)["ok"] is False


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


def test_intake_schemas_allow_create_while_execution_still_requires_task_identity(monkeypatch):
    tool = load(monkeypatch)
    assert set(tool.schema("worker")["inputSchema"]["required"]) == {"action", "taskId"}
    for role in ["manager", "leader"]:
        assert tool.schema(role)["inputSchema"]["required"] == ["action"]
        assert "create" in tool.schema(role)["inputSchema"]["properties"]["action"]["enum"]


@pytest.mark.parametrize("message, retryable", [
    ("Error: HTTP 503: storage unavailable\nUsage: agt task accept", True),
    ("HTTP 429: rate limited", True),
    ('Error: Post "http://controller/api/v1/tasks/a/accept": EOF', True),
    ("Error: HTTP 400: input contains HTTP 503", False),
    ("Error: HTTP 409: stale candidate", False),
    ("unknown argument: 503", False),
])
def test_retryable_status_is_transport_scoped(monkeypatch, message, retryable):
    tool = load(monkeypatch)
    monkeypatch.setattr(tool.subprocess, "run", lambda *args, **kwargs:
                        SimpleNamespace(returncode=1, stderr=message, stdout=""))
    result = tool.run({"action": "get", "taskId": "a"})
    assert result["ok"] is False
    assert result["retryable"] is retryable


def test_only_process_timeout_is_retryable_without_cli_status(monkeypatch):
    tool = load(monkeypatch)
    for error, expected in [(tool.subprocess.TimeoutExpired("agt", 120), True),
                            (FileNotFoundError("agt missing"), False)]:
        def fail(*args, **kwargs):
            raise error
        monkeypatch.setattr(tool.subprocess, "run", fail)
        assert tool.run({"action": "get", "taskId": "a"})["retryable"] is expected


def test_create_binds_related_corrections_in_a_private_process_environment(monkeypatch):
    tool = load(monkeypatch)
    source = {"eventId": "$original", "userId": "@a:test", "roomId": "!room:test", "relatedSources": ["$change"]}
    arguments = {"action": "create", "leader": "lead", "title": "Report", "inputId": "input-1", "relatedInputIds": ["input-2"], "_nativeSource": source}
    argv, env = tool.command(arguments)
    assert argv[-2:] == ["--related-input-id", "input-2"]
    assert json.loads(env["AGENTTEAMS_RELATED_INPUTS"]) == {"input-2": "$change"}
    assert "relatedInputIds" not in tool.schema("worker")["inputSchema"]["properties"]
    arguments["relatedInputIds"] = ["input-2", "input-3"]
    with pytest.raises(ValueError):
        tool.command(arguments)


def test_rejection_sends_correction_graph_as_one_json_argument(monkeypatch):
    tool = load(monkeypatch)
    nodes = [{"id": "fix", "assignedTo": "developer", "spec": "Fix $(literal)", "dependsOn": []},
             {"id": "review", "assignedTo": "reviewer", "spec": "Recheck", "dependsOn": ["fix"]}]
    argv, _ = tool.command({"action": "reject", "taskId": "t1", "submissionId": "s1", "note": "Failed review",
                           "replacementSpec": "Correct then independently recheck", "replacementNodes": nodes})
    assert json.loads(argv[argv.index("--replacement-nodes") + 1]) == nodes
    assert "replacementNodes" in tool.schema("leader")["inputSchema"]["properties"]


def test_invalid_correction_dependencies_are_rejected_before_native_ledger(monkeypatch):
    tool = load(monkeypatch)
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller.test")
    monkeypatch.setattr(tool.subprocess, "run", lambda *args, **kwargs: pytest.fail("invalid graph reached CLI"))
    args = {"action": "reject", "taskId": "t1", "submissionId": "s1", "note": "Failed review", "replacementSpec": "Fix",
            "replacementNodes": [{"id": "fix", "assignedTo": "dev", "spec": "Fix", "dependsOn": ["original-analysis"]}]}
    result = tool.run(args)
    assert result["effect"] == "none" and result["error"] == "invalid_task_arguments"
    assert "local correction IDs" in result["message"]
