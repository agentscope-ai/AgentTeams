"""Matrix transport for DeepAgents Worker messages and approval replies."""

from __future__ import annotations

import asyncio
import html
import logging
from collections.abc import Awaitable, Callable
from contextlib import suppress
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx
from nio import (
    AsyncClient,
    AsyncClientConfig,
    JoinResponse,
    RoomMessageText,
    RoomSendResponse,
    SyncResponse,
    WhoamiResponse,
)

from deepagents_agentteams.config import MatrixConfig

_LOGGER = logging.getLogger(__name__)


class MatrixAccessTokenExpired(RuntimeError):
    """Raised internally when Matrix rejects an access token."""


class ControllerMatrixTokenProvider:
    """Refresh a Matrix token with the Worker's rotating ServiceAccount token."""

    def __init__(
        self,
        *,
        controller_url: str,
        service_account_token_path: Path,
        client: httpx.AsyncClient,
    ) -> None:
        if not controller_url:
            raise ValueError("controller URL is required for Matrix token refresh")
        self._controller_url = controller_url.rstrip("/")
        self._service_account_token_path = service_account_token_path
        self._client = client

    async def refresh(self) -> str:
        """Issue a fresh Matrix token without caching the ServiceAccount token."""
        service_account_token = self._service_account_token_path.read_text().strip()
        if not service_account_token:
            raise RuntimeError("ServiceAccount token is empty")
        response = await self._client.post(
            f"{self._controller_url}/api/v1/credentials/matrix-token",
            headers={"Authorization": f"Bearer {service_account_token}"},
        )
        response.raise_for_status()
        access_token = response.json().get("access_token")
        if not isinstance(access_token, str) or not access_token:
            raise RuntimeError("controller returned an invalid Matrix access token")
        return access_token


@dataclass(frozen=True)
class MatrixMessage:
    """Normalized inbound Matrix text event."""

    room_id: str
    event_id: str
    thread_root_event_id: str
    sender: str
    body: str


def matrix_message_from_event(
    room: Any,
    event: Any,
    *,
    allowed_room_ids: frozenset[str],
    own_user_id: str,
) -> MatrixMessage | None:
    """Validate and normalize one matrix-nio text event."""
    room_id = str(getattr(room, "room_id", ""))
    sender = str(getattr(event, "sender", ""))
    event_id = str(getattr(event, "event_id", ""))
    body = str(getattr(event, "body", "") or "").strip()
    if room_id not in allowed_room_ids or sender == own_user_id or not sender or not event_id or not body:
        return None
    source = getattr(event, "source", {})
    content = source.get("content", {}) if isinstance(source, dict) else {}
    relation = content.get("m.relates_to", {}) if isinstance(content, dict) else {}
    thread_root = event_id
    if isinstance(relation, dict) and relation.get("rel_type") == "m.thread":
        related_event_id = relation.get("event_id")
        if isinstance(related_event_id, str) and related_event_id:
            thread_root = related_event_id
    return MatrixMessage(
        room_id=room_id,
        event_id=event_id,
        thread_root_event_id=thread_root,
        sender=sender,
        body=body,
    )


