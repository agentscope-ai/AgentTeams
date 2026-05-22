"""Matrix relay using matrix-nio."""
from __future__ import annotations

import asyncio
import logging
import time
from typing import Callable, Awaitable, Optional

from nio import AsyncClient, RoomMessageText, RoomSendError
from nio.events.invite_events import InviteMemberEvent

from harness_worker.policies import DualAllowList, HistoryBuffer, apply_outbound_mentions

logger = logging.getLogger(__name__)

# Ignore events older than this at startup (avoid replaying history)
_STARTUP_GRACE_MS = 10_000


class MatrixRelay:
    """Plain Matrix client relay that invokes a harness on each message."""

    def __init__(
        self,
        client: AsyncClient,
        policies: DualAllowList,
        history: HistoryBuffer,
        user_id: str,
        harness,
        harness_home,
        workspace_dir,
        on_invoke: Callable[[str, Optional[str]], Awaitable[tuple[str, Optional[str]]]],
    ) -> None:
        self.client = client
        self.policies = policies
        self.history = history
        self.user_id = user_id
        self.harness = harness
        self.harness_home = harness_home
        self.workspace_dir = workspace_dir
        self.on_invoke = on_invoke
        self._running = False
        # Events before this timestamp (ms) are considered "old" and skipped
        self._start_ms = int(time.time() * 1000) - _STARTUP_GRACE_MS

    async def run(self) -> None:
        self._running = True

        # Register event callbacks before entering the sync loop
        self.client.add_event_callback(self._handle_room_message, RoomMessageText)
        self.client.add_event_callback(self._handle_invite, InviteMemberEvent)

        await self.client.sync_forever(timeout=30000, full_state=False)

    async def stop(self) -> None:
        self._running = False
        await self.client.close()

    async def _handle_invite(self, room, event) -> None:
        """Auto-accept room invites."""
        if event.membership != "invite":
            return
        logger.info("Received invite to %s — joining", room.room_id)
        try:
            await self.client.join(room.room_id)
        except Exception as exc:
            logger.error("Failed to join room %s: %s", room.room_id, exc)

    async def _handle_room_message(self, room, event) -> None:
        """Process one Matrix room message event."""
        try:
            await self._process_message(room, event)
        except Exception as exc:
            logger.error("Error processing message in %s: %s", room.room_id, exc)

    async def _process_message(self, room, event) -> None:
        # Skip own messages
        if event.sender == self.user_id:
            return

        # Skip events that arrived before we started (replay from history)
        if hasattr(event, "server_timestamp") and event.server_timestamp < self._start_ms:
            return

        body = getattr(event, "body", "") or ""
        if not body:
            return

        # DM heuristic: room with ≤2 joined members
        is_dm = getattr(room, "joined_count", room.member_count if hasattr(room, "member_count") else 2) <= 2
        if not self.policies.permits(event.sender, is_dm):
            if not is_dm:
                self.history.record(room.room_id, event.sender, body)
            return

        context = ""
        if not is_dm:
            context = self.history.drain(room.room_id)

        full_message = context + body

        async def _typing_keepalive(room_id: str) -> None:
            """Renew typing indicator every 30 s so it doesn't expire mid-run."""
            while True:
                try:
                    await self.client.room_typing(room_id, typing_state=True, timeout=40_000)
                except Exception:
                    pass
                await asyncio.sleep(30)

        keepalive = asyncio.create_task(_typing_keepalive(room.room_id))
        try:
            reply, new_sid = await self.on_invoke(full_message, None)
        except Exception as exc:
            logger.error("invoke failed: %s", exc)
            reply = f"Sorry, an error occurred: {exc}"
        finally:
            keepalive.cancel()
            try:
                await self.client.room_typing(room.room_id, typing_state=False)
            except Exception:
                pass

        content = {"msgtype": "m.text", "body": reply}
        apply_outbound_mentions(content, self_user_id=self.user_id)

        try:
            await self.client.room_send(
                room.room_id,
                "m.room.message",
                content,
            )
        except RoomSendError as exc:
            logger.error("room_send failed: %s", exc)
