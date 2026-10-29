"""Runtime-neutral AgentTeams Matrix core (P9, PR1).

The core is the Matrix contract (sync / E2EE / media / mention /
allowlist / history / session) shared by the three runtime adapters
(QwenPaw in-process, Hermes in-process, OpenClaw via the sidecar
protocol).  M1 lands the skeleton: interfaces, file-backed session
state with the single-writer lock, and the sidecar protocol.

This package has no matrix-nio dependency at the package level:
``core.transport`` (M4a) and ``core.sync`` (M4b) are the only core
modules that import matrix-nio, and neither is imported from
``core/__init__.py``.  ``core.media`` and ``core.e2ee`` (M4c) also do
not import matrix-nio at module level — the encrypted-attachment
decryption lazily imports ``nio.crypto.attachments`` inside the
function.

M2 adds the runtime-neutral mention parsing (``mentions``), the
per-room history buffer (``history``), and the session-id derivation
(``session``) extracted from the QwenPaw channel.
"""

from .core import MatrixCore
from .e2ee import E2EEMaintainer, e2ee_store_path, probe_olm
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
from .media import MediaCache, mxc_to_http
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
    "E2EEMaintainer",
    "FileSessionStore",
    "FRAME_TYPES",
    "HistoryEntry",
    "InboundEvent",
    "MAX_LINE_BYTES",
    "MatrixCore",
    "MatrixTransport",
    "MediaCache",
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
    "e2ee_store_path",
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
    "mxc_to_http",
    "probe_olm",
    "resolve_session_id",
    "strip_mention_prefix",
    "text_mentions_worker",
    "to_handle_from_target",
    "was_mentioned",
]
