"""Runtime-neutral AgentTeams Matrix core (P9, PR1).

The core is the Matrix contract (sync / E2EE / media / mention /
allowlist / history / session) shared by the three runtime adapters
(QwenPaw in-process, Hermes in-process, OpenClaw via the sidecar
protocol).  M1 lands the skeleton: interfaces, file-backed session
state with the single-writer lock, and the sidecar protocol.

This package has no matrix-nio dependency; the transport is injected.
"""

from .core import MatrixCore
from .interfaces import (
    INBOUND_KINDS,
    OUTBOUND_KINDS,
    TYPING_STATES,
    CoreConfig,
    InboundEvent,
    MediaRef,
    MatrixTransport,
    OutboundMessage,
    RuntimeBridge,
    SessionStore,
)
from .sidecar import (
    FRAME_TYPES,
    MAX_LINE_BYTES,
    PROTOCOL_VERSION,
    SidecarClient,
    SidecarNotConnectedError,
    SidecarProtocolError,
    SidecarServer,
    decode_line,
    encode_frame,
    frame_to_inbound,
    frame_to_outbound,
    make_ack,
    make_error,
    make_hello,
    make_inbound,
    make_outbound,
    make_typing,
)
from .state import (
    CoreLockedError,
    FileSessionStore,
    SingleWriterLock,
    StateLayout,
)

__all__ = [
    "INBOUND_KINDS",
    "OUTBOUND_KINDS",
    "TYPING_STATES",
    "CoreConfig",
    "CoreLockedError",
    "FileSessionStore",
    "FRAME_TYPES",
    "InboundEvent",
    "MAX_LINE_BYTES",
    "MatrixCore",
    "MatrixTransport",
    "MediaRef",
    "OutboundMessage",
    "PROTOCOL_VERSION",
    "RuntimeBridge",
    "SidecarClient",
    "SidecarNotConnectedError",
    "SidecarProtocolError",
    "SidecarServer",
    "SessionStore",
    "SingleWriterLock",
    "StateLayout",
    "decode_line",
    "encode_frame",
    "frame_to_inbound",
    "frame_to_outbound",
    "make_ack",
    "make_error",
    "make_hello",
    "make_inbound",
    "make_outbound",
    "make_typing",
]
