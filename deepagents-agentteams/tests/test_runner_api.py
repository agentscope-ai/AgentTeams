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


async def test_bounded_file_operations_do_not_create_execution_state() -> None:
    with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
        Path(workspace, "nested").mkdir()  # noqa: ASYNC240 - setup happens before the first await.
        Path(workspace, "nested", "note.txt").write_text(  # noqa: ASYNC240 - setup happens before the first await.
            "alpha\nbeta\nalpha\n"
        )
        transport = httpx.ASGITransport(app=app_for(workspace, state))

        async with httpx.AsyncClient(transport=transport, base_url="http://runner.test") as client:
            listing = await client.post(
                "/v1/files/list",
                json={"path": "/workspace/nested"},
                headers=authorization_headers(),
            )
            grep = await client.post(
                "/v1/files/grep",
                json={
                    "pattern": "alpha",
                    "path": "/workspace",
                    "glob": "**/*.txt",
                    "max_count": 1,
                },
                headers=authorization_headers(),
            )
            glob = await client.post(
                "/v1/files/glob",
                json={"pattern": "**/*.txt", "path": "/workspace"},
                headers=authorization_headers(),
            )
            deletion = await client.post(
                "/v1/files/delete",
                json={"path": "/workspace/nested"},
                headers=authorization_headers(),
            )

        assert listing.status_code == 200
        assert listing.json()["entries"] == [
            {"path": "/workspace/nested/note.txt", "is_dir": False, "size": 17}
        ]
        assert grep.status_code == 200
        assert grep.json() == {
            "matches": [{"path": "/workspace/nested/note.txt", "line": 1, "text": "alpha"}],
            "error": None,
            "truncated": True,
        }
        assert glob.status_code == 200
        assert glob.json()["matches"] == [
            {"path": "/workspace/nested/note.txt", "is_dir": False, "size": 17}
        ]
        assert deletion.status_code == 200
        assert deletion.json() == {
            "path": "/workspace/nested",
            "error": None,
            "changes": [
                {"path": "nested/note.txt", "sha256": None, "size": 0, "deleted": True}
            ],
        }
        assert list(Path(state).iterdir()) == []  # noqa: ASYNC240 - assertion happens after the final await.


async def test_bounded_file_operations_reject_workspace_escape() -> None:
    with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
        transport = httpx.ASGITransport(app=app_for(workspace, state))

        async with httpx.AsyncClient(transport=transport, base_url="http://runner.test") as client:
            responses = [
                await client.post(
                    "/v1/files/list",
                    json={"path": "/etc"},
                    headers=authorization_headers(),
                ),
                await client.post(
                    "/v1/files/grep",
                    json={"pattern": "secret", "path": "/workspace/../etc"},
                    headers=authorization_headers(),
                ),
                await client.post(
                    "/v1/files/glob",
                    json={"pattern": "../*", "path": "/workspace"},
                    headers=authorization_headers(),
                ),
                await client.post(
                    "/v1/files/delete",
                    json={"path": "/workspace"},
                    headers=authorization_headers(),
                ),
            ]

        assert [response.status_code for response in responses] == [200, 200, 200, 200]
        assert [response.json()["error"] for response in responses] == [
            "invalid_path",
            "invalid_path",
            "invalid_path",
            "invalid_path",
        ]
