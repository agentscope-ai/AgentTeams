from contextlib import asynccontextmanager
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest

from deepagents_agentteams import cli


async def test_worker_resets_stale_readiness_and_signals_only_from_post_sync_callback(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    readiness_path = tmp_path / "agentteams-deepagents-ready"
    readiness_path.write_text("stale")
    monkeypatch.setattr(cli, "_READINESS_PATH", readiness_path)
    monkeypatch.setenv("AGENTTEAMS_DEEPAGENTS_STATE_DIR", str(tmp_path / "state"))

    async def runtime_document() -> dict:
        return {}

    monkeypatch.setattr(cli, "_fetch_runtime_with_retry", runtime_document)
    config = SimpleNamespace(
        storage=object(),
        controller_url="http://controller:8090",
        worker_name="researcher",
        service_account_token_path="/var/run/secrets/agentteams/token",  # noqa: S106
        checkpoint=SimpleNamespace(dsn="postgresql://checkpoint", aes_key="a" * 32),
        matrix=object(),
        room_ids=("!room:example.org",),
        execution=SimpleNamespace(mode="disabled"),
    )
    monkeypatch.setattr(cli.RuntimeConfig, "from_document", lambda *_args, **_kwargs: config)
    monkeypatch.setattr(cli.MinIOWorkspaceStore, "from_config", lambda _config: object())

    runner_http = SimpleNamespace(close=Mock())
    controller_http = SimpleNamespace(aclose=AsyncMock())
    monkeypatch.setattr(
        cli.httpx,
        "Client",
        lambda **_kwargs: runner_http,
    )
    monkeypatch.setattr(
        cli.httpx,
        "AsyncClient",
        lambda **_kwargs: controller_http,
    )
    monkeypatch.setattr(
        cli,
        "ControllerMatrixTokenProvider",
        lambda **_kwargs: SimpleNamespace(refresh=AsyncMock()),
    )
    monkeypatch.setattr(cli, "AgentEngine", lambda **_kwargs: SimpleNamespace(handle_message=AsyncMock()))

    @asynccontextmanager
    async def checkpointer(_dsn: str, _key: str):
        yield object()

    monkeypatch.setattr(cli, "async_postgres_checkpointer", checkpointer)

    class FakeTransport:
        def __init__(self, **kwargs) -> None:  # noqa: ANN003
            self._on_synchronized = kwargs["on_synchronized"]

        async def run_forever(self) -> None:
            assert not readiness_path.exists()
            await self._on_synchronized()
            assert readiness_path.read_text() == ""

        async def send_reply(self, *_args, **_kwargs) -> None:  # noqa: ANN002, ANN003
            return None

    monkeypatch.setattr(cli, "MatrixTransport", FakeTransport)

    await cli.run_worker()

    assert readiness_path.exists()
    runner_http.close.assert_called_once_with()
    controller_http.aclose.assert_awaited_once_with()
