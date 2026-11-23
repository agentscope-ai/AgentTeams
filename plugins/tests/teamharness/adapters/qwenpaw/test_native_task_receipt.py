"""Verify receipt injection at the real SDK's next model-context boundary."""
import asyncio
import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch
import os
import shlex
from types import SimpleNamespace


def native_available():
    try:
        return importlib.util.find_spec("agentscope.agent") is not None
    except (ModuleNotFoundError, ValueError):
        return False


@unittest.skipUnless(native_available(), "native QwenPaw SDK required")
class NativeTaskReceiptTest(unittest.TestCase):
    def test_multiple_inputs_bind_the_selected_source_and_reject_ambiguity_before_execution(self):
        from agentscope.tool import ToolResponse
        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("multi_input_native_plugin", root / "plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)
        sources = [{"inputId": "input-1", "eventId": "$first", "userId": "@a:test", "roomId": "!a:test"},
                   {"inputId": "input-2", "eventId": "$second", "userId": "@b:test", "roomId": "!b:test"}]

        async def scenario():
            source_token = plugin._creation_sources.set(sources)
            event_token = plugin._source_event.set("$first")
            route_token = plugin._requester_route.set({"target_user": "@a:test", "target_session": "!a:test"})
            try:
                for cli in (False, True):
                    with self.subTest(cli=cli):
                        observed = []
                        args = ({"command": "agt task create --leader lead --title Second --input-id input-2"}
                                if cli else {"action": "create", "leader": "lead", "title": "Second", "inputId": "input-2"})
                        call = SimpleNamespace(name="execute_shell_command" if cli else "teamharness__task", input=args)
                        async def execute(**_):
                            observed.append(json.loads(json.dumps(call.input)))
                            result = {"sourceEventId": "$second", "task": {"id": "main", "kind": "main", "status": "planned"},
                                      "notification": {"status": "queued", "eventId": "handoff", "recipient": "lead"}}
                            yield ToolResponse(state="success", content=[{"type": "text", "text": json.dumps(result)}])
                        middleware = plugin._sanitizer_factory(None, None)
                        replies = [r async for r in middleware.on_acting(None, {"tool_call": call}, execute)]
                        self.assertEqual(len(observed), 1)
                        if cli:
                            words = shlex.split(observed[0]["command"])
                            self.assertIn("AGENTTEAMS_ORIGIN_EVENT_ID=$second", words)
                            self.assertIn("AGENTTEAMS_INPUT_ID=input-2", words)
                        else:
                            self.assertEqual(observed[0]["_nativeSource"], {k: sources[1][k] for k in ("eventId", "userId", "roomId")})
                        self.assertEqual(middleware._finished_phase_receipt, "")
                        self.assertIn("[Task system receipt]", str(replies[0].content))
                        self.assertIn('supplied input IDs: ["input-1"]', replies[0].content[0].text)
                        self.assertEqual(middleware._registered_inputs, {"$second"})
                for input_id in (None, "input-unknown"):
                    called = []
                    args = {"action": "create", "leader": "lead", "title": "Ambiguous"}
                    if input_id is not None:
                        args["inputId"] = input_id
                    async def forbidden(**_):
                        called.append(True)
                        yield ToolResponse(state="success", content=[])
                    middleware = plugin._sanitizer_factory(None, None)
                    replies = [r async for r in middleware.on_acting(None, {"tool_call": SimpleNamespace(name="teamharness__task", input=args)}, forbidden)]
                    self.assertEqual(called, [])
                    self.assertEqual(replies[0].state, "error")
            finally:
                plugin._creation_sources.reset(source_token)
                plugin._source_event.reset(event_token)
                plugin._requester_route.reset(route_token)
        with patch.dict(os.environ, {"AGENTTEAMS_AGENT_ROLE": "manager"}):
            asyncio.run(scenario())

    def test_related_input_binding_reaches_the_native_task_tool(self):
        from agentscope.tool import ToolResponse
        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("related_native_plugin", root / "plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)
        sources = [{"inputId": "input-1", "eventId": "$original", "userId": "@a:test", "roomId": "!room:test"},
                   {"inputId": "input-2", "eventId": "$change", "userId": "@a:test", "roomId": "!room:test"}]

        async def scenario():
            token = plugin._creation_sources.set(sources)
            try:
                middleware = plugin._sanitizer_factory(None, None)
                for shell in (False, True):
                    command = 'agt task create --leader lead --title Report --input-id input-1 --related-input-id input-2'
                    args = {"command": command} if shell else {"action": "create", "leader": "lead", "title": "Report", "inputId": "input-1", "relatedInputIds": ["input-2"]}
                    call = SimpleNamespace(name="execute_shell_command" if shell else "teamharness__task", input=args)
                    async def execute(**kwargs):
                        bound = kwargs["tool_call"].input
                        if shell:
                            env = dict(part.split("=", 1) for part in shlex.split(bound["command"])[1:] if part.startswith("AGENTTEAMS_"))
                            self.assertEqual(json.loads(env["AGENTTEAMS_RELATED_INPUTS"]), {"input-2": "$change"})
                        else:
                            self.assertEqual(bound["_nativeSource"]["relatedSources"], ["$change"])
                        yield ToolResponse(state="success", content=[])
                    _ = [reply async for reply in middleware.on_acting(None, {"tool_call": call}, execute)]
            finally:
                plugin._creation_sources.reset(token)
        with patch.dict(os.environ, {"AGENTTEAMS_AGENT_ROLE": "manager"}):
            asyncio.run(scenario())

    def test_fresh_input_is_reasoned_before_creation_and_queued_duplicates_are_recorded(self):
        from agentscope.agent import Agent
        from agentscope.model import ChatModelBase, ChatResponse
        from agentscope.message import Msg, ToolCallBlock, TextBlock, ThinkingBlock
        from agentscope.tool import Toolkit, FunctionTool
        from agentscope.formatter import OpenAIChatFormatter
        from agentscope.permission import PermissionBehavior, PermissionDecision
        from pydantic import BaseModel
        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("fresh_input_native_plugin", root / "plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec); spec.loader.exec_module(plugin)
        class Parameters(BaseModel):
            pass
        testcase = self

        async def scenario(amend):
            commands = []
            body = "Change the report to JSON." if amend else "Do not create the requested Task."
            class Model(ChatModelBase):
                def __init__(self):
                    super().__init__(None, "synthetic-intake-boundary", Parameters(), stream=False)
                    self.formatter = OpenAIChatFormatter(); self.calls = 0
                async def __call__(self, *args, **kwargs):
                    self.calls += 1
                    if self.calls == 2:
                        refreshed = [message for message in kwargs["messages"]
                            if message.role == "user" and message.metadata.get("agentteams.event.id") == "$new"]
                        testcase.assertEqual(len(refreshed), 1, "Verified Human updates must reach the model as Human input")
                        testcase.assertIn(body, "".join(block.text for block in refreshed[0].content if block.type == "text"))
                        testcase.assertTrue(checkpointed, "New Human input must be checkpointed before inference")
                    if self.calls == 1 or amend and self.calls == 2:
                        command = 'agt task create --leader lead --title Report --input-id input-1'
                        if self.calls == 2:
                            command += ' --related-input-id ' + plugin._receipt_module().input_reference('!room:test', '$new')
                        return ChatResponse(content=[ToolCallBlock(id="call-"+str(self.calls), name="execute_shell_command", input=json.dumps({"command":command}))], is_last=True)
                    return ChatResponse(content=[ThinkingBlock(thinking="Synthetic final reasoning block."), TextBlock(text="Updated intent has been handled.")], is_last=True)
            async def execute(command: str):
                commands.append(command)
                if len(commands) == 1:
                    return {"ok":True,"outcome":"input_changed","noTaskRegistered":True,"sourceEventId":"$original", "inputs":[{"eventId":"$new","userId":"@a:test","roomId":"!room:test","body":body}]}
                env = dict(part.split("=",1) for part in shlex.split(command)[1:] if part.startswith("AGENTTEAMS_"))
                testcase.assertEqual(env["AGENTTEAMS_INPUT_FRONTIER"], "$new")
                testcase.assertEqual(json.loads(env["AGENTTEAMS_RELATED_INPUTS"]), {plugin._receipt_module().input_reference("!room:test", "$new"):"$new"})
                return {"sourceEventId":"$original","task":{"id":"main","kind":"main","status":"planned"},"notification":{"status":"queued","eventId":"handoff","recipient":"lead"}}
            toolkit = Toolkit()
            await toolkit.add_tool(FunctionTool(execute,name="execute_shell_command",permission=PermissionDecision(behavior=PermissionBehavior.ALLOW,message="Synthetic no-network fixture")))
            model = Model()
            checkpointed = []
            async def checkpoint(agent, inputs):
                testcase.assertEqual([entry["eventId"] for entry in inputs], ["$new"])
                testcase.assertTrue(any(message.role == "user" and message.metadata.get("agentteams.event.id") == "$new"
                    for message in agent.state.context))
                checkpointed.append(True)
            ctx = SimpleNamespace(extras={"agentteams_checkpoint_inputs": checkpoint})
            agent = Agent("Manager","Interpret Human intent.",model,toolkit=toolkit,middlewares=[plugin._sanitizer_factory(ctx,None)])
            token = plugin._creation_sources.set([{"inputId":"input-1","eventId":"$original","userId":"@a:test","roomId":"!room:test"}])
            try:
                await agent.reply(Msg(name="Human",role="user",content=[TextBlock(text="Build a CSV report.")]))
                testcase.assertEqual(len(commands), 2 if amend else 1)
                testcase.assertEqual(model.calls, 3 if amend else 2)
                text = json.dumps(await model.formatter.format(agent.state.context),ensure_ascii=False)
                testcase.assertIn(body,text)
                testcase.assertIn("[Task input refresh]",text)
                testcase.assertTrue(agent.state.middle_context["teamharness.refreshed_inputs"]["$new"])
            finally:
                plugin._creation_sources.reset(token)
        with patch.dict(os.environ,{"AGENTTEAMS_AGENT_ROLE":"manager"}):
            for amend in (False, True):
                with self.subTest(amend=amend):
                    asyncio.run(scenario(amend))

    def test_handled_queued_input_still_checkpoints_and_reads_without_model_inference(self):
        from agentscope.agent import Agent
        from agentscope.model import ChatModelBase
        from agentscope.message import Msg, TextBlock
        from agentscope.formatter import OpenAIChatFormatter
        from qwenpaw.agents.react_agent import QwenPawAgent
        from pydantic import BaseModel
        from unittest.mock import AsyncMock
        root=Path(__file__).resolve().parents[5]
        def load(name,path):
            spec=importlib.util.spec_from_file_location(name,path)
            module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
        plugin=load("handled_input_native_plugin",root/"plugins/teamharness/adapters/qwenpaw/plugin.py")
        matrix=load("handled_input_receipts",root/"plugins/agentteams-matrix/adapters/qwenpaw/agentteams_matrix/session_receipts.py")
        class Parameters(BaseModel):
            pass
        class Model(ChatModelBase):
            def __init__(self):
                super().__init__(None,"must-not-run",Parameters(),stream=False)
                self.formatter=OpenAIChatFormatter();self.calls=0
            async def __call__(self,*args,**kwargs):
                self.calls+=1
                raise AssertionError("Handled transport copy invoked the model")
        async def scenario():
            order=[]
            async def save(**kwargs):
                order.append("checkpoint")
                snapshot=kwargs["agent"].state_dict()
                self.assertIn("$handled",json.dumps(snapshot))
                self.assertIn("Previously interpreted input.",json.dumps(snapshot))
            async def read(room,events,pending):
                self.assertEqual((room,events),("!room:test",["$handled"]))
                order.append("read")
            delivery=SimpleNamespace(submit=read)
            channel=SimpleNamespace(_session_receipts=delivery,_task_execution=None)
            workspace=SimpleNamespace(channel_manager=SimpleNamespace(get_channel=AsyncMock(return_value=channel)),session=SimpleNamespace(save_session_state=save))
            meta={"event_id":"$handled","sender_id":"@human:test","room_id":"!room:test","session_source_event_ids":["$handled"]}
            ctx=SimpleNamespace(request=SimpleNamespace(channel="agentteams_matrix",channel_meta=meta,user_id="!room:test"),workspace=workspace,session_id="matrix:!room:test",mode_state={},extras={},inject_context=lambda *a,**kw:None,agent=None)
            model=Model()
            agent=Agent("Manager","Handle Human input.",model,middlewares=[matrix.SessionReceiptMiddleware(ctx),plugin._sanitizer_factory(None,None)])
            # Use the real runtime serializer on the same native SDK state.
            agent.state_dict=lambda:QwenPawAgent.state_dict(agent)
            ctx.agent=agent;agent.state.middle_context["teamharness.refreshed_inputs"]={"$handled":True}
            try:
                with patch.object(plugin,"_controller_json",return_value={"humans":[{"matrixUserID":"@human:test"}]}):
                    self.assertFalse(await plugin._manager_report_context(ctx))
                await agent.reply(Msg(name="Human",role="user",content=[TextBlock(text="Previously interpreted input.")]))
                self.assertEqual(model.calls,0)
                self.assertEqual(order,["checkpoint","read"])
                self.assertIn("[Task input acknowledgement]",json.dumps(await model.formatter.format(agent.state.context)))
            finally:
                for field,variable in (("handled_refresh_input_token",plugin._handled_refresh_input),("creation_sources_token",plugin._creation_sources),("source_event_token",plugin._source_event)):
                    token=ctx.extras.pop(field,None)
                    if token is not None:variable.reset(token)
        with patch.dict(os.environ,{"AGENTTEAMS_AGENT_ROLE":"manager"}):
            asyncio.run(scenario())

    def test_successful_task_cli_receipt_reaches_model_as_tool_context(self):
        self._assert_receipt("submit")

    def test_completion_receipt_reaches_leader_context_without_a_send_tool(self):
        self._assert_receipt("complete")

    def test_creation_receipt_binds_native_source_without_model_route_arguments(self):
        self._assert_receipt("create")

    def test_acceptance_receipt_reaches_exact_candidate_review_context(self):
        self._assert_receipt("accept")

    def test_rejection_receipt_reaches_exact_candidate_review_context(self):
        self._assert_receipt("reject")

    def test_plan_receipt_reaches_assigned_main_context(self):
        self._assert_receipt("plan")

    def test_replacement_receipt_injects_new_identity_without_reopening_old_task(self):
        self._assert_receipt("reject", replacement=True)

    def _assert_receipt(self, action, replacement=False):
        from agentscope.agent import Agent
        from agentscope.model import ChatModelBase, ChatResponse
        from agentscope.message import Msg, ToolCallBlock, TextBlock
        from agentscope.tool import Toolkit, FunctionTool
        from agentscope.formatter import OpenAIChatFormatter
        from agentscope.permission import PermissionBehavior, PermissionDecision
        from pydantic import BaseModel

        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("receipt_native_plugin", root / "plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(plugin)
        testcase = self
        calls = []
        command = f"agt task {action} t1 --" + ("summary" if action == "submit" else "report") + " Done"
        if action == "create": command = 'agt task create --leader leader-a --title "Request" --spec "Original criteria"'
        if action in {"accept", "reject"}: command = f'agt task {action} t1 --submission s1 --note "Business validation evidence"'
        if replacement: command += ' --replacement-spec "Fix exact cents"'
        if action == "plan": command = 'agt task plan t1 --json \'{"nodes":[{"id":"A","assignedTo":"worker-a","spec":"Implement"}]}\''
        identity_field = "sourceEventId" if action == "create" else ("submissionId" if action == "submit" else "completionId")

        class Parameters(BaseModel):
            pass

        class Model(ChatModelBase):
            def __init__(self):
                super().__init__(None, "isolated-receipt-test", Parameters(), stream=False)
                self.formatter = OpenAIChatFormatter()
                self.calls = 0

            async def __call__(self, *args, **kwargs):
                self.calls += 1
                if self.calls == 1:
                    return ChatResponse(content=[ToolCallBlock(id="call-one", name="execute_shell_command", input=json.dumps({"command":command}))], is_last=True)
                if action == "create" and self.calls == 2:
                    return ChatResponse(content=[TextBlock(text="Task registered; remaining input has been considered.")], is_last=True)
                raise AssertionError("A durable Task receipt must finish this phase without another model call")

        async def execute(command: str):
            calls.append(command)
            if action == "create":
                return {"sourceEventId":"$request", "task":{"id":"t1","kind":"main","status":"planned"},"notification":{"status":"queued","eventId":"handoff","recipient":"leader-a"}}
            if action in {"accept", "reject"}:
                state = "completed" if action == "accept" else "revision"
                return {**({"replacementTaskId":"exec-new"} if replacement else {}), "task":{"id":"t1","submissionId":"s1","status":state},
                        "review":{"id":"operation-one","submissionId":"s1","accepted":action == "accept","status":state},
                        "mainProjection":"pending"}
            if action == "plan":
                return {"task":{"id":"t1","kind":"main","status":"assigned","planId":"plan-one"},"progress":"ready",
                        "assignments":[{"taskId":"execution-a","status":"queued","eventId":"assignment-one","recipient":"worker-a"}]}
            return {"task":{"id":"t1","status":"submitted" if action == "submit" else "completed",identity_field:"operation-one"},
                    "notification":{"status":"queued","eventId":"e1","recipient":"manager-a" if action == "complete" else "leader-a"}}

        async def run():
            toolkit = Toolkit()
            await toolkit.add_tool(FunctionTool(execute, name="execute_shell_command", permission=PermissionDecision(behavior=PermissionBehavior.ALLOW, message="In-memory fixture; no shell or network access")))
            model = Model()
            agent = Agent("Worker", "Execute assigned work.", model, toolkit=toolkit, middlewares=[plugin._sanitizer_factory(None,None)])
            context = (plugin._source_event if action == "create" else plugin._execution_task if action == "submit"
                       else plugin._planning_tasks if action == "plan" else plugin._review_task if action in {"accept", "reject"} else plugin._completion_task)
            token = context.set("$request" if action == "create" else frozenset({"t1"}) if action == "plan" else ("t1","s1") if action in {"accept", "reject"} else "t1")
            route_token = plugin._requester_route.set({"target_user":"@human:test","target_session":"!source:test"}) if action == "create" else None
            try:
                await agent.reply(Msg(name="Leader", role="user", content=[TextBlock(text="Submit the result.")]))
                testcase.assertEqual(model.calls, 2 if action == "create" else 1)
                messages = await model.formatter.format(agent.state.context)
                text = json.dumps([m for m in messages if m.get("role") == "tool"], ensure_ascii=False)
                testcase.assertIn("[Task system receipt]", text)
                if action not in {"accept", "reject"}:
                    testcase.assertIn("worker-a" if action == "plan" else "manager-a" if action == "complete" else "leader-a", text)
                if action == "complete": testcase.assertIn("original conversation", text)
                testcase.assertIn("Do not recreate" if action == "plan" else "Do not repeat" if action in {"accept", "reject"} else "Do not resend" if action == "submit" else "Do not send another", text)
                if action in {"accept", "reject"}:
                    testcase.assertIn("synchronization is pending", text)
                    testcase.assertIn("operation-one", text)
                    if replacement:
                        testcase.assertIn("exec-new", text)
                        testcase.assertIn("remains terminal", text)
                elif action != "create":
                    testcase.assertIn("not yet confirmed", text)
                if action not in {"accept", "reject"}:
                    testcase.assertIn("planId" if action == "plan" else identity_field, text)
                # A creation receipt is not proof that the Human conversation
                # is finished. Fixed execution/review phases can end directly.
                if action != "create":
                    testcase.assertIn("[Task system receipt]", json.dumps([m for m in messages if m.get("role") == "assistant"]))
                else:
                    testcase.assertIn("remaining input has been considered", json.dumps(messages))
                if action == "create":
                    testcase.assertEqual(len(calls),1)
                    words=shlex.split(calls[0])
                    testcase.assertEqual(words[:4],["env","AGENTTEAMS_ORIGIN_EVENT_ID=$request","AGENTTEAMS_REQUESTER_USER_ID=@human:test","AGENTTEAMS_REQUESTER_ROOM_ID=!source:test"])
                    testcase.assertEqual(words[4:],shlex.split(command))
                else: testcase.assertEqual(calls, [command])
            finally:
                context.reset(token)
                if route_token is not None: plugin._requester_route.reset(route_token)
        with patch.dict(os.environ,{"AGENTTEAMS_AGENT_ROLE":"manager" if action == "create" else "worker"}):
            asyncio.run(run())


if __name__ == "__main__":
    unittest.main()


@unittest.skipUnless(native_available(), "native QwenPaw SDK required")
class StaleCreationBatchTest(unittest.TestCase):
    def test_obsolete_model_batch_cannot_create_again_before_reasoning_refresh(self):
        from agentscope.tool import ToolResponse
        root = Path(__file__).resolve().parents[5]
        spec = importlib.util.spec_from_file_location("obsolete_batch_plugin",root/"plugins/teamharness/adapters/qwenpaw/plugin.py")
        plugin = importlib.util.module_from_spec(spec);spec.loader.exec_module(plugin)
        async def scenario():
            token = plugin._creation_sources.set([{"inputId":"input-1","eventId":"$original","roomId":"!room:test","userId":"@a:test"}])
            try:
                middleware = plugin._sanitizer_factory(None,None)
                calls = []
                async def execute(**kwargs):
                    calls.append(True)
                    yield ToolResponse(state="success",content=[{"type":"text","text":json.dumps({"ok":True,"outcome":"input_changed","noTaskRegistered":True,"sourceEventId":"$original","inputs":[{"eventId":"$new","roomId":"!room:test","userId":"@a:test","body":"Withdraw it."}]})}])
                for _ in range(2):
                    call = SimpleNamespace(name="teamharness__task",input={"action":"create","leader":"lead","title":"Report","inputId":"input-1"})
                    replies = [item async for item in middleware.on_acting(None,{"tool_call":call},execute)]
                self.assertEqual(len(calls),1)
                self.assertEqual(replies[0].state,"error")
            finally:
                plugin._creation_sources.reset(token)
        with patch.dict(os.environ,{"AGENTTEAMS_AGENT_ROLE":"manager"}):
            asyncio.run(scenario())
