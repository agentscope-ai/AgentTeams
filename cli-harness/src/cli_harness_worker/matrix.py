"""Matrix loop for the CLI-harness worker.

The CLI agents have no Matrix support of their own; this module is the
transport. Contract:

  - Own room (AGENTTEAMS_WORKER_ROOM_ID): every non-self message is a task.
  - Other rooms (team rooms): only messages that @mention this worker are
    tasks; everything else is observed background chatter.
  - Each task is dispatched to the configured CLI adapter and the final
    output is sent back into the originating room.
"""
from __future__ import annotations

import asyncio
import logging
from typing import Awaitable, Callable, Optional

from nio import (
    AsyncClient,
    AsyncClientConfig,
    JoinResponse,
    LoginResponse,
    RoomMessageText,
)

logger = logging.getLogger(__name__)

# Matrix events cap out well below this; keep chunks conservative so a long
# task result arrives as a handful of readable messages.
MAX_MESSAGE_CHARS = 8000

# Full MXID mention check for team rooms.
def _mentions_user(body: str, user_id: str) -> bool:
    if not user_id:
        return False
    localpart = user_id.split(":", 1)[0].lstrip("@")
    return f"@{localpart}:" in body or user_id in body


def _split_message(text: str, limit: int = MAX_MESSAGE_CHARS) -> list[str]:
    if len(text) <= limit:
        return [text]
    chunks: list[str] = []
    while text:
        chunk = text[:limit]
        if len(text) > limit:
            # Prefer breaking on a newline inside the window.
            cut = chunk.rfind("\n")
            if cut > limit // 2:
                chunk = chunk[:cut]
        chunks.append(chunk)
        text = text[len(chunk):].lstrip("\n")
    return chunks


class MatrixLoop:
    """Owns the nio AsyncClient and the message dispatch loop."""

    def __init__(
        self,
        homeserver: str,
        user_id: str,
        access_token: str,
        own_room_id: str,
        on_task: Callable[[str, str, str], Awaitable[None]],
        password: Optional[str] = None,
        store_path: Optional[str] = None,
    ) -> None:
        # ``on_task(room_id, sender, body)`` runs the CLI and returns the reply
        # text (or raises); MatrixLoop handles chunking + delivery.
        self.user_id = user_id
        self.own_room_id = own_room_id
        self.on_task = on_task
        self.password = (password or "").strip() or None
        self.client = AsyncClient(
            homeserver,
            user_id,
            config=AsyncClientConfig(max_timeouts=3),
            store_path=store_path,
        )
        # matrix-nio takes the token via attribute, not the constructor.
        if access_token:
            self.client.access_token = access_token
        self._seen_event_ids: set[str] = set()
        self._queue: asyncio.Queue = asyncio.Queue()
        self._worker_task: Optional[asyncio.Task] = None

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def login(self) -> None:
        if self.password:
            response = await self.client.login(password=self.password)
            if isinstance(response, LoginResponse):
                logger.info("Matrix login OK (device=%s)", response.device_id)
                return
            logger.warning("Matrix password login failed: %s", response)
        if not self.client.access_token:
            raise RuntimeError(
                "Matrix authentication failed: no password and no access token"
            )
        logger.info("Using existing Matrix access token")

    async def join_own_room(self) -> None:
        if not self.own_room_id:
            logger.warning("No AGENTTEAMS_WORKER_ROOM_ID configured")
            return
        response = await self.client.join(self.own_room_id)
        if isinstance(response, JoinResponse):
            logger.info("Joined own room %s", self.own_room_id)
        else:
            logger.warning(
                "Join own room %s failed: %s", self.own_room_id, response
            )

    async def run(self) -> None:
        self._worker_task = asyncio.create_task(self._task_worker())
        # sync_forever retries internally; it only returns on fatal errors.
        await self.client.sync_forever(timeout=30000, full_state=True)

    async def close(self) -> None:
        if self._worker_task:
            self._worker_task.cancel()
        await self.client.close()

    # ------------------------------------------------------------------
    # Sync callbacks
    # ------------------------------------------------------------------

    async def _on_message(self, room, event: RoomMessageText) -> None:
        if event.sender == self.user_id:
            return
        if event.event_id in self._seen_event_ids:
            return
        self._seen_event_ids.add(event.event_id)
        if len(self._seen_event_ids) > 2048:
            # Bound memory; recent IDs matter most.
            self._seen_event_ids = set(list(self._seen_event_ids)[-1024:])

        body = (event.body or "").strip()
        if not body or body.startswith(">"):
            # Skip empty bodies and quote-reply scaffolding.
            return

        if room.room_id == self.own_room_id:
            await self._queue.put((room.room_id, event.sender, body))
            return

        if _mentions_user(body, self.user_id):
            await self._queue.put((room.room_id, event.sender, body))

    async def _task_worker(self) -> None:
        while True:
            room_id, sender, body = await self._queue.get()
            try:
                reply = await self.on_task(room_id, sender, body)
                await self._send_reply(room_id, reply)
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                logger.exception("Task failed")
                try:
                    await self._send_reply(room_id, f"task failed: {exc}")
                except Exception:
                    logger.exception("Failed to report task failure")
            finally:
                self._queue.task_done()

    async def _send_reply(self, room_id: str, text: str) -> None:
        reply = (text or "").strip()
        if not reply:
            return
        for chunk in _split_message(reply):
            await self.client.room_send(
                room_id=room_id,
                message_type="m.room.message",
                content={"msgtype": "m.text", "body": chunk},
            )


def attach_callbacks(loop: MatrixLoop) -> MatrixLoop:
    """Register the message callback on the underlying nio client."""
    loop.client.add_event_callback(loop._on_message, RoomMessageText)
    return loop
