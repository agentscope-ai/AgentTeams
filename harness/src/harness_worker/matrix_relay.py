"""Matrix relay for harness-worker — thin adapter over hiclaw_common.matrix."""
from __future__ import annotations

from typing import Awaitable, Callable, Optional

from hiclaw_common.matrix import MautrixRelay
from hiclaw_common.policies import DualAllowList, HistoryBuffer

__all__ = ["MatrixRelay"]


class MatrixRelay:
    """Thin harness-worker adapter wrapping MautrixRelay."""

    def __init__(
        self,
        homeserver: str,
        user_id: str,
        access_token: str,
        device_id: str,
        policies: DualAllowList,
        history: HistoryBuffer,
        on_invoke: Callable[[str], Awaitable[tuple[str, Optional[str]]]],
        # Legacy keyword args kept for call-site compatibility; unused.
        harness=None,
        harness_home=None,
        workspace_dir=None,
        client=None,
    ) -> None:
        self._relay = MautrixRelay(
            homeserver=homeserver,
            user_id=user_id,
            access_token=access_token,
            device_id=device_id,
            policies=policies,
            history=history,
            on_invoke=on_invoke,
        )

    async def run(self) -> None:
        await self._relay.run()

    async def stop(self) -> None:
        await self._relay.stop()
