"""Backward-compatible import path for the unified AgentTeams Matrix overlay."""

from matrix.channel import (
    DEFAULT_SYNC_TIMEOUT_MS,
    MatrixChannel,
    MatrixChannelConfig,
    STARTUP_REPLAY_BUFFER_CAP,
)
from nio.responses import JoinedMembersResponse

__all__ = [
    "DEFAULT_SYNC_TIMEOUT_MS",
    "JoinedMembersResponse",
    "MatrixChannel",
    "MatrixChannelConfig",
    "STARTUP_REPLAY_BUFFER_CAP",
]
