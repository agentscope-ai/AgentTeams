"""Register the AgentTeams-owned Matrix transport with QwenPaw."""


class AgentTeamsMatrixPlugin:
    def register(self, api):
        from .agentteams_matrix.channel import AgentTeamsMatrixChannel

        api.register_channel(
            AgentTeamsMatrixChannel,
            label="AgentTeams Matrix",
            description="Managed Matrix transport for AgentTeams rooms.",
            config_fields=[
                {"name": "enabled", "label": "Enabled", "type": "switch"},
                {"name": "homeserver", "label": "Homeserver", "type": "text", "required": True},
                {"name": "user_id", "label": "User ID", "type": "text", "required": True},
                {"name": "access_token", "label": "Access Token", "type": "password", "required": True},
                {"name": "encryption", "label": "Encryption", "type": "switch"},
                {"name": "require_mention", "label": "Require mention", "type": "switch"},
                {"name": "show_thinking", "label": "Show thinking", "type": "switch"},
                {"name": "show_tool_calls", "label": "Show tool calls", "type": "switch"},
                {"name": "show_tool_results", "label": "Show tool results", "type": "switch"},
            ],
        )


plugin = AgentTeamsMatrixPlugin()
