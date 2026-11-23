"""Use real SDK state and runtime admission to record actual sent deliveries."""
import asyncio
import importlib.util
import json
import os
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch


def native_available():
    try:
        return bool(importlib.util.find_spec("agentscope.agent") and importlib.util.find_spec("qwenpaw.runtime"))
    except (ModuleNotFoundError, ValueError):
        return False


@unittest.skipUnless(native_available(), "native SDK required")
class NativeDeliveryContextTest(unittest.TestCase):
    def test_actual_delivery_is_admitted_and_persisted_without_observing_creation(self):
        from agentscope.agent import Agent
        from agentscope.model import ChatModelBase, ChatResponse
        from agentscope.message import Msg, TextBlock
        from agentscope.formatter import OpenAIChatFormatter
        from qwenpaw.runtime.hooks import HookContext
        from qwenpaw.runtime.runtime import Runtime
        from pydantic import BaseModel

        path=Path(__file__).resolve().parents[5]/"plugins/teamharness/adapters/qwenpaw/plugin.py"
        spec=importlib.util.spec_from_file_location("native_delivery_plugin",path)
        plugin=importlib.util.module_from_spec(spec);spec.loader.exec_module(plugin)
        testcase=self;manager=[None];reads=[]
        task_id = "task-" + "a" * 32
        row={"eventId":"completion","taskId":task_id,"status":"completed","roomId":"!source:test","reportMessageId":"$report","report":"Accepted report.","artifacts":[{"taskId":"child","name":"app.py","artifactId":"a1","messageId":"$file"}]}

        class Parameters(BaseModel):pass
        class Model(ChatModelBase):
            def __init__(self):
                super().__init__(None,"isolated-delivery-state-test",Parameters(),stream=False)
                self.formatter=OpenAIChatFormatter();self.calls=0
            async def __call__(self,*args,**kwargs):
                self.calls+=1
                messages=await self.formatter.format(kwargs.get("messages") or args[0]);text=json.dumps(messages)
                testcase.assertIn("[Task system delivery record]",text)
                testcase.assertIn("Accepted report.",text);testcase.assertIn("app.py",text)
                testcase.assertIn("$report",text);testcase.assertIn("$file",text)
                if any("What did you deliver for " in row.get("content", [{}])[0].get("text", "")
                       for row in messages if row.get("role") == "user" and row.get("content")):
                    testcase.assertIn("verified terminal delivery is current", text)
                testcase.assertTrue(manager[0].state.middle_context["teamharness.sent_deliveries"]["!source:test"]["completion"])
                return ChatResponse(content=[TextBlock(text="Delivery is already recorded.")],is_last=True)

        def get(path):
            reads.append(path);testcase.assertEqual(path,"task-deliveries?roomId=%21source%3Atest")
            return {"deliveries":[row]}

        def hook_context(agent, question="Did you deliver?"):
            return HookContext(request=SimpleNamespace(channel_meta={"room_id":"!source:test"}),session_id="original-room",agent_id="default",root_session_id="original-room",root_agent_id="default",workspace_dir=None,workspace=None,app_services=None,agent=agent,input_msgs=[Msg(name="Human",role="user",content=[TextBlock(text=question)])])

        async def run():
            model=Model();agent=Agent("Manager","Handle requests.",model,middlewares=[plugin._sanitizer_factory(None,None)]);manager[0]=agent
            ctx=hook_context(agent)
            with patch.object(plugin,"_controller_json",side_effect=get):
                await plugin._prepare_delivery_context(ctx)
                testcase.assertNotIn("teamharness.sent_deliveries",agent.state.middle_context) # Not acknowledged before admission.
                Runtime._apply_context_injections(ctx)
                await agent.reply(ctx.input_msgs)
            plugin._delivery_ack.reset(ctx.extras.pop("delivery_ack_token"))
            testcase.assertEqual(model.calls,1)
            # Real native state serialization retains the fact and its deduplication metadata together.
            persisted=type(agent.state).model_validate(agent.state.model_dump(mode="json"))
            restored_model=Model();restored=Agent("Manager","Handle requests.",restored_model,state=persisted,middlewares=[plugin._sanitizer_factory(None,None)]);manager[0]=restored
            next_ctx=hook_context(restored)
            with patch.object(plugin,"_controller_json",side_effect=get):
                await plugin._prepare_delivery_context(next_ctx)
                testcase.assertEqual(next_ctx.context_injections,[])
                await restored.reply(next_ctx.input_msgs)
            testcase.assertEqual(restored_model.calls,1)
            followup = hook_context(restored, "What did you deliver for " + task_id + "?")
            with patch.object(plugin, "_controller_json", side_effect=get):
                await plugin._prepare_delivery_context(followup)
                testcase.assertEqual(followup.context_injections, [])
                testcase.assertNotIn("delivery_ack_token", followup.extras)
                # The terminal record must reach the actual model even when the
                # runtime compacts or drops previous turn injections.
                try:
                    await restored.reply(followup.input_msgs)
                finally:
                    plugin._current_deliveries.reset(followup.extras.pop("current_deliveries_token"))
            testcase.assertEqual(restored_model.calls, 2)
            testcase.assertEqual(len(reads), 3) # One common read per actual Human turn.
        with patch.dict(os.environ,{"AGENTTEAMS_AGENT_ROLE":"manager"}):asyncio.run(run())


if __name__=="__main__":unittest.main()
