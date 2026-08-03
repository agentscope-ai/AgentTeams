"""Register AgentTeams Manager tools as a QwenPaw 2.0 plugin.

The four tools (projectflow, taskflow, message, filesync) were previously
injected via ``install_tool_hooks()`` which monkey-patched
``CoPawAgent._create_toolkit``.  QwenPaw 2.0's ``QwenPawAgent`` does not
have that method, so the tools are now registered through the plugin API's
``api.register_tool()``.

Tool dependencies (verified against agentscope 2.0.4.post1):
  - ``agentscope.message.TextBlock / Msg`` ✅
  - ``agentscope.tool.ToolResponse`` ✅
  - ``copaw_worker.task / sync / hooks.message_filter`` — pure stdlib + agentscope

No dependency on ``copaw`` 1.0.2 at module-import time.  The message and
taskflow tools read Matrix credentials directly from ``agent.json`` instead
of importing ``copaw.config.config``.
"""


class AgentTeamsManagerToolsPlugin:
    def register(self, api):
        from copaw_worker.hooks.tools.projectflow import projectflow
        from copaw_worker.hooks.tools.taskflow import taskflow
        from copaw_worker.hooks.tools.message import message
        from copaw_worker.hooks.tools.filesync import filesync

        api.register_tool(
            tool_name="projectflow",
            tool_func=projectflow,
            description="AgentTeams project/DAG lifecycle management",
            tool_type="internal",
            enabled=True,
        )
        api.register_tool(
            tool_name="taskflow",
            tool_func=taskflow,
            description="AgentTeams task state management",
            tool_type="internal",
            enabled=True,
        )
        api.register_tool(
            tool_name="message",
            tool_func=message,
            description="Send messages to workers via Matrix",
            tool_type="internal",
            enabled=True,
        )
        api.register_tool(
            tool_name="filesync",
            tool_func=filesync,
            description="Sync files between Manager and Workers via MinIO",
            tool_type="internal",
            enabled=True,
        )


plugin = AgentTeamsManagerToolsPlugin()
