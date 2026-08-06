"""DeepAgents sandbox backend backed by AgentTeams ExecutionSandbox resources."""

from __future__ import annotations

import asyncio
import base64
import binascii
import hashlib
import logging
import re
import time
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol
from urllib.parse import quote, urlsplit

import httpx
from deepagents.backends.protocol import (
    DeleteResult,
    EditResult,
    ExecuteResponse,
    FileData,
    FileDownloadResponse,
    FileUploadResponse,
    GlobResult,
    GrepResult,
    LsResult,
    ReadResult,
    WriteResult,
)
from deepagents.backends.sandbox import BaseSandbox
from deepagents.backends.utils import (
    _get_backend_read_file_type,
    check_empty_content,
    perform_string_replacement,
    slice_read_response,
)

from deepagents_agentteams.runner_core import WorkspaceChange

_SESSION_ID_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}\Z")
_LOGGER = logging.getLogger(__name__)


@dataclass(frozen=True)
class SandboxLease:
    """Ready runner endpoint and its short-lived capability credential."""

    name: str
    endpoint: str
    token: str


class WorkspaceStore(Protocol):
    """Worker-side durable storage boundary used by a sandbox backend."""

    def hydrate(self, sandbox: BaseSandbox) -> None: ...

    def persist_changes(self, sandbox: BaseSandbox, changes: tuple[WorkspaceChange, ...]) -> None: ...


class SandboxControlClient:
    """Self-service client for the controller's ExecutionSandbox API."""

    def __init__(
        self,
        *,
        controller_url: str,
        worker_name: str,
        service_account_token_path: Path,
        client: httpx.Client | None = None,
        ready_timeout_seconds: float = 120,
        poll_interval_seconds: float = 1,
    ) -> None:
        self._controller_url = _validated_base_url(controller_url, "controller_url")
        if not worker_name:
            raise ValueError("worker_name must be non-empty")
        self._worker_name = worker_name
        self._token_path = service_account_token_path
        self._client = client or httpx.Client(timeout=10, trust_env=False)
        self._ready_timeout_seconds = ready_timeout_seconds
        self._poll_interval_seconds = poll_interval_seconds

    def ensure_ready(self, session_id: str) -> SandboxLease:
        """Create a session sandbox if absent and wait until its runner is ready."""
        _validate_session_id(session_id)
        deadline = time.monotonic() + self._ready_timeout_seconds
        path = f"/api/v1/workers/{quote(self._worker_name, safe='')}/execution-sandboxes/ensure"
        while True:
            response = self._client.post(
                self._controller_url + path,
                json={"sessionId": session_id},
                headers=self._authorization_headers(),
            )
            response.raise_for_status()
            payload = response.json()
            if payload.get("phase") == "Ready":
                endpoint = _validated_base_url(str(payload.get("endpoint", "")), "sandbox endpoint")
                token = str(payload.get("token", ""))
                name = str(payload.get("name", ""))
                if not name or len(token) < 32:
                    raise RuntimeError("controller returned an incomplete ready sandbox lease")
                if self._runner_health_ready(endpoint, deadline):
                    return SandboxLease(name=name, endpoint=endpoint, token=token)
            if payload.get("phase") not in {None, "", "Pending", "Ready"}:
                raise RuntimeError(f"execution sandbox entered unexpected phase {payload.get('phase')!r}")
            if time.monotonic() >= deadline:
                raise TimeoutError(f"execution sandbox was not ready within {self._ready_timeout_seconds} seconds")
            time.sleep(self._poll_interval_seconds)

    def _runner_health_ready(self, endpoint: str, deadline: float) -> bool:
        """Probe the side-effect-free runner endpoint before leasing it for execution."""
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return False
        try:
            response = self._client.get(
                endpoint + "/healthz",
                timeout=min(2.0, remaining),
            )
        except httpx.TransportError:
            return False
        if response.status_code != 200:
            return False
        try:
            payload = response.json()
        except ValueError:
            return False
        return payload == {"status": "ok"}

    def heartbeat(self, session_id: str) -> None:
        """Refresh the idle deadline for a session sandbox."""
        response = self._client.post(
            self._session_url(session_id) + "/heartbeat",
            headers=self._authorization_headers(),
        )
        response.raise_for_status()

    def delete(self, session_id: str) -> None:
        """Request deletion of a session sandbox."""
        response = self._client.delete(
            self._session_url(session_id),
            headers=self._authorization_headers(),
        )
        if response.status_code != 404:
            response.raise_for_status()

    def _session_url(self, session_id: str) -> str:
        _validate_session_id(session_id)
        worker = quote(self._worker_name, safe="")
        session = quote(session_id, safe="")
        return f"{self._controller_url}/api/v1/workers/{worker}/execution-sandboxes/{session}"

    def _authorization_headers(self) -> dict[str, str]:
        token = self._token_path.read_text().strip()
        if not token:
            raise RuntimeError("projected service account token is empty")
        return {"Authorization": f"Bearer {token}"}


