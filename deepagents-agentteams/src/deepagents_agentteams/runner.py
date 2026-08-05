"""Authenticated HTTP API for the credential-free execution runner."""

import base64
import binascii
import ctypes
import os
import resource
import secrets
import sys
from dataclasses import asdict
from pathlib import Path
from typing import Annotated

import uvicorn
from fastapi import Depends, FastAPI, HTTPException
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
from pydantic import BaseModel, Field

from deepagents_agentteams.runner_core import (
    FileTooLarge,
    InvalidWorkspacePath,
    RunnerService,
    UnknownExecutionResult,
)

_PR_SET_DUMPABLE = 4


def consume_and_harden_runner_token() -> str:
    """Remove the runner token from the environment and disable process dumps."""
    bearer_token = os.environ.pop("AGENTTEAMS_RUNNER_TOKEN", "")
    if not bearer_token:
        raise RuntimeError("AGENTTEAMS_RUNNER_TOKEN must be configured")
    if sys.platform != "linux":
        raise RuntimeError("runner process hardening requires Linux")

    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    library = ctypes.CDLL(None, use_errno=True)
    prctl = library.prctl
    prctl.argtypes = [ctypes.c_int, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_ulong, ctypes.c_ulong]
    prctl.restype = ctypes.c_int
    if prctl(_PR_SET_DUMPABLE, 0, 0, 0, 0) != 0:
        error_number = ctypes.get_errno()
        raise OSError(error_number, "failed to disable process dumpability")
    return bearer_token


class ExecuteRequest(BaseModel):
    """One idempotent command execution request."""

    request_id: str = Field(min_length=1, max_length=128)
    command: str = Field(min_length=1, max_length=256 * 1024)
    timeout_seconds: int = Field(ge=1, le=3600)


class UploadFileRequest(BaseModel):
    """One base64-encoded file upload."""

    path: str = Field(min_length=1, max_length=4096)
    content_base64: str


class UploadFilesRequest(BaseModel):
    """Bounded file upload batch."""

    files: list[UploadFileRequest] = Field(min_length=1, max_length=128)


class DownloadFilesRequest(BaseModel):
    """Bounded file download batch."""

    paths: list[str] = Field(min_length=1, max_length=128)


class UploadFileResult(BaseModel):
    """Per-file upload result."""

    path: str
    error: str | None = None


class DownloadFileResult(BaseModel):
    """Per-file download result."""

    path: str
    content_base64: str | None = None
    error: str | None = None


def create_app(*, service: RunnerService, bearer_token: str) -> FastAPI:
    """Build the runner API around one workspace-scoped service."""
    if not bearer_token:
        raise ValueError("runner bearer token must be non-empty")
    app = FastAPI(title="AgentTeams DeepAgents Runner", docs_url=None, redoc_url=None)
    security = HTTPBearer(auto_error=False)

    async def authorize(
        credentials: Annotated[HTTPAuthorizationCredentials | None, Depends(security)],
    ) -> None:
        if (
            credentials is None
            or credentials.scheme.casefold() != "bearer"
            or not secrets.compare_digest(credentials.credentials, bearer_token)
        ):
            raise HTTPException(status_code=401, detail="invalid_runner_credential")

    @app.get("/healthz")
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    @app.post("/v1/execute", dependencies=[Depends(authorize)])
    async def execute(request: ExecuteRequest) -> dict[str, object]:
        try:
            result = service.execute(
                request_id=request.request_id,
                command=request.command,
                timeout_seconds=request.timeout_seconds,
            )
        except UnknownExecutionResult as exc:
            raise HTTPException(status_code=409, detail="execution_result_unknown") from exc
        except ValueError as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc
        return asdict(result)

    @app.post("/v1/files/upload", dependencies=[Depends(authorize)])
    async def upload_files(request: UploadFilesRequest) -> dict[str, list[UploadFileResult]]:
        results: list[UploadFileResult] = []
        for file in request.files:
            try:
                content = base64.b64decode(file.content_base64, validate=True)
                service.upload_file(path=file.path, content=content)
                results.append(UploadFileResult(path=file.path))
            except binascii.Error:
                results.append(UploadFileResult(path=file.path, error="invalid_content"))
            except FileTooLarge:
                results.append(UploadFileResult(path=file.path, error="file_too_large"))
            except (InvalidWorkspacePath, ValueError):
                results.append(UploadFileResult(path=file.path, error="invalid_path"))
            except PermissionError:
                results.append(UploadFileResult(path=file.path, error="permission_denied"))
            except OSError:
                results.append(UploadFileResult(path=file.path, error="io_error"))
        return {"files": results}

    @app.post("/v1/files/download", dependencies=[Depends(authorize)])
    async def download_files(request: DownloadFilesRequest) -> dict[str, list[DownloadFileResult]]:
        results: list[DownloadFileResult] = []
        for path in request.paths:
            try:
                content = service.download_file(path=path)
                results.append(
                    DownloadFileResult(
                        path=path,
                        content_base64=base64.b64encode(content).decode("ascii"),
                    )
                )
            except InvalidWorkspacePath:
                results.append(DownloadFileResult(path=path, error="invalid_path"))
            except FileNotFoundError:
                results.append(DownloadFileResult(path=path, error="file_not_found"))
            except IsADirectoryError:
                results.append(DownloadFileResult(path=path, error="is_directory"))
            except PermissionError:
                results.append(DownloadFileResult(path=path, error="permission_denied"))
            except FileTooLarge:
                results.append(DownloadFileResult(path=path, error="file_too_large"))
            except OSError:
                results.append(DownloadFileResult(path=path, error="io_error"))
        return {"files": results}

    return app


def main() -> None:
    """Start the runner HTTP service from environment configuration."""
    bearer_token = consume_and_harden_runner_token()
    service = RunnerService(
        workspace=Path(os.environ.get("AGENTTEAMS_RUNNER_WORKSPACE", "/workspace")),
        state_dir=Path(
            os.environ.get(
                "AGENTTEAMS_RUNNER_STATE_DIR",
                "/tmp/agentteams-runner-state",  # noqa: S108 - /tmp is a dedicated Pod emptyDir mount.
            )
        ),
        max_output_bytes=int(os.environ.get("AGENTTEAMS_RUNNER_MAX_OUTPUT_BYTES", str(512 * 1024))),
        max_file_bytes=int(os.environ.get("AGENTTEAMS_RUNNER_MAX_FILE_BYTES", str(10 * 1024 * 1024))),
    )
    app = create_app(service=service, bearer_token=bearer_token)
    uvicorn.run(
        app,
        host="0.0.0.0",  # noqa: S104 - the Pod NetworkPolicy and bearer token form the network boundary.
        port=int(os.environ.get("AGENTTEAMS_RUNNER_PORT", "8080")),
        access_log=False,
    )


if __name__ == "__main__":
    main()
