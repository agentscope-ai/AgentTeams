"""Runtime-neutral Matrix core interfaces (P9, PR1 M1 skeleton).

These interfaces define the boundary between the Matrix transport layer
(the only component that talks to matrix-nio) and the runtime adapters
(QwenPaw in-process, Hermes in-process, OpenClaw via the sidecar
protocol).  The core itself has **no** matrix-nio dependency: a
transport implementation is injected.

Field layout follows the P9 detailed design
(`17-P9-详细设计-20261029.md` §3).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Awaitable, Callable, Protocol, runtime_checkable

# Inbound event kinds (normalised Matrix events).
INBOUND_KINDS = ("text", "media", "edit", "redaction")

# Outbound message kinds (what the runtime asks the core to send).
OUTBOUND_KINDS = ("final", "tool", "notice")

# Typing notification states.
TYPING_STATES = ("start", "stop")


@dataclass(frozen=True)
class MediaRef:
    """Reference to a Matrix media object (mxc URI plus optional metadata)."""

    mxc_uri: str
    mime_type: str | None = None
    file_name: str | None = None
    size: int | None = None

    def to_dict(self) -> dict[str, Any]:
        """Serialise for the sidecar protocol / persistence."""
        return {
            "mxc_uri": self.mxc_uri,
            "mime_type": self.mime_type,
            "file_name": self.file_name,
            "size": self.size,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "MediaRef":
        """Parse a frame/persisted mapping back into a :class:`MediaRef`."""
        return cls(
            mxc_uri=str(data["mxc_uri"]),
            mime_type=data.get("mime_type"),
            file_name=data.get("file_name"),
            size=data.get("size"),
        )


@dataclass(frozen=True)
class InboundEvent:
    """A normalised Matrix event delivered to the runtime bridge."""

    event_id: str
    room_id: str
    sender: str
    ts: int
    text: str
    media: list[MediaRef] = field(default_factory=list)
    mentions: list[str] = field(default_factory=list)
    thread_root: str | None = None
    kind: str = "text"

    def __post_init__(self) -> None:
        if self.kind not in INBOUND_KINDS:
            raise ValueError(
                f"InboundEvent.kind must be one of {INBOUND_KINDS}, got {self.kind!r}"
            )
        if not self.event_id or not self.room_id or not self.sender:
            raise ValueError(
                "InboundEvent.event_id, room_id and sender must be non-empty"
            )


@dataclass(frozen=True)
class OutboundMessage:
    """A message the runtime wants the core to deliver to a Matrix room."""

    room_id: str
    text: str
    reply_to: str | None = None
    kind: str = "final"
    media: list[MediaRef] = ()

    def __post_init__(self) -> None:
        if self.kind not in OUTBOUND_KINDS:
            raise ValueError(
                f"OutboundMessage.kind must be one of {OUTBOUND_KINDS}, got {self.kind!r}"
            )
        if not self.room_id:
            raise ValueError("OutboundMessage.room_id must be non-empty")


@runtime_checkable
class MatrixTransport(Protocol):
    """The only interface that touches matrix-nio (implemented per runtime).

    Reconnection on disconnect is the transport's job; the core records
    the resulting state/errors.
    """

    async def start(self, on_event: Callable[[InboundEvent], Awaitable[None]]) -> None:
        """Start syncing and invoke *on_event* for each inbound event."""
        ...

    async def stop(self) -> None:
        ...

    async def send(self, msg: OutboundMessage) -> str:
        """Send a message; return the resulting Matrix event id."""
        ...

    async def edit(self, room_id: str, event_id: str, text: str) -> None:
        ...

    async def react(self, room_id: str, event_id: str, key: str) -> None:
        ...

    async def typing(self, room_id: str, state: str) -> None:
        ...

    async def download(self, media: MediaRef) -> bytes:
        ...


@runtime_checkable
class SessionStore(Protocol):
    """Sync token / auth state / per-(room, sender) session keys."""

    def load_sync_token(self) -> str | None:
        ...

    def save_sync_token(self, token: str) -> None:
        ...

    def load_session_key(self, room_id: str, sender: str) -> str | None:
        ...

    def save_session_key(self, room_id: str, sender: str, key: str) -> None:
        ...


@runtime_checkable
class RuntimeBridge(Protocol):
    """Bridge implemented by the runtime side.

    The core invokes :meth:`on_inbound` for every normalised Matrix event
    and :meth:`on_runtime_output` (a core callback) after an outbound
    message produced by the runtime has been handed to the transport.
    The runtime pushes outbound messages via :meth:`MatrixCore.submit`.
    """

    async def on_inbound(self, event: InboundEvent) -> None:
        ...

    async def on_runtime_output(self, msg: OutboundMessage) -> None:
        ...


@dataclass(frozen=True)
class CoreConfig:
    """Configuration for a :class:`~core.core.MatrixCore` instance.

    ``state_dir`` is ``$AGENTTEAMS_MATRIX_STATE_DIR`` in production
    (default ``<agent workspace>/.agentteams/matrix/``); ``socket_path``
    is ``$AGENTTEAMS_MATRIX_SOCKET`` (default ``<state_dir>/sidecar.sock``).
    """

    state_dir: Path
    agent_id: str
    socket_path: Path | None = None
    # Transport reconnect backoff (exponential): delay doubles per attempt.
    reconnect_initial_delay: float = 1.0
    reconnect_max_delay: float = 60.0
    reconnect_backoff_factor: float = 2.0

    def __post_init__(self) -> None:
        object.__setattr__(self, "state_dir", Path(self.state_dir))
        if self.socket_path is not None:
            object.__setattr__(self, "socket_path", Path(self.socket_path))
        if not self.agent_id:
            raise ValueError("CoreConfig.agent_id must be non-empty")
        if self.reconnect_initial_delay <= 0:
            raise ValueError("CoreConfig.reconnect_initial_delay must be > 0")
        if self.reconnect_max_delay < self.reconnect_initial_delay:
            raise ValueError(
                "CoreConfig.reconnect_max_delay must be >= reconnect_initial_delay"
            )
        if self.reconnect_backoff_factor < 1.0:
            raise ValueError("CoreConfig.reconnect_backoff_factor must be >= 1.0")

    @property
    def sidecar_socket_path(self) -> Path:
        """Effective sidecar socket path (``socket_path`` or the default)."""
        if self.socket_path is not None:
            return self.socket_path
        return self.state_dir / "sidecar.sock"
