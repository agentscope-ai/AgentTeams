"""Bind QwenPaw native turns to durable ordinary Matrix work."""
import asyncio
import json
import logging
import time
from uuid import uuid4

from task_recovery import Recovery, RecoveryConflict, RecoveryUnavailable
from worker_matrix_delivery import confirm_bound, publish_saved

log = logging.getLogger(__name__)

async def publish_nio_reply(channel, room, checkpoint, text, identity, write_checkpoint, send):
    client = channel._client

    async def whoami():
        response = await client.whoami()
        if getattr(response, "user_id", None) != channel.matrix_user_id:
            raise RecoveryConflict("Matrix session no longer belongs to this Worker")
        return response.user_id, getattr(response, "device_id", None)

    async def bind(next_checkpoint):
        saved = await asyncio.to_thread(write_checkpoint, next_checkpoint)
        confirm_bound(saved["execution"], identity, "qwenpaw", next_checkpoint)

    async def find(txn, body, sender):
        from nio import RoomMessageText, RoomMessagesResponse

        token = None
        seen = set()
        while token not in seen:
            seen.add(token)
            # The SDK decrypts /messages before inspecting the marker. A
            # server-side event-type filter would hide encrypted replies.
            page = await client.room_messages(room, start=token, limit=100, message_filter={"senders": [sender]})
            if not isinstance(page, RoomMessagesResponse) or page.room_id != room:
                raise RecoveryUnavailable("Matrix reply history is unavailable")
            for event in page.chunk:
                if not isinstance(event, RoomMessageText) or event.sender != sender:
                    continue
                source = event.source
                content = source["content"]
                if source.get("room_id", room) != room or content.get("body") != body:
                    continue
                marker = content.get("org.agentteams.reply_txn")
                if marker == txn:
                    return event.event_id
                if not checkpoint.get("reply_attempt") and marker is None and (source.get("unsigned") or {}).get("transaction_id") == txn:
                    return event.event_id
            token = page.end
            if not token:
                break
        return None

    return await publish_saved(checkpoint, text, whoami, bind, find, send)


