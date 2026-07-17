"""Runtime desired-state update package."""

import asyncio

from qwenpaw_worker.update.agent_package import AgentPackageManager
from qwenpaw_worker.update.constants import (
    AGENT_IDENTITY_DATA_ENDPOINT_FORMAT,
    DEFAULT_AGENT_ID,
    PACKAGE_PROMPT_FILES,
    PACKAGE_RUNTIME_OWNED_CONFIG_FILES,
    REGION_ID_ENV_NAMES,
    TEAMS_CONTEXT_END,
    TEAMS_CONTEXT_START,
    TEAMS_INTERNAL_CONTROL_MARKER,
    TEAMS_PROMPT_FILE,
)
from qwenpaw_worker.update.model_sync import ApplyResult, QwenPawModelRuntimeSync
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.runtime_updater import RuntimeUpdater
from qwenpaw_worker.update.utils import _strip_json_line_comments, credential_provider_env_name

__all__ = [
    "AGENT_IDENTITY_DATA_ENDPOINT_FORMAT",
    "AgentPackageManager",
    "ApplyResult",
    "DEFAULT_AGENT_ID",
    "MemberRuntimeConfig",
    "PACKAGE_PROMPT_FILES",
    "PACKAGE_RUNTIME_OWNED_CONFIG_FILES",
    "QwenPawModelRuntimeSync",
    "REGION_ID_ENV_NAMES",
    "RuntimeUpdater",
    "TEAMS_CONTEXT_END",
    "TEAMS_CONTEXT_START",
    "TEAMS_INTERNAL_CONTROL_MARKER",
    "TEAMS_PROMPT_FILE",
    "_strip_json_line_comments",
    "credential_provider_env_name",
]
