"""Runtime desired-state update support for qwenpaw-worker."""

from __future__ import annotations

from pathlib import Path

DEFAULT_AGENT_ID = "default"
TEAMS_PROMPT_FILE = "TEAMS.md"
PACKAGE_PROMPT_FILES = ("AGENTS.md", "SOUL.md")
PACKAGE_RUNTIME_OWNED_CONFIG_FILES = {Path(TEAMS_PROMPT_FILE)}
TEAMS_INTERNAL_CONTROL_MARKER = (
    "<!-- AGENTTEAMS_INTERNAL_CONTROL_FILE: TEAMS.md is managed by "
    "TeamHarness/QwenPaw runtime; agent packages must not overwrite or delete it. -->"
)
TEAMS_CONTEXT_START = "<!-- BEGIN AGENTTEAMS RUNTIME TEAM CONTEXT -->"
TEAMS_CONTEXT_END = "<!-- END AGENTTEAMS RUNTIME TEAM CONTEXT -->"
AGENT_IDENTITY_DATA_ENDPOINT_FORMAT = "agentidentitydata.{region_id}.aliyuncs.com"
REGION_ID_ENV_NAMES = ("AGENTTEAMS_REGION", "ALIBABA_CLOUD_REGION_ID", "REGION_ID")