class MessageExecution:
    def __init__(self, channel, execution):
        self.channel = channel
        self.matrix = execution.matrix
        self.sessions = execution.sessions
        tracker = channel._workspace.task_tracker
        if not hasattr(tracker, "_agentteams_message_turns"):
            tracker._agentteams_message_turns = {}
            tracker._agentteams_message_admission_lock = asyncio.Lock()
        self.turns = tracker._agentteams_message_turns
        self.admission_lock = tracker._agentteams_message_admission_lock

    async def admit(self, key):
        if key in self.turns:
            return None
        entry = await asyncio.to_thread(self.matrix.message, key)
        if entry["recovery"] in {"backfill", "verify"}:
            return None
        claim = await asyncio.to_thread(self.matrix.message_claim, key)
        mode = claim["recovery"]
        if mode == "deliver":
            await self.deliver(claim)
            return None
        if mode != "resume":
            return None
        identity = claim["identity"]
        work = {"kind": "message", "event_key": key}
        view = await asyncio.to_thread(self.sessions.load, work)
        if (view["generation"] != identity["generation"]
                or view["checkpoint_digest"] != claim["checkpoint_digest"]):
            raise RecoveryConflict("Message changed before native session selection")
        prefix = "matrix:message:" + key + ":"
        session_id = (view.get("cache") or {}).get("native_ref", "")
        if session_id.startswith(prefix):
            from qwenpaw.exceptions import AgentStateError
            try:
                native = await self.native_state(session_id, claim["input"]["room_id"])
            except (AgentStateError, OSError, ValueError, KeyError):
                native = None
            if not native or not native.get("agent") or not self.sessions.usable(view, session_id, Recovery.digest(native)):
                session_id = ""
        else:
            session_id = ""
        await asyncio.to_thread(self.matrix.message_current, identity)
        return {"identity": identity, "session_id": session_id or prefix + uuid4().hex, "rebuild": not session_id,
                "resume_text": claim["resume_text"] if claim["execution"].get("checkpoint") else "",
                "room": claim["input"]["room_id"], "lock": asyncio.Lock()}

    async def native_state(self, session, room):
        return await self.channel._workspace.session.get_session_state_dict(
            session_id=session, user_id=room, channel=self.channel.channel, allow_not_exist=True)

    async def deliver(self, claim):
        identity = claim["identity"]
        cp = claim["execution"]["checkpoint"]
        source, text = claim["input"], claim["saved_result"]
        event_id = await publish_nio_reply(self.channel, source["room_id"], cp, text, identity,
            lambda next_cp: self.matrix.message_checkpoint(identity, next_cp),
            lambda txn: self.channel.send_message_reply(source, text, txn))
        return await asyncio.to_thread(self.matrix.message_delivered, identity, event_id)

    async def finish(self, identity, text):
        if not text.strip():
            raise RecoveryConflict("Ordinary Matrix turn has no final result")
        if text.strip() == "NO_REPLY":
            await asyncio.to_thread(self.matrix.message_cancel, identity)
            return
        saved = await asyncio.to_thread(self.matrix.message_save_reply, identity, text)
        delivered = await self.deliver(saved)
        self.turns[identity["event_key"]]["checkpoint_digest"] = delivered["checkpoint_digest"]

    async def stream(self, turn, native):
        identity = turn["identity"]
        key = identity["event_key"]
        try:
            await asyncio.to_thread(self.matrix.message_current, identity)
            async for item in native:
                yield item
        finally:
            try:
                await native.aclose()
                try:
                    await self.remember(turn)
                except (RecoveryConflict, RecoveryUnavailable, OSError, ValueError):
                    log.warning("QwenPaw could not publish the optional native message cache", exc_info=True)
            finally:
                if self.turns.get(key) is turn:
                    del self.turns[key]

    async def remember(self, turn):
        identity = turn["identity"]
        digest = turn.get("checkpoint_digest")
        if not digest:
            return
        work = {"kind": "message", "event_key": identity["event_key"]}
        view = await asyncio.to_thread(self.sessions.load, work)
        if view["checkpoint_digest"] != digest:
            return
        native = await self.native_state(turn["session_id"], turn["room"])
        if native and native.get("agent"):
            await asyncio.to_thread(self.sessions.remember, work, view, identity, turn["session_id"],
                                    Recovery.digest(native), int(time.time()) + 86400)


