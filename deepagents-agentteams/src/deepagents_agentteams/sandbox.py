"""DeepAgents sandbox backend backed by AgentTeams ExecutionSandbox resources."""

from __future__ import annotations

import base64
import binascii
import re
import time
import uuid
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import quote, urlsplit

import httpx
from deepagents.backends.protocol import ExecuteResponse, FileDownloadResponse, FileUploadResponse
from deepagents.backends.sandbox import BaseSandbox

_SESSION_ID_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}\Z")


@dataclass(frozen=True)
class SandboxLease:
    """Ready runner endpoint and its short-lived capability credential."""

    name: str
    endpoint: str
    token: str


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
                return SandboxLease(name=name, endpoint=endpoint, token=token)
            if payload.get("phase") not in {None, "", "Pending"}:
                raise RuntimeError(f"execution sandbox entered unexpected phase {payload.get('phase')!r}")
            if time.monotonic() >= deadline:
                raise TimeoutError(f"execution sandbox was not ready within {self._ready_timeout_seconds} seconds")
            time.sleep(self._poll_interval_seconds)

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

    enable_capture_offload = True

    def __init__(
        self,
        *,
        control: SandboxControlClient,
        session_id: str,
        client: httpx.Client | None = None,
        default_timeout_seconds: int = 120,
    ) -> None:
        _validate_session_id(session_id)
        if default_timeout_seconds < 1:
            raise ValueError("default_timeout_seconds must be positive")
        self._control = control
        self._session_id = session_id
        self._client = client or httpx.Client(timeout=130, trust_env=False)
        self._default_timeout_seconds = default_timeout_seconds
        self._lease: SandboxLease | None = None

    @property
    def id(self) -> str:
        """Return a stable ID without forcing eager sandbox creation."""
        return f"agentteams:{self._session_id}"

    def execute(self, command: str, *, timeout: int | None = None) -> ExecuteResponse:
        """Execute a command with a retry-stable request ID."""
        timeout_seconds = self._default_timeout_seconds if timeout is None else timeout
        if isinstance(timeout_seconds, bool) or not isinstance(timeout_seconds, int) or timeout_seconds < 1:
            raise ValueError("timeout must be a positive integer")
        request_id = f"run-{uuid.uuid4().hex}"
        response = self._runner_request(
            "/v1/execute",
            {
                "request_id": request_id,
                "command": command,
                "timeout_seconds": timeout_seconds,
            },
            timeout_seconds=timeout_seconds + 10,
        )
        if response.status_code == 409:
            return ExecuteResponse(
                output="Execution outcome is unknown; the command was not re-run for safety.",
                exit_code=None,
            )
        response.raise_for_status()
        payload = response.json()
        return ExecuteResponse(
            output=str(payload.get("output", "")),
            exit_code=payload.get("exit_code"),
            truncated=bool(payload.get("truncated", False)),
        )

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
        self._control.heartbeat(self._session_id)
        last_error: httpx.TransportError | None = None
        for _attempt in range(2):
            try:
                return self._client.post(
                    lease.endpoint + path,
                    json=payload,
                    headers={"Authorization": f"Bearer {lease.token}"},
                    timeout=timeout_seconds,
                )
            except httpx.TransportError as exc:
                last_error = exc
        if last_error is None:  # pragma: no cover - the fixed retry loop always records an error.
            raise RuntimeError("runner request failed without a transport error")
        raise last_error

    def _ensure_lease(self) -> SandboxLease:
        if self._lease is None:
            self._lease = self._control.ensure_ready(self._session_id)
        return self._lease


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
