"""Runtime-neutral AgentTeams Matrix core (P9, PR1).

The core is the Matrix contract (sync / E2EE / media / mention /
allowlist / history / session) shared by the three runtime adapters
(QwenPaw in-process, Hermes in-process, OpenClaw via the sidecar
protocol).  M1 lands the skeleton: interfaces, file-backed session
state with the single-writer lock, and the sidecar protocol.

This package has no matrix-nio dependency at the package level:
``core.transport`` (M4a) is the only core module that imports
matrix-nio, and it must not be imported from ``core/__init__.py``.

M2 adds the runtime-neutral mention parsing (``mentions``), the
per-room history buffer (``history``), and the session-id derivation
(``session``) extracted from the QwenPaw channel.
"""

from .core import MatrixCore
from .history import (
    CURRENT_MESSAGE_MARKER,
    DEFAULT_HISTORY_LIMIT,
    HISTORY_CONTEXT_MARKER,
    HistoryEntry,
    RoomHistory,
)
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
from .mentions import (
    extract_mentions_from_text,
    strip_mention_prefix,
    text_mentions_worker,
    was_mentioned,
)
from .session import resolve_session_id, to_handle_from_target
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
    "CURRENT_MESSAGE_MARKER",
    "DEFAULT_HISTORY_LIMIT",
    "HISTORY_CONTEXT_MARKER",
    "INBOUND_KINDS",
    "OUTBOUND_KINDS",
    "TYPING_STATES",
    "CoreConfig",
    "CoreLockedError",
    "FileSessionStore",
    "FRAME_TYPES",
    "HistoryEntry",
    "InboundEvent",
    "MAX_LINE_BYTES",
    "MatrixCore",
    "MatrixTransport",
    "MediaRef",
    "OutboundMessage",
    "PROTOCOL_VERSION",
    "RoomHistory",
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
    "extract_mentions_from_text",
    "frame_to_inbound",
    "frame_to_outbound",
    "make_ack",
    "make_error",
    "make_hello",
    "make_inbound",
    "make_outbound",
    "make_typing",
    "resolve_session_id",
    "strip_mention_prefix",
    "text_mentions_worker",
    "to_handle_from_target",
    "was_mentioned",
]