async def canonical_registration(agent, call, server_path):
    """Bind delegation to the registered native MCP session and builtin file."""
    from pathlib import Path
    import sys
    from agentscope.mcp import MCPClient
    from agentscope.mcp._config import StdioMCPConfig
    from agentscope.tool import MCPTool

    toolkit = getattr(agent, 'toolkit', None)
    if toolkit is None:
        return None
    tool = await toolkit.get_tool(call.name)
    if type(tool) is not MCPTool:
        # QwenPaw 2.2 exposes builtin MCP through the native Driver adapter,
        # rather than AgentScope's legacy MCPTool/tool_groups.mcps registry.
        from qwenpaw.drivers.adapters.agentscope_tool import DriverCapabilityTool
        from qwenpaw.drivers.capabilities import parse_capability_id
        from qwenpaw.drivers.handlers.mcp import MCPDriverHandler
        from qwenpaw.drivers.handlers.mcp_stateful_client import StdIOStatefulClient
        from qwenpaw.drivers.manager import DriverManager

        if type(tool) is not DriverCapabilityTool:
            return None
        invoker = tool._invoker
        manager = getattr(invoker, '__self__', None)
        if (type(manager) is not DriverManager
                or getattr(invoker, '__func__', None) is not DriverManager.invoke_capability):
            return None
        protocol, driver, kind, action, name = parse_capability_id(tool._capability.capability_id)
        if (protocol, kind, action, name) != ('mcp', 'tool', 'invoke', 'task'):
            return None
        handler = manager._handlers.get(driver)
        if type(handler) is not MCPDriverHandler:
            return None
        client = handler._client
        if (type(client) is not StdIOStatefulClient or not client.is_connected
                or client.session is None or not handler.card.enabled):
            return None
        params = client.server_params
        endpoint = handler.card.endpoint
        if (endpoint.get('transport', 'stdio') != 'stdio'
                or endpoint.get('command') != params.command
                or endpoint.get('args', []) != params.args
                or params.command not in {'python', 'python3', sys.executable,
                    '/opt/venv/qwenpaw/bin/python', '/opt/venv/qwenpaw/bin/python3'}
                or len(params.args) != 1
                or Path(params.args[0]).resolve() != Path(server_path).resolve()):
            return None
        return {'origin': 'agentteams.common.task_native',
                'entrypoint': '/opt/agentteams/plugins/teamharness/mcp/common/task_native.py'}
    if getattr(tool._tool, 'name', None) != 'task':
        return None
    for group in toolkit.tool_groups:
        for client in group.mcps:
            if not isinstance(client, MCPClient) or not client.is_stateful:
                continue
            config = client.mcp_config
            args = getattr(config, 'args', None)
            if (getattr(config, 'command', None) not in {'python', 'python3', sys.executable,
                    '/opt/venv/qwenpaw/bin/python', '/opt/venv/qwenpaw/bin/python3'}
                    or not isinstance(config, StdioMCPConfig) or not isinstance(args, list)
                    or len(args) != 1 or Path(args[0]).resolve() != Path(server_path).resolve()
                    or client._session is None or tool._session is not client._session):
                continue
            return {'origin': 'agentteams.common.task_native',
                    'entrypoint': '/opt/agentteams/plugins/teamharness/mcp/common/task_native.py'}
    return None


async def read_only_registration(agent, call):
    """Recognize only the installed native file reader, including its guard."""
    from agentscope.tool import FunctionTool
    from qwenpaw.agents.tools.file_io import read_file
    from qwenpaw.governance.tool_adapter import _policy_tool_call

    toolkit = getattr(agent, 'toolkit', None)
    tool = await toolkit.get_tool(call.name) if toolkit else None
    return (isinstance(tool, FunctionTool) and type(tool).call is FunctionTool.call
            and type(tool).__call__ in {FunctionTool.__call__, _policy_tool_call}
            and tool._func is read_file)


async def guarded_tool(owner, turn, call, next_handler, kwargs, *, registration=None, read_only=False):
    """Save the terminal response of the exact admitted SDK call."""
    from agentscope.tool import ToolResponse
    from task_operation import tool_policy

    identity = turn["identity"]
    async with turn["lock"]:
        if tool_policy(registration)['mode'] == 'delegate':
            # The public Task bridge owns its transaction and unknown fence.
            # A generic started operation would block its own submit call.
            async for item in next_handler(**kwargs):
                yield item
            return
        call_id = call.id
        arguments = json.loads(call.input) if isinstance(call.input, str) else call.input
        message = 'event_key' in identity
        begin = owner.message_begin_tools if message else owner.begin_tools
        finish = owner.message_finish_tools if message else owner.finish_tools
        request = {"id": call_id, "name": call.name, "arguments": arguments}
        if read_only:
            request["read_only"] = True
        operation = await asyncio.to_thread(begin, identity, [request])
        final = None
        async for item in next_handler(**kwargs):
            if final is not None:
                raise RecoveryConflict("Native tool emitted output after its final result")
            if isinstance(item, ToolResponse):
                if item.state != "success" and not (read_only and item.state == "error"):
                    raise RecoveryConflict("Native tool outcome requires verification")
                final = item
            else:
                yield item
        if final is None:
            raise RecoveryConflict("Native tool closed without a confirmed result")
        result = {"id": call_id, "content": final.model_dump(mode="json")["content"]}
        if final.state == "error":
            result.update(effect="none", status="error")
        saved = await asyncio.to_thread(finish, identity, operation, [result])
        turn["checkpoint_digest"] = (saved["checkpoint_digest"] if message else
                                      Recovery.digest(saved['execution'].get('checkpoint')))
        yield final
