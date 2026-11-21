"""Real MCPTool + hook + agt binary, with an isolated HTTP contract fixture.

Run in the native image with the candidate Linux agt mounted on PATH. This is
SDK/CLI integration evidence, not a live model or Harness business certification.
"""
import asyncio
import importlib.util
import json
import os
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace
import threading
import unittest
from unittest.mock import patch
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit


def native_available():
    try:
        return bool(importlib.util.find_spec("agentscope.agent") and importlib.util.find_spec("qwenpaw.runtime"))
    except (ValueError, ModuleNotFoundError):
        return False


@unittest.skipUnless(native_available(), "native SDK and candidate agt required")
class NativeTaskToolTest(unittest.TestCase):
    def test_accept_works_after_shell_pruning_without_tool_authorization(self):
        self.run_task("accept")

    def test_reject_dispatches_explicit_replacement_and_injects_receipt(self):
        self.run_task("reject")

    def test_complete_works_after_shell_pruning_without_tool_authorization(self):
        self.run_task("complete")

    def test_create_injects_native_source_after_schema_validation(self):
        self.run_task("create")

    def test_create_uses_captured_request_without_spec_copy(self):
        self.run_task("create", no_spec=True)

    def test_stale_create_error_refreshes_context_without_an_extra_task_call(self):
        self.run_task("create", stale_input=True)

    def test_model_cannot_supply_private_source_fields(self):
        self.run_task("create", invalid_source=True)

    def test_unrelated_tools_keep_their_existing_permission_policy(self):
        from agentscope.permission import PermissionBehavior, PermissionDecision
        path=Path(__file__).resolve().parents[5]/"plugins/teamharness/adapters/qwenpaw/plugin.py"
        spec=importlib.util.spec_from_file_location("task_permission_scope_plugin",path)
        plugin=importlib.util.module_from_spec(spec);spec.loader.exec_module(plugin)
        middleware=plugin._sanitizer_factory(None,None)
        calls=[]
        async def policy(**kwargs):
            calls.append(kwargs)
            return PermissionDecision(behavior=PermissionBehavior.ASK,message="Existing unrelated tool policy")
        async def check(name):
            return await middleware.on_check_permission(None,{"tool_call":SimpleNamespace(name=name),"tool_input":{}},policy)
        self.assertEqual(asyncio.run(check("mcp__teamharness__task")).behavior,PermissionBehavior.ALLOW)
        for name in ("mcp__other__task","mcp__teamharness__filesync","execute_shell_command"):
            self.assertEqual(asyncio.run(check(name)).behavior,PermissionBehavior.ASK)
        self.assertEqual(len(calls),3)

    def test_stdio_server_invokes_candidate_cli_and_reports_errors(self):
        requests=[]
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                requests.append(self.path)
                self.send_response(409 if "stale" in self.path else 200)
                self.send_header("Content-Type","application/json");self.end_headers()
                self.wfile.write(json.dumps({"message":"stale candidate"} if "stale" in self.path else
                                           {"task":{"id":"a","status":"submitted","submissionId":"s1"}}).encode())
            def log_message(self,*args):pass
        server=ThreadingHTTPServer(("127.0.0.1",0),Handler)
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        try:
            root=Path(__file__).resolve().parents[5]
            messages=[{"jsonrpc":"2.0","id":0,"method":"initialize"}]+[
                {"jsonrpc":"2.0","id":index,"method":"tools/call","params":{"name":"task","arguments":{"action":"get","taskId":task}}}
                for index,task in ((1,"a"),(2,"stale"))]
            env=os.environ.copy();env.update({"AGENTTEAMS_CONTROLLER_URL":f"http://127.0.0.1:{server.server_port}",
                                             "AGENTTEAMS_AUTH_TOKEN":"fixture-token","AGENTTEAMS_AGENT_ROLE":"worker","AGENTTEAMS_OTEL_ENABLED":"false"})
            result=subprocess.run([sys.executable,str(root/"plugins/teamharness/mcp/server.py")],
                                  input="\n".join(json.dumps(message) for message in messages)+"\n",text=True,capture_output=True,env=env,timeout=30)
            self.assertEqual(result.returncode,0,result.stderr)
            replies=[json.loads(line) for line in result.stdout.splitlines()]
            self.assertEqual(len(replies),3)
            self.assertEqual(json.loads(replies[1]["result"]["content"][0]["text"])["task"]["submissionId"],"s1")
            self.assertTrue(replies[2]["result"]["isError"])
            self.assertEqual(len(requests),2)
        finally:
            server.shutdown();server.server_close();thread.join()

    def run_task(self, action, invalid_source=False, no_spec=False, stale_input=False):
        from agentscope.agent import Agent
        from agentscope.model import ChatModelBase, ChatResponse
        from agentscope.message import Msg, ToolCallBlock, TextBlock
        from agentscope.tool import Toolkit, FunctionTool, MCPTool
        from agentscope.formatter import OpenAIChatFormatter
        from qwenpaw.runtime.hooks import HookContext
        from qwenpaw.runtime.runtime import Runtime
        from pydantic import BaseModel
        import mcp.types

        root=Path(__file__).resolve().parents[5]
        def load(name,path):
            spec=importlib.util.spec_from_file_location(name,path)
            module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
        plugin=load("native_task_tool_plugin",root/"plugins/teamharness/adapters/qwenpaw/plugin.py")
        bridge=load("native_task_bridge",root/"plugins/teamharness/mcp/common/task_cli.py")
        testcase=self;requests=[];tool_calls=[]
        if action in {"accept", "reject"}:
            arguments={"action":action,"taskId":"execution-a","submissionId":"s1","note":"Independent business checks passed"}
            result={"task":{"id":"execution-a","status":"completed","submissionId":"s1"},
                    "review":{"id":"review-one","submissionId":"s1","accepted":True,"status":"completed"},
                    "mainProjection":"applied","dispatchStatus":"ready",
                    "assignments":[{"taskId":"execution-c","status":"queued","recipient":"worker-c","eventId":"dispatch-c"}]}
            if action == "reject":
                arguments["replacementSpec"] = "Fix exact cents"
                result["task"]["status"] = result["review"]["status"] = "revision"
                result["review"]["accepted"] = False
                result["replacementTaskId"] = "execution-c"
        elif action=="complete":
            arguments={"action":"complete","taskId":"main","report":"Accepted results attached."}
            result={"task":{"id":"main","status":"completed","completionId":"completion-one"},
                    "notification":{"status":"queued","recipient":"manager-a","eventId":"return-one"}}
        else:
            arguments={"action":"create","leader":"leader-a","title":"Original request","spec":"Original acceptance criteria"}
            if no_spec: arguments.pop("spec")
            if invalid_source:arguments["_nativeSource"]={"eventId":"forged","userId":"@other:test","roomId":"!other:test"}
            result={"sourceEventId":"$source","task":{"id":"main","kind":"main","status":"planned"},
                    "notification":{"status":"queued","recipient":"leader-a","eventId":"handoff"}}

        if stale_input:
            result = {"ok":True,"outcome":"input_changed","noTaskRegistered":True,"sourceEventId":"$source","inputs":[{"eventId":"$new","userId":"@human:test","roomId":"!source:test","body":"Do not create the requested Task."}]}

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                body=json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                requests.append((urlsplit(self.path).path,body,dict(self.headers)))
                self.send_response(200);self.send_header("Content-Type","application/json");self.end_headers()
                self.wfile.write(json.dumps(result).encode())
            def log_message(self,*args):pass

        class Session:
            async def call_tool(self,name,arguments,**kwargs):
                tool_calls.append((name,arguments))
                value=await asyncio.to_thread(bridge.run,arguments)
                return mcp.types.CallToolResult(content=[mcp.types.TextContent(type="text",text=json.dumps(value))],isError=value.get("ok") is False)

        class Parameters(BaseModel):pass
        class Model(ChatModelBase):
            def __init__(self):
                super().__init__(None,"native-task-tool-fixture",Parameters(),stream=False)
                self.formatter=OpenAIChatFormatter();self.calls=0
            async def __call__(self,*args,**kwargs):
                self.calls+=1
                if self.calls==1:
                    return ChatResponse(content=[ToolCallBlock(id="operation",name="mcp__teamharness__task",input=json.dumps(arguments))],is_last=True)
                testcase.assertTrue(invalid_source or action == "create", "A fixed Task phase invoked the model again")
                testcase.assertEqual(self.calls, 2)
                messages = await self.formatter.format(kwargs.get("messages") or args[0])
                text = json.dumps([message for message in messages if message.get("role") == "tool"])
                if invalid_source:
                    testcase.assertIn("Additional properties", text)
                    testcase.assertNotIn("[Task system receipt]", text)
                elif stale_input:
                    testcase.assertIn("[Task input refresh]", text)
                    testcase.assertIn("Do not create the requested Task.", text)
                else:
                    testcase.assertIn("[Task system receipt]", text)
                return ChatResponse(content=[TextBlock(text="Use the native task source context.")], is_last=True)

        async def run():
            toolkit=Toolkit();model=Model()
            # Keep the native MCPTool's ASK policy. The plugin must authorize
            # only this fixed task capability, rather than test code overriding it.
            schema=bridge.schema("manager" if action=="create" else "leader")
            await toolkit.add_tool(MCPTool("teamharness",mcp.types.Tool(name="task",description=schema["description"],inputSchema=schema["inputSchema"]),session=Session()))
            async def shell(command:str):raise AssertionError("acceptance invoked general shell")
            await toolkit.add_tool(FunctionTool(shell,name="execute_shell_command"))
            agent=Agent("Leader","Use the task system for coordination.",model,toolkit=toolkit,middlewares=[plugin._sanitizer_factory(None,None)])
            tokens=[]
            try:
                if action=="create":
                    tokens=[(plugin._creation_sources,plugin._creation_sources.set([{ "inputId":"input-1", "eventId":"$source", "userId":"@human:test", "roomId":"!source:test"}])),
                            (plugin._source_event,plugin._source_event.set("$source")),
                            (plugin._requester_route,plugin._requester_route.set({"target_user":"@human:test","target_session":"!source:test"}))]
                    inputs=[Msg(name="Human",role="user",content=[TextBlock(text="Delegate this request.")])]
                else:
                    kind="task_submitted" if action in {"accept","reject"} else "main_task_ready"
                    event={"kind":kind,"id":"receipt","main_task_id":"main","project_id":"","plan_type":"dag","dispatch_mode":"automatic"}
                    if action in {"accept","reject"}:event.update(task_id="execution-a",submission_id="s1")
                    sender="@worker:test" if action in {"accept","reject"} else "@leader:test"
                    meta={"agent_matrix_user_id":"@leader:test","sender_id":sender,"room_id":"!task:test",
                          "teamharness_matrix_coordination":True,"teamharness_coordination":event}
                    ctx=HookContext(request=SimpleNamespace(channel_meta=meta),session_id="review",agent_id="leader",root_session_id="review",root_agent_id="leader",workspace_dir=None,workspace=None,app_services=None,agent=agent,input_msgs=[Msg(name="Task system",role="user",content=[TextBlock(text="Review the current task result.")])])
                    def read(path):
                        if path.startswith("coordination/events/"):
                            return {**event,"actor":sender,"recipient":"@leader:test","room_id":"!task:test"}
                        if "execution-a" in path:return {"task_id":"execution-a","main_task_id":"main","status":"submitted","submission_id":"s1"}
                        return {"task_id":"main","task_kind":"main","status":"in_progress","plan_type":"dag","tasks":[{"task_id":"execution-a","status":"completed"}]}
                    with patch.object(plugin,"_controller_json",side_effect=read):
                        testcase.assertFalse(await plugin._manager_report_context(ctx))
                    names=[tool["function"]["name"] for tool in await toolkit.get_tool_schemas()]
                    testcase.assertNotIn("execute_shell_command",names)
                    testcase.assertIn("mcp__teamharness__task",names)
                    Runtime._apply_context_injections(ctx);inputs=ctx.input_msgs
                    for field,variable in (("review_task_token",plugin._review_task),("completion_task_token",plugin._completion_task)):
                        if field in ctx.extras:tokens.append((variable,ctx.extras[field]))
                await agent.reply(inputs)
                testcase.assertEqual(model.calls, 2 if invalid_source or action == "create" else 1)
                if stale_input:
                    testcase.assertTrue(agent.state.middle_context["teamharness.refreshed_inputs"]["$new"])
                if not invalid_source and not stale_input:
                    messages = await model.formatter.format(agent.state.context)
                    text = json.dumps([message for message in messages if message.get("role") == "tool"])
                    testcase.assertIn("[Task system receipt]", text)
                    testcase.assertIn("queued", text)
                    if action in {"accept", "reject"}: testcase.assertIn("worker-c", text)
                    if action == "reject": testcase.assertIn("remains terminal", text)
                    if action == "complete": testcase.assertIn("original conversation", text)
                    if action == "create":
                        testcase.assertIn("registered", text)
                        testcase.assertNotIn("_nativeSource", json.dumps(messages))
                testcase.assertEqual(len(tool_calls),0 if invalid_source else 1)
                testcase.assertEqual(len(requests),0 if invalid_source else 1)
            finally:
                for variable,token in reversed(tokens):variable.reset(token)

        server=ThreadingHTTPServer(("127.0.0.1",0),Handler)
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        try:
            env={"AGENTTEAMS_AGENT_ROLE":"manager" if action=="create" else "leader",
                 "AGENTTEAMS_CONTROLLER_URL":f"http://127.0.0.1:{server.server_port}","AGENTTEAMS_AUTH_TOKEN":"fixture-token",
                 "AGENTTEAMS_OTEL_ENABLED":"false",
                 "AT_OTEL_TRACEPARENT":"00-11111111111111111111111111111111-2222222222222222-01"}
            with patch.dict(os.environ,env):asyncio.run(run())
            if invalid_source:return
            path,body,headers=requests[0]
            testcase.assertEqual(headers.get("Authorization"),"Bearer fixture-token")
            testcase.assertEqual({key.lower():value for key,value in headers.items()}["traceparent"],env["AT_OTEL_TRACEPARENT"])
            if action=="reject": testcase.assertEqual(body["replacementSpec"],"Fix exact cents")
            if action=="create":
                testcase.assertEqual(path,"/api/v1/tasks")
                testcase.assertEqual({key.lower():value for key,value in headers.items()}["x-agentteams-origin-event-id"],"$source")
                testcase.assertEqual(set(body),{"title","spec","leader"})
                if no_spec: testcase.assertEqual(body["spec"], "")
                testcase.assertNotIn("_nativeSource",bridge.TASK_SCHEMA["inputSchema"]["properties"])
            else:testcase.assertEqual(path,"/api/v1/tasks/"+("execution-a" if action in {"accept","reject"} else "main")+"/"+action)
        finally:
            server.shutdown();server.server_close();thread.join()


if __name__=="__main__":unittest.main()