class AgentTeamsSandbox(BaseSandbox):
    """DeepAgents ``BaseSandbox`` implementation using a remote runner Pod."""

    # The credential-free Runner mounts only /workspace and /tmp. Advertising
    # capture offload would make DeepAgents write to the absent, read-only-root
    # path /large_tool_results after an otherwise successful command.
    enable_capture_offload = False

    def __init__(
        self,
        *,
        control: SandboxControlClient,
        session_id: str,
        client: httpx.Client | None = None,
        default_timeout_seconds: int = 120,
        workspace_store: WorkspaceStore | None = None,
    ) -> None:
        _validate_session_id(session_id)
        if default_timeout_seconds < 1:
            raise ValueError("default_timeout_seconds must be positive")
        self._control = control
        self._session_id = session_id
        self._client = client or httpx.Client(timeout=130, trust_env=False)
        self._default_timeout_seconds = default_timeout_seconds
        self._workspace_store = workspace_store
        self._lease: SandboxLease | None = None

    @property
    def id(self) -> str:
        """Return a stable ID without forcing eager sandbox creation."""
        return f"agentteams:{self._session_id}"

    def execute(self, command: str, *, timeout: int | None = None) -> ExecuteResponse:
        """Execute a command once, failing closed when its response is ambiguous."""
        timeout_seconds = self._default_timeout_seconds if timeout is None else timeout
        if isinstance(timeout_seconds, bool) or not isinstance(timeout_seconds, int) or timeout_seconds < 1:
            raise ValueError("timeout must be a positive integer")
        request_id = f"run-{uuid.uuid4().hex}"
        try:
            response = self._runner_request(
                "/v1/execute",
                {
                    "request_id": request_id,
                    "command": command,
                    "timeout_seconds": timeout_seconds,
                },
                timeout_seconds=timeout_seconds + 10,
            )
        except httpx.TransportError:
            _LOGGER.exception("runner result is ambiguous after the single allowed request")
            return ExecuteResponse(
                output="Execution outcome is unknown; the command was not re-run with a new request ID for safety.",
                exit_code=None,
            )
        if response.status_code == 409:
            return ExecuteResponse(
                output="Execution outcome is unknown; the command was not re-run for safety.",
                exit_code=None,
            )
        response.raise_for_status()
        payload = response.json()
        result = ExecuteResponse(
            output=str(payload.get("output", "")),
            exit_code=payload.get("exit_code"),
            truncated=bool(payload.get("truncated", False)),
        )
        changes = tuple(WorkspaceChange(**item) for item in payload.get("changes", []))
        if self._workspace_store is not None and changes:
            try:
                self._workspace_store.persist_changes(self, changes)
            except Exception:  # noqa: BLE001 - storage SDKs expose several transport-specific exception types.
                _LOGGER.exception("failed to persist execution sandbox changes")
                warning = (
                    "Workspace persistence failed after command completion; "
                    "do not repeat the command automatically."
                )
                result.output = f"{result.output}\n{warning}" if result.output else warning
        return result

    def upload_files(self, files: list[tuple[str, bytes]]) -> list[FileUploadResponse]:
        """Upload files as one bounded batch and preserve per-file errors."""
        payload = {
            "files": [
                {"path": path, "content_base64": base64.b64encode(content).decode("ascii")}
                for path, content in files
            ]
        }
        response = self._runner_request("/v1/files/upload", payload)
        response.raise_for_status()
        return [
            FileUploadResponse(path=str(item.get("path", "")), error=item.get("error"))
            for item in response.json().get("files", [])
        ]

    def download_files(self, paths: list[str]) -> list[FileDownloadResponse]:
        """Download files as one bounded batch and preserve per-file errors."""
        response = self._runner_request("/v1/files/download", {"paths": paths})
        response.raise_for_status()
        results: list[FileDownloadResponse] = []
        for item in response.json().get("files", []):
            content: bytes | None = None
            error = item.get("error")
            if error is None:
                try:
                    content = base64.b64decode(str(item.get("content_base64", "")), validate=True)
                except binascii.Error:
                    error = "invalid_content"
            results.append(
                FileDownloadResponse(
                    path=str(item.get("path", "")),
                    content=content,
                    error=error,
                )
            )
        return results

    def ls(self, path: str) -> LsResult:
        """List workspace entries through the bounded Runner file API."""
        response = self._runner_request("/v1/files/list", {"path": path})
        response.raise_for_status()
        payload = response.json()
        if payload.get("error"):
            return LsResult(error=f"Error listing '{path}': {payload['error']}")
        return LsResult(entries=list(payload.get("entries") or []))

    async def als(self, path: str) -> LsResult:
        return await asyncio.to_thread(self.ls, path)

    def read(self, file_path: str, offset: int = 0, limit: int = 2000) -> ReadResult:
        """Read and paginate a file without translating the operation into shell execution."""
        responses = self.download_files([file_path])
        if len(responses) != 1:
            raise RuntimeError("Runner returned an incomplete file download response")
        response = responses[0]
        if response.error or response.content is None:
            return ReadResult(error=f"File '{file_path}': {response.error or 'empty_response'}")
        file_type = _get_backend_read_file_type(file_path)
        if file_type != "text":
            return ReadResult(
                file_data=FileData(
                    content=base64.b64encode(response.content).decode("ascii"),
                    encoding="base64",
                )
            )
        try:
            content = response.content.decode("utf-8")
        except UnicodeDecodeError as exc:
            return ReadResult(error=f"Error reading file '{file_path}': {exc}")
        empty_message = check_empty_content(content)
        if empty_message:
            return ReadResult(file_data=FileData(content=empty_message, encoding="utf-8"))
        return slice_read_response(FileData(content=content, encoding="utf-8"), offset, limit)

    async def aread(self, file_path: str, offset: int = 0, limit: int = 2000) -> ReadResult:
        return await asyncio.to_thread(self.read, file_path, offset, limit)

    def write(self, file_path: str, content: str) -> WriteResult:
        """Write UTF-8 content through the bounded upload API and persist its manifest."""
        encoded = content.encode("utf-8")
        responses = self.upload_files([(file_path, encoded)])
        if len(responses) != 1:
            raise RuntimeError("Runner returned an incomplete file upload response")
        if responses[0].error:
            return WriteResult(error=f"Failed to write file '{file_path}': {responses[0].error}")
        change = self._uploaded_change(file_path, encoded)
        warning = self._persist_file_changes((change,))
        if warning:
            return WriteResult(error=warning)
        return WriteResult(path=file_path)

    async def awrite(self, file_path: str, content: str) -> WriteResult:
        return await asyncio.to_thread(self.write, file_path, content)

    def edit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,  # noqa: FBT001, FBT002
    ) -> EditResult:
        """Perform an exact replacement through bounded download/upload APIs."""
        downloads = self.download_files([file_path])
        if len(downloads) != 1:
            raise RuntimeError("Runner returned an incomplete file download response")
        downloaded = downloads[0]
        if downloaded.error or downloaded.content is None:
            return EditResult(error=f"Error editing file '{file_path}': {downloaded.error or 'empty_response'}")
        try:
            content = downloaded.content.decode("utf-8").replace("\r\n", "\n").replace("\r", "\n")
        except UnicodeDecodeError as exc:
            return EditResult(error=f"Error editing file '{file_path}': {exc}")
        old_string = old_string.replace("\r\n", "\n").replace("\r", "\n")
        new_string = new_string.replace("\r\n", "\n").replace("\r", "\n")
        replacement = perform_string_replacement(content, old_string, new_string, replace_all)
        if isinstance(replacement, str):
            return EditResult(error=replacement)
        updated, occurrences = replacement
        encoded = updated.encode("utf-8")
        uploads = self.upload_files([(file_path, encoded)])
        if len(uploads) != 1:
            raise RuntimeError("Runner returned an incomplete file upload response")
        if uploads[0].error:
            return EditResult(error=f"Error editing file '{file_path}': {uploads[0].error}")
        warning = self._persist_file_changes((self._uploaded_change(file_path, encoded),))
        if warning:
            return EditResult(error=warning)
        return EditResult(path=file_path, occurrences=occurrences)

    async def aedit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,  # noqa: FBT001, FBT002
    ) -> EditResult:
        return await asyncio.to_thread(self.edit, file_path, old_string, new_string, replace_all)

    def delete(self, file_path: str) -> DeleteResult:
        """Delete a workspace path through the bounded Runner file API."""
        response = self._runner_request("/v1/files/delete", {"path": file_path})
        response.raise_for_status()
        payload = response.json()
        if payload.get("error"):
            return DeleteResult(error=f"Error deleting file '{file_path}': {payload['error']}")
        changes = tuple(WorkspaceChange(**item) for item in payload.get("changes", []))
        warning = self._persist_file_changes(changes)
        if warning:
            return DeleteResult(error=warning)
        return DeleteResult(path=file_path)

    async def adelete(self, file_path: str) -> DeleteResult:
        return await asyncio.to_thread(self.delete, file_path)

    def grep(
        self,
        pattern: str,
        path: str | None = None,
        glob: str | None = None,
        *,
        max_count: int | None = None,
    ) -> GrepResult:
        """Run one bounded literal file search without shell execution."""
        response = self._runner_request(
            "/v1/files/grep",
            {"pattern": pattern, "path": path, "glob": glob, "max_count": max_count},
        )
        response.raise_for_status()
        payload = response.json()
        if payload.get("error"):
            return GrepResult(error=f"Error searching '{path or '/workspace'}': {payload['error']}")
        return GrepResult(matches=list(payload.get("matches") or []), truncated=bool(payload.get("truncated")))

    async def agrep(
        self,
        pattern: str,
        path: str | None = None,
        glob: str | None = None,
        *,
        max_count: int | None = None,
    ) -> GrepResult:
        return await asyncio.to_thread(self.grep, pattern, path, glob, max_count=max_count)

    def glob(self, pattern: str, path: str | None = None) -> GlobResult:
        """Run one bounded workspace glob without shell execution."""
        response = self._runner_request("/v1/files/glob", {"pattern": pattern, "path": path})
        response.raise_for_status()
        payload = response.json()
        if payload.get("error"):
            return GlobResult(error=f"Error globbing '{path or '/workspace'}': {payload['error']}")
        return GlobResult(matches=list(payload.get("matches") or []), truncated=bool(payload.get("truncated")))

    async def aglob(self, pattern: str, path: str | None = None) -> GlobResult:
        return await asyncio.to_thread(self.glob, pattern, path)

    def close(self) -> None:
        """Release the controller-managed sandbox when no longer needed."""
        if self._lease is not None:
            self._control.delete(self._session_id)
            self._lease = None

    def _runner_request(
        self,
        path: str,
        payload: dict[str, object],
        *,
        timeout_seconds: float = 30,
    ) -> httpx.Response:
        lease = self._ensure_lease()
        try:
            self._control.heartbeat(self._session_id)
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code not in {404, 410}:
                raise
            self._lease = None
            lease = self._ensure_lease()
        return self._client.post(
            lease.endpoint + path,
            json=payload,
            headers={"Authorization": f"Bearer {lease.token}"},
            timeout=timeout_seconds,
        )

    def _ensure_lease(self) -> SandboxLease:
        if self._lease is None:
            lease = self._control.ensure_ready(self._session_id)
            self._lease = lease
            if self._workspace_store is not None:
                try:
                    self._workspace_store.hydrate(self)
                except Exception:
                    self._lease = None
                    raise
        return self._lease

    @staticmethod
    def _uploaded_change(file_path: str, content: bytes) -> WorkspaceChange:
        virtual_path = Path(file_path)
        try:
            relative = virtual_path.relative_to("/workspace").as_posix()
        except ValueError as exc:
            raise ValueError("uploaded paths must be below /workspace") from exc
        if not relative or relative == ".":
            raise ValueError("uploaded path must name a file below /workspace")
        return WorkspaceChange(
            path=relative,
            sha256=hashlib.sha256(content).hexdigest(),
            size=len(content),
            deleted=False,
        )

    def _persist_file_changes(self, changes: tuple[WorkspaceChange, ...]) -> str | None:
        if self._workspace_store is None or not changes:
            return None
        try:
            self._workspace_store.persist_changes(self, changes)
        except Exception:  # noqa: BLE001 - storage SDKs expose transport-specific exception types.
            _LOGGER.exception("failed to persist bounded file operation changes")
            return "Workspace persistence failed after the file operation; do not repeat it automatically."
        return None


def _validate_session_id(session_id: str) -> None:
    if _SESSION_ID_PATTERN.fullmatch(session_id) is None:
        raise ValueError("session_id contains unsupported characters")


def _validated_base_url(value: str, field: str) -> str:
    parsed = urlsplit(value)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password:
        raise ValueError(f"{field} must be an HTTP(S) URL without embedded credentials")
    if parsed.query or parsed.fragment:
        raise ValueError(f"{field} must not contain a query or fragment")
    return value.rstrip("/")
