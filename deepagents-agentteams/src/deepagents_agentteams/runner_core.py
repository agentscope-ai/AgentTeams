"""Credential-free command runner used inside an ExecutionSandbox Pod."""

from __future__ import annotations

import hashlib
import json
import os
import re
import signal
import subprocess
import tempfile
from dataclasses import asdict, dataclass
from pathlib import Path, PurePosixPath


@dataclass(frozen=True)
class WorkspaceChange:
    """One changed or deleted regular file in the runner workspace."""

    path: str
    sha256: str | None
    size: int
    deleted: bool


@dataclass(frozen=True)
class RunnerResult:
    """Persisted result for one idempotent runner request."""

    request_id: str
    output: str
    exit_code: int | None
    truncated: bool = False
    changes: tuple[WorkspaceChange, ...] = ()


class UnknownExecutionResult(RuntimeError):
    """Raised when a prior command may have run but has no terminal result."""


class InvalidWorkspacePath(ValueError):
    """Raised when a file operation escapes the mounted workspace."""


class FileTooLarge(ValueError):
    """Raised when a file exceeds the runner's transfer limit."""


_REQUEST_ID_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}\Z")
_VIRTUAL_WORKSPACE = PurePosixPath("/workspace")


class RunnerService:
    """Execute commands once and persist their terminal result by request ID."""

    def __init__(
        self,
        *,
        workspace: Path,
        state_dir: Path,
        max_output_bytes: int = 512 * 1024,
        max_file_bytes: int = 10 * 1024 * 1024,
    ) -> None:
        self._workspace = workspace.resolve()
        self._state_dir = state_dir.resolve()
        if max_output_bytes < 1 or max_file_bytes < 1:
            raise ValueError("runner byte limits must be positive")
        self._max_output_bytes = max_output_bytes
        self._max_file_bytes = max_file_bytes
        self._workspace.mkdir(parents=True, exist_ok=True)
        self._state_dir.mkdir(parents=True, exist_ok=True)

    def execute(self, *, request_id: str, command: str, timeout_seconds: int) -> RunnerResult:
        """Execute a shell command at most once for a stable request ID."""
        if _REQUEST_ID_PATTERN.fullmatch(request_id) is None:
            raise ValueError("request_id contains unsupported characters")
        if not command.strip():
            raise ValueError("command must be non-empty")
        if isinstance(timeout_seconds, bool) or not isinstance(timeout_seconds, int) or timeout_seconds < 1:
            raise ValueError("timeout_seconds must be a positive integer")
        state_path = self._state_dir / f"{request_id}.json"
        try:
            state_descriptor = os.open(state_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        except FileExistsError:
            return self._existing_result(state_path, request_id)

        with os.fdopen(state_descriptor, "w", encoding="utf-8") as state_handle:
            json.dump({"request_id": request_id, "status": "pending"}, state_handle, sort_keys=True)
            state_handle.flush()
            os.fsync(state_handle.fileno())

        before = self._snapshot_workspace()
        with tempfile.TemporaryFile() as output_handle:
            process = subprocess.Popen(  # noqa: S602
                command,
                cwd=self._workspace,
                env=self._command_environment(),
                shell=True,
                executable="/bin/sh",
                stdout=output_handle,
                stderr=subprocess.STDOUT,
                close_fds=True,
                start_new_session=True,
            )
            timed_out = False
            try:
                process.wait(timeout=timeout_seconds)
            except subprocess.TimeoutExpired:
                timed_out = True
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            output_handle.seek(0)
            output_bytes = output_handle.read(self._max_output_bytes + 1)

        truncated = len(output_bytes) > self._max_output_bytes
        output = output_bytes[: self._max_output_bytes].decode("utf-8", errors="replace")
        exit_code = process.returncode
        if timed_out:
            timeout_message = f"command timed out after {timeout_seconds} seconds"
            output = f"{timeout_message}\n{output}"[: self._max_output_bytes]
            exit_code = 124
            truncated = truncated or len(timeout_message) + 1 + len(output_bytes) > self._max_output_bytes
        after = self._snapshot_workspace()
        result = RunnerResult(
            request_id=request_id,
            output=output,
            exit_code=exit_code,
            truncated=truncated,
            changes=_workspace_changes(before, after),
        )
        self._write_state(state_path, {"status": "completed", **asdict(result)})
        return result

    def upload_file(self, *, path: str, content: bytes) -> None:
        """Atomically write one bounded file below ``/workspace``."""
        if len(content) > self._max_file_bytes:
            raise FileTooLarge(f"file exceeds {self._max_file_bytes} byte limit")
        target = self._workspace_path(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(dir=target.parent, prefix=".agentteams-upload-", delete=False) as handle:
            temporary_path = Path(handle.name)
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        try:
            temporary_path.replace(target)
        finally:
            temporary_path.unlink(missing_ok=True)

    def download_file(self, *, path: str) -> bytes:
        """Read one bounded regular file below ``/workspace``."""
        target = self._workspace_path(path)
        if not target.exists():
            raise FileNotFoundError(path)
        if not target.is_file():
            raise IsADirectoryError(path)
        with target.open("rb") as handle:
            content = handle.read(self._max_file_bytes + 1)
        if len(content) > self._max_file_bytes:
            raise FileTooLarge(f"file exceeds {self._max_file_bytes} byte limit")
        return content

    @staticmethod
    def _existing_result(state_path: Path, request_id: str) -> RunnerResult:
        data = json.loads(state_path.read_text())
        if data.get("status") == "pending":
            raise UnknownExecutionResult(f"execution result for {request_id} is unknown")
        data.pop("status", None)
        data["changes"] = tuple(WorkspaceChange(**change) for change in data.get("changes", ()))
        return RunnerResult(**data)

    @staticmethod
    def _write_state(state_path: Path, data: dict[str, object]) -> None:
        temporary_path = state_path.with_suffix(".tmp")
        temporary_path.write_text(json.dumps(data, sort_keys=True))
        temporary_path.replace(state_path)

    def _snapshot_workspace(self) -> dict[str, tuple[str, int]]:
        snapshot: dict[str, tuple[str, int]] = {}
        for path in self._workspace.rglob("*"):
            if path.is_symlink() or not path.is_file():
                continue
            relative_path = path.relative_to(self._workspace).as_posix()
            digest = hashlib.sha256()
            with path.open("rb") as handle:
                for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                    digest.update(chunk)
            snapshot[relative_path] = (digest.hexdigest(), path.stat().st_size)
        return snapshot

    def _command_environment(self) -> dict[str, str]:
        """Return a small non-secret environment for untrusted commands."""
        return {
            "HOME": str(self._workspace),
            "LANG": "C.UTF-8",
            "LC_ALL": "C.UTF-8",
            "PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
            "TMPDIR": "/tmp",  # noqa: S108 - /tmp is a dedicated Pod emptyDir mount.
        }

    def _workspace_path(self, value: str) -> Path:
        if not value or "\0" in value:
            raise InvalidWorkspacePath("workspace path must be non-empty")
        virtual_path = PurePosixPath(value)
        if ".." in virtual_path.parts:
            raise InvalidWorkspacePath("workspace path traversal is not allowed")
        if virtual_path.is_absolute():
            try:
                relative_path = virtual_path.relative_to(_VIRTUAL_WORKSPACE)
            except ValueError as exc:
                raise InvalidWorkspacePath("absolute paths must be below /workspace") from exc
        else:
            relative_path = virtual_path
        if not relative_path.parts:
            raise InvalidWorkspacePath("workspace root is not a file path")

        lexical_path = self._workspace.joinpath(*relative_path.parts)
        current = self._workspace
        for part in relative_path.parts:
            current = current / part
            if current.is_symlink():
                raise InvalidWorkspacePath("symbolic links are not allowed in workspace file paths")
        resolved_path = lexical_path.resolve(strict=False)
        if not resolved_path.is_relative_to(self._workspace):
            raise InvalidWorkspacePath("workspace path escapes the mounted workspace")
        return resolved_path


def _workspace_changes(
    before: dict[str, tuple[str, int]],
    after: dict[str, tuple[str, int]],
) -> tuple[WorkspaceChange, ...]:
    changed: list[WorkspaceChange] = []
    for path in sorted(before.keys() | after.keys()):
        if path not in after:
            changed.append(WorkspaceChange(path=path, sha256=None, size=0, deleted=True))
        elif before.get(path) != after[path]:
            digest, size = after[path]
            changed.append(WorkspaceChange(path=path, sha256=digest, size=size, deleted=False))
    return tuple(changed)
