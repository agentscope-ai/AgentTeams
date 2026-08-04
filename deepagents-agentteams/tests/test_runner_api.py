import base64
import json
import tempfile
from pathlib import Path

import httpx

from deepagents_agentteams.runner import create_app
from deepagents_agentteams.runner_core import RunnerService


def runner_token() -> str:
    return "runner-test-credential"


def app_for(workspace: str, state: str):  # noqa: ANN201
    service = RunnerService(workspace=Path(workspace), state_dir=Path(state))
    return create_app(service=service, bearer_token=runner_token())


def authorization_headers() -> dict[str, str]:
    return {"Authorization": f"Bearer {runner_token()}"}


async def test_health_is_available_without_runner_credential() -> None:
    with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
        transport = httpx.ASGITransport(app=app_for(workspace, state))
        async with httpx.AsyncClient(transport=transport, base_url="http://runner.test") as client:
            response = await client.get("/healthz")

            assert response.status_code == 200
            assert response.json() == {"status": "ok"}


async def test_execution_requires_exact_bearer_credential() -> None:
    with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
        transport = httpx.ASGITransport(app=app_for(workspace, state))
        payload = {"request_id": "req-1", "command": "true", "timeout_seconds": 5}

        async with httpx.AsyncClient(transport=transport, base_url="http://runner.test") as client:
            assert (await client.post("/v1/execute", json=payload)).status_code == 401
            assert (
                await client.post(
                    "/v1/execute",
                    json=payload,
                    headers={"Authorization": "Bearer incorrect"},
                )
            ).status_code == 401
            assert (
                await client.post("/v1/execute", json=payload, headers=authorization_headers())
            ).status_code == 200


async def test_unknown_prior_execution_returns_conflict() -> None:
    with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
        Path(state, "req-unknown.json").write_text(  # noqa: ASYNC240 - setup happens before the first await.
            json.dumps({"request_id": "req-unknown", "status": "pending"})
        )
        transport = httpx.ASGITransport(app=app_for(workspace, state))

        async with httpx.AsyncClient(transport=transport, base_url="http://runner.test") as client:
            response = await client.post(
                "/v1/execute",
                json={"request_id": "req-unknown", "command": "true", "timeout_seconds": 5},
                headers=authorization_headers(),
            )

        assert response.status_code == 409
        assert response.json()["detail"] == "execution_result_unknown"


async def test_batch_file_transfer_reports_partial_success() -> None:
    with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
        transport = httpx.ASGITransport(app=app_for(workspace, state))
        encoded = base64.b64encode(b"payload").decode("ascii")

        async with httpx.AsyncClient(transport=transport, base_url="http://runner.test") as client:
            upload_response = await client.post(
                "/v1/files/upload",
                json={
                    "files": [
                        {"path": "/workspace/input.txt", "content_base64": encoded},
                        {"path": "/etc/escape.txt", "content_base64": encoded},
                    ]
                },
                headers=authorization_headers(),
            )

            assert upload_response.status_code == 200
            assert upload_response.json() == {
                "files": [
                    {"path": "/workspace/input.txt", "error": None},
                    {"path": "/etc/escape.txt", "error": "invalid_path"},
                ]
            }

            download_response = await client.post(
                "/v1/files/download",
                json={"paths": ["/workspace/input.txt", "/workspace/missing.txt"]},
                headers=authorization_headers(),
            )

            assert download_response.status_code == 200
            assert download_response.json() == {
                "files": [
                    {
                        "path": "/workspace/input.txt",
                        "content_base64": encoded,
                        "error": None,
                    },
                    {
                        "path": "/workspace/missing.txt",
                        "content_base64": None,
                        "error": "file_not_found",
                    },
                ]
            }
