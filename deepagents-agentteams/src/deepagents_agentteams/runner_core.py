"""Credential-free command runner used inside an ExecutionSandbox Pod."""

from __future__ import annotations

import json
import hashlib
import re
import subprocess
from dataclasses import asdict, dataclass
from pathlib import Path


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


_REQUEST_ID_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}\Z")


class RunnerService:
    """Execute commands once and persist their terminal result by request ID."""

    def __init__(self, *, workspace: Path, state_dir: Path) -> None:
        self._workspace = workspace.resolve()
        self._state_dir = state_dir.resolve()
        self._workspace.mkdir(parents=True, exist_ok=True)
        self._state_dir.mkdir(parents=True, exist_ok=True)

    def execute(self, *, request_id: str, command: str, timeout_seconds: int) -> RunnerResult:
        """Execute a shell command at most once for a stable request ID."""
        if _REQUEST_ID_PATTERN.fullmatch(request_id) is None:
            raise ValueError("request_id contains unsupported characters")
        state_path = self._state_dir / f"{request_id}.json"
        if state_path.exists():
            data = json.loads(state_path.read_text())
            if data.get("status") == "pending":
                raise UnknownExecutionResult(f"execution result for {request_id} is unknown")
            data.pop("status", None)
            data["changes"] = tuple(WorkspaceChange(**change) for change in data.get("changes", ()))
            return RunnerResult(**data)

        before = self._snapshot_workspace()
        self._write_state(
            state_path,
            {"request_id": request_id, "status": "pending"},
        )

        completed = subprocess.run(  # noqa: S602
            command,
            cwd=self._workspace,
            shell=True,
            executable="/bin/sh",
            capture_output=True,
            timeout=timeout_seconds,
            check=False,
        )
        output = (completed.stdout + completed.stderr).decode("utf-8", errors="replace")
        after = self._snapshot_workspace()
        result = RunnerResult(
            request_id=request_id,
            output=output,
            exit_code=completed.returncode,
            changes=_workspace_changes(before, after),
        )
        self._write_state(state_path, {"status": "completed", **asdict(result)})
        return result

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