class MatrixTransport:
    """Long-running matrix-nio client restricted to projected AgentTeams rooms."""

    def __init__(
        self,
        *,
        config: MatrixConfig,
        allowed_room_ids: frozenset[str],
        state_dir: Path,
        on_message: Callable[[MatrixMessage], Awaitable[None]],
        refresh_access_token: Callable[[], Awaitable[str]] | None = None,
        client_factory: Callable[..., Any] = AsyncClient,
    ) -> None:
        if not allowed_room_ids:
            raise ValueError("at least one Matrix room must be allowed")
        self._config = config
        self._allowed_room_ids = allowed_room_ids
        self._state_dir = state_dir
        self._on_message = on_message
        self._refresh_access_token = refresh_access_token
        self._client_factory = client_factory
        self.client: Any | None = None
        self._event_tasks: set[asyncio.Task[None]] = set()

    async def run_forever(self) -> None:
        """Authenticate, catch up without replay, then process incremental events."""
        await self._connect()
        try:
            await self._sync_loop()
        finally:
            for task in self._event_tasks:
                task.cancel()
            if self._event_tasks:
                await asyncio.gather(*self._event_tasks, return_exceptions=True)
            if self.client is not None:
                await self.client.close()

    async def send_reply(self, message: MatrixMessage, body: str) -> None:
        """Send a text reply in the same Matrix thread with a structured mention."""
        if self.client is None:
            raise RuntimeError("Matrix transport is not connected")
        content: dict[str, Any] = {
            "msgtype": "m.text",
            "body": body,
            "format": "org.matrix.custom.html",
            "formatted_body": html.escape(body).replace("\n", "<br>"),
            "m.mentions": {"user_ids": [message.sender]},
            "m.relates_to": {
                "rel_type": "m.thread",
                "event_id": message.thread_root_event_id,
                "is_falling_back": True,
                "m.in_reply_to": {"event_id": message.event_id},
            },
        }
        response = await self.client.room_send(
            room_id=message.room_id,
            message_type="m.room.message",
            content=content,
            ignore_unverified_devices=True,
        )
        if _is_unauthorized(response):
            await self._refresh_client_token()
            response = await self.client.room_send(
                room_id=message.room_id,
                message_type="m.room.message",
                content=content,
                ignore_unverified_devices=True,
            )
        _require_matrix_response(response, RoomSendResponse, "Matrix room_send")

    async def _connect(self) -> None:
        self._state_dir.mkdir(parents=True, exist_ok=True)
        if self._config.encryption_enabled:
            nio_config = AsyncClientConfig(store_sync_tokens=False, encryption_enabled=True)
            self.client = self._client_factory(
                self._config.homeserver_url,
                user="",
                store_path=str(self._state_dir / "e2ee"),
                config=nio_config,
            )
        else:
            self.client = self._client_factory(self._config.homeserver_url, user="")
        self.client.access_token = self._config.access_token
        whoami = await self.client.whoami()
        if _is_unauthorized(whoami) and self._refresh_access_token is not None:
            await self._refresh_client_token()
            whoami = await self.client.whoami()
        if not isinstance(whoami, WhoamiResponse):
            raise RuntimeError(f"Matrix access token validation failed: {whoami}")
        if whoami.user_id != self._config.user_id:
            raise RuntimeError("Matrix access token resolved to an unexpected user")
        self.client.user_id = whoami.user_id
        self.client.user = whoami.user_id
        if whoami.device_id:
            self.client.device_id = whoami.device_id
        if self._config.encryption_enabled:
            if not self.client.device_id:
                raise RuntimeError("Matrix E2EE requires an access token with a device ID")
            self.client.load_store()
            if self.client.should_upload_keys:
                await self.client.keys_upload()
        self.client.add_event_callback(self._schedule_message, (RoomMessageText,))

    async def _sync_loop(self) -> None:
        next_batch = self._load_sync_token()
        while True:
            try:
                if next_batch is None:
                    response = await self._initial_sync_without_replay()
                else:
                    response = await self.client.sync(
                        timeout=30_000,
                        since=next_batch,
                        full_state=False,
                    )
                next_batch = await self._accept_sync_response(response)
            except asyncio.CancelledError:
                raise
            except MatrixAccessTokenExpired:
                await self._refresh_client_token()
            except Exception:
                _LOGGER.exception("Matrix sync failed; retrying")
                await asyncio.sleep(5)

    async def _initial_sync_without_replay(self) -> Any:
        callbacks = list(self.client.event_callbacks)
        self.client.event_callbacks.clear()
        # matrix-nio otherwise falls back to its in-memory next_batch when
        # ``since`` is omitted. Clear it so a failed invite join can replay the
        # same full-state response without replaying message callbacks.
        self.client.next_batch = None
        try:
            return await self.client.sync(timeout=30_000, full_state=True)
        finally:
            self.client.event_callbacks.extend(callbacks)

    async def _accept_sync_response(self, response: Any) -> str:
        if not isinstance(response, SyncResponse):
            if _is_unauthorized(response):
                raise MatrixAccessTokenExpired("Matrix access token expired")
            raise RuntimeError(f"Matrix sync failed: {response}")
        for room_id in response.rooms.invite:
            if room_id not in self._allowed_room_ids:
                _LOGGER.warning("ignoring Matrix invitation for an unprojected room")
                continue
            join_response = await self.client.join(room_id)
            if _is_unauthorized(join_response):
                await self._refresh_client_token()
                join_response = await self.client.join(room_id)
            _require_matrix_response(join_response, JoinResponse, "Matrix room join")
        await self._e2ee_maintenance()
        self._save_sync_token(response.next_batch)
        return response.next_batch

    async def _refresh_client_token(self) -> None:
        if self._refresh_access_token is None or self.client is None:
            raise MatrixAccessTokenExpired("Matrix token refresh is unavailable")
        self.client.access_token = await self._refresh_access_token()

    async def _e2ee_maintenance(self) -> None:
        if not self._config.encryption_enabled or not self.client.olm:
            return
        if self.client.should_upload_keys:
            await self.client.keys_upload()
        if self.client.should_query_keys:
            await self.client.keys_query()
        if self.client.should_claim_keys:
            await self.client.keys_claim(self.client.get_users_for_key_claiming())
        await self.client.send_to_device_messages()

    async def _schedule_message(self, room: Any, event: Any) -> None:
        message = matrix_message_from_event(
            room,
            event,
            allowed_room_ids=self._allowed_room_ids,
            own_user_id=self._config.user_id,
        )
        if message is None:
            return
        with suppress(Exception):
            await self.client.room_read_markers(
                message.room_id,
                fully_read_event=message.event_id,
                read_event=message.event_id,
            )
        task = asyncio.create_task(self._on_message(message))
        self._event_tasks.add(task)
        task.add_done_callback(self._event_tasks.discard)

    def _load_sync_token(self) -> str | None:
        path = self._state_dir / "sync-token"
        if not path.exists():
            return None
        token = path.read_text().strip()
        return token or None

    def _save_sync_token(self, token: str) -> None:
        path = self._state_dir / "sync-token"
        temporary = path.with_suffix(".tmp")
        temporary.write_text(token)
        temporary.chmod(0o600)
        temporary.replace(path)


def _is_unauthorized(response: Any) -> bool:
    status = getattr(response, "status_code", None)
    errcode = getattr(response, "errcode", None)
    transport_response = getattr(response, "transport_response", None)
    http_status = getattr(transport_response, "status", None)
    return status in {401, "401", "M_UNKNOWN_TOKEN", "M_UNAUTHORIZED"} or errcode in {
        "M_UNKNOWN_TOKEN",
        "M_UNAUTHORIZED",
    } or http_status == 401


def _require_matrix_response(response: Any, expected_type: type[Any], operation: str) -> None:
    if isinstance(response, expected_type):
        return
    if _is_unauthorized(response):
        raise MatrixAccessTokenExpired(f"{operation} failed because the Matrix access token expired")
    raise RuntimeError(f"{operation} failed with {type(response).__name__}")
