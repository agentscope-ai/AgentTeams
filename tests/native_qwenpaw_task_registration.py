"""Native QwenPaw Driver/MCP registry conformance using an isolated stdio server.

Run in the pinned QwenPaw image with this source mounted at /source. No model,
Controller, Matrix traffic, production configuration or session state is used.
"""
import asyncio
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace

from agentscope.tool import Toolkit
from qwenpaw.drivers.adapters.agentscope_tool import DriverCapabilityTool, build_driver_agent_tools
from qwenpaw.drivers.contracts import DriverCard
from qwenpaw.drivers.credentials.store import AsyncCredentialStore
from qwenpaw.drivers.handlers.mcp import MCPDriverHandler
from qwenpaw.drivers.manager import DriverManager

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'shared/lib'))
from qwenpaw_message_execution import canonical_registration
from task_operation import tool_policy


async def main():
    server = Path('/opt/agentteams/plugins/teamharness/mcp/server.py')
    with tempfile.TemporaryDirectory(prefix='own-qwen-native-registration-') as directory:
        root = Path(directory)
        manager = DriverManager(root/'cards', AsyncCredentialStore(root/'credentials.yaml'))
        manager.register_handler_type('mcp', MCPDriverHandler)
        card = DriverCard(name='teamharness', protocol='mcp', endpoint={
            'transport': 'stdio', 'command': sys.executable, 'args': [str(server)],
            'cwd': str(server.parent.parent)}, config={'tools': ['task']})
        await manager.replace_transient_drivers('own-fixture', [card])
        try:
            tools, _ = await build_driver_agent_tools(manager, {'driver_scope_id': 'own-fixture'})
            assert len(tools) == 1 and type(tools[0]) is DriverCapabilityTool
            tool = tools[0]
            agent = SimpleNamespace(toolkit=Toolkit(tools=tools))
            call = SimpleNamespace(name=tool.name)
            assert call.name == 'teamharness__task'
            assert not any(group.mcps for group in agent.toolkit.tool_groups)
            baseline = Path('/baseline/qwenpaw_message_execution.py')
            if baseline.is_file():
                spec = importlib.util.spec_from_file_location('prior_native_binding', baseline)
                prior = importlib.util.module_from_spec(spec); spec.loader.exec_module(prior)
                assert await prior.canonical_registration(agent, call, server) is None
            registration = await canonical_registration(agent, call, server)
            assert tool_policy(registration)['mode'] == 'delegate'
            assert await canonical_registration(agent, call, root/'foreign-server.py') is None
            # A renamed arbitrary Driver tool cannot borrow the fixed factory's identity.
            async def custom_invoker(_):
                raise AssertionError('No tool invocation is needed for registry validation')
            forged = DriverCapabilityTool(tool._capability, custom_invoker)
            forged_agent = SimpleNamespace(toolkit=Toolkit(tools=[forged]))
            assert await canonical_registration(forged_agent, call, server) is None
            # Editing card metadata without changing the connected process is not authority.
            saved_args = list(card.endpoint['args'])
            card.endpoint['args'] = [str(root/'foreign-server.py')]
            assert await canonical_registration(agent, call, server) is None
            card.endpoint['args'] = saved_args
            assert tool_policy(await canonical_registration(agent, call, server))['mode'] == 'delegate'
            print(json.dumps({'sdkRegistry': type(tool).__name__, 'nativeToolName': call.name,
                              'realStdioMCPConnected': manager._handlers['teamharness']._client.is_connected,
                              'legacyBindingReproducedMissingDelegation': baseline.is_file(),
                              'fixedFactoryDelegates': True, 'spoofedBindingsRejected': 3,
                              'realTaskCertification': False}))
        finally:
            await manager.shutdown_all()


if __name__ == '__main__':
    asyncio.run(main())
