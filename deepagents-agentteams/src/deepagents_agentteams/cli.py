"""DeepAgents AgentTeams Worker process entrypoint."""

from __future__ import annotations

import asyncio
import logging
import os
from collections.abc import Mapping
from pathlib import Path
from typing import Any

import httpx

from deepagents_agentteams.bootstrap import fetch_runtime_document
from deepagents_agentteams.checkpoints import async_postgres_checkpointer
from deepagents_agentteams.config import RuntimeConfig
from deepagents_agentteams.engine import AgentEngine, PendingApprovalStore
from deepagents_agentteams.graph import build_deepagents_graph
from deepagents_agentteams.matrix import ControllerMatrixTokenProvider, MatrixMessage, MatrixTransport
from deepagents_agentteams.sandbox import AgentTeamsSandbox, SandboxControlClient
from deepagents_agentteams.workspace import MinIOWorkspaceStore

_LOGGER = logging.getLogger(__name__)


async def run_worker() -> None:
    """Bootstrap the projected runtime and serve Matrix messages until cancelled."""
    document = await _fetch_runtime_with_retry()
    config = RuntimeConfig.from_document(document, environ=os.environ)
    state_dir = Path(
        os.environ.get(
            "AGENTTEAMS_DEEPAGENTS_STATE_DIR",
            "/var/lib/agentteams/deepagents",
        )
    )
    await asyncio.to_thread(state_dir.mkdir, parents=True, exist_ok=True)
    workspace_store = MinIOWorkspaceStore.from_config(config.storage)
    runner_http = httpx.Client(timeout=130, trust_env=False)
    controller_http = httpx.AsyncClient(timeout=30, trust_env=False)
    matrix_token_provider = ControllerMatrixTokenProvider(
        controller_url=config.controller_url,
        service_account_token_path=Path(config.service_account_token_path),
        client=controller_http,
    )
    sandboxes: list[AgentTeamsSandbox] = []

    async with async_postgres_checkpointer(config.checkpoint.dsn, config.checkpoint.aes_key) as checkpointer:

        async def graph_factory(thread_id: str):  # noqa: ANN202
            backend = None
            if config.execution.mode == "sandbox":
                if not config.controller_url:
                    raise RuntimeError("AGENTTEAMS_CONTROLLER_URL is required for sandbox execution")
                control = SandboxControlClient(
                    controller_url=config.controller_url,
                    worker_name=config.worker_name,
                    service_account_token_path=Path(config.service_account_token_path),
                    client=runner_http,
                )
                sandbox = AgentTeamsSandbox(
                    control=control,
                    session_id=thread_id,
                    client=runner_http,
                    workspace_store=workspace_store,
                )
                sandboxes.append(sandbox)
                backend = sandbox
            return await build_deepagents_graph(
                config,
                backend=backend,
                checkpointer=checkpointer,
            )

        transport: MatrixTransport

        async def send_reply(message: MatrixMessage, body: str) -> None:
            await transport.send_reply(message, body)

        engine = AgentEngine(
            config=config,
            graph_factory=graph_factory,
            send_reply=send_reply,
            pending_store=PendingApprovalStore(state_dir / "pending-approvals.json"),
        )
        transport = MatrixTransport(
            config=config.matrix,
            allowed_room_ids=frozenset(config.room_ids),
            state_dir=state_dir / "matrix",
            on_message=engine.handle_message,
            refresh_access_token=matrix_token_provider.refresh,
        )
        try:
            await transport.run_forever()
        finally:
            for sandbox in sandboxes:
                try:
                    await asyncio.to_thread(sandbox.close)
                except Exception:  # noqa: BLE001 - shutdown cleanup is best-effort.
                    _LOGGER.exception("failed to release execution sandbox during shutdown")
            await asyncio.to_thread(runner_http.close)
            await controller_http.aclose()


async def _fetch_runtime_with_retry() -> Mapping[str, Any]:
    retries = int(os.environ.get("AGENTTEAMS_RUNTIME_CONFIG_RETRIES", "12"))
    delay_seconds = float(os.environ.get("AGENTTEAMS_RUNTIME_CONFIG_RETRY_SECONDS", "5"))
    last_error: Exception | None = None
    for attempt in range(1, retries + 1):
        try:
            return fetch_runtime_document(os.environ)
        except Exception as exc:  # noqa: BLE001 - MinIO SDK exceptions vary by transport/provider.
            last_error = exc
            if attempt == retries:
                break
            _LOGGER.warning("runtime configuration is not ready (attempt %d/%d)", attempt, retries)
            await asyncio.sleep(delay_seconds)
    raise RuntimeError("runtime configuration did not become available") from last_error


def main() -> None:
    """Configure logging and run the asynchronous Worker."""
    logging.basicConfig(
        level=os.environ.get("AGENTTEAMS_LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    asyncio.run(run_worker())


if __name__ == "__main__":
    main()
