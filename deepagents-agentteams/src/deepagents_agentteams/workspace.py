"""Credential-bearing MinIO proxy for credential-free execution sandboxes."""

from __future__ import annotations

import io
from collections.abc import Iterable, Sequence
from pathlib import PurePosixPath
from typing import Any, Protocol
from urllib.parse import urlsplit

from deepagents.backends.protocol import FileDownloadResponse, FileUploadResponse
from minio import Minio

from deepagents_agentteams.config import StorageConfig
from deepagents_agentteams.runner_core import WorkspaceChange

_SENSITIVE_PREFIXES = ("credentials/", "runtime/", "config/", ".openclaw/", ".agentteams/")
_SENSITIVE_FILES = frozenset({"openclaw.json", "mcporter-servers.json", "matrix_sync_token", ".last-pull"})


class SandboxFiles(Protocol):
    """Subset of the DeepAgents sandbox contract needed for synchronization."""

    def upload_files(self, files: list[tuple[str, bytes]]) -> list[FileUploadResponse]: ...

    def download_files(self, paths: list[str]) -> list[FileDownloadResponse]: ...


class MinIOWorkspaceStore:
    """Synchronize non-sensitive member files through the Worker credential boundary."""

    def __init__(
        self,
        *,
        client: Any,
        bucket: str,
        member_prefix: str,
        max_files: int = 10_000,
        max_file_bytes: int = 10 * 1024 * 1024,
        max_total_bytes: int = 100 * 1024 * 1024,
    ) -> None:
        if not bucket or not member_prefix:
            raise ValueError("workspace bucket and member prefix must be non-empty")
        self._client = client
        self._bucket = bucket
        self._prefix = member_prefix.strip("/")
        self._max_files = max_files
        self._max_file_bytes = max_file_bytes
        self._max_total_bytes = max_total_bytes

    @classmethod
    def from_config(cls, config: StorageConfig) -> MinIOWorkspaceStore:
        """Construct a MinIO client from secret-resolved Worker configuration."""
        parsed = urlsplit(config.endpoint)
        if parsed.scheme not in {"http", "https"} or not parsed.netloc or parsed.path not in {"", "/"}:
            raise ValueError("storage endpoint must be an HTTP(S) origin URL")
        if parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise ValueError("storage endpoint must not contain credentials, query, or fragment")
        client = Minio(
            parsed.netloc,
            access_key=config.access_key,
            secret_key=config.secret_key,
            secure=parsed.scheme == "https",
        )
        return cls(
            client=client,
            bucket=config.bucket,
            member_prefix=config.member_prefix,
        )

    def hydrate(self, sandbox: SandboxFiles) -> None:
        """Copy safe durable objects into a newly-created session sandbox."""
        prefix = self._prefix + "/"
        files: list[tuple[str, bytes]] = []
        total_bytes = 0
        objects = sorted(
            self._client.list_objects(self._bucket, prefix=prefix, recursive=True),
            key=lambda item: item.object_name,
        )
        for item in objects:
            relative = self._safe_relative(str(item.object_name)[len(prefix) :])
            if relative is None:
                continue
            if len(files) >= self._max_files:
                raise RuntimeError(f"workspace exceeds {self._max_files} file hydration limit")
            advertised_size = int(getattr(item, "size", 0) or 0)
            if advertised_size > self._max_file_bytes:
                raise RuntimeError(f"workspace file {relative} exceeds hydration size limit")
            response = self._client.get_object(self._bucket, item.object_name)
            try:
                content = response.read(self._max_file_bytes + 1)
            finally:
                response.close()
                response.release_conn()
            if len(content) > self._max_file_bytes:
                raise RuntimeError(f"workspace file {relative} exceeds hydration size limit")
            total_bytes += len(content)
            if total_bytes > self._max_total_bytes:
                raise RuntimeError("workspace exceeds total hydration size limit")
            files.append((f"/workspace/{relative}", content))

        for batch in _batches(files, 128):
            results = sandbox.upload_files(batch)
            if len(results) != len(batch):
                raise RuntimeError("sandbox returned an incomplete workspace upload response")
            failures = [result for result in results if result.error]
            if failures:
                raise RuntimeError(f"sandbox workspace upload failed for {failures[0].path}: {failures[0].error}")

    def persist_changes(self, sandbox: SandboxFiles, changes: Sequence[WorkspaceChange]) -> None:
        """Apply a runner change manifest to the durable member prefix."""
        changed_paths: list[str] = []
        for change in changes:
            relative = self._safe_relative(change.path)
            if relative is None:
                continue
            object_name = f"{self._prefix}/{relative}"
            if change.deleted:
                self._client.remove_object(self._bucket, object_name)
            else:
                changed_paths.append(f"/workspace/{relative}")

        for batch in _batches(changed_paths, 128):
            downloads = sandbox.download_files(batch)
            if len(downloads) != len(batch):
                raise RuntimeError("sandbox returned an incomplete workspace download response")
            for result in downloads:
                if result.error or result.content is None:
                    raise RuntimeError(f"sandbox workspace download failed for {result.path}: {result.error}")
                relative = self._safe_relative(result.path.removeprefix("/workspace/"))
                if relative is None:
                    raise RuntimeError(f"sandbox returned a non-persistable path {result.path}")
                self._client.put_object(
                    self._bucket,
                    f"{self._prefix}/{relative}",
                    io.BytesIO(result.content),
                    len(result.content),
                )

    @staticmethod
    def _safe_relative(value: str) -> str | None:
        path = PurePosixPath(value)
        if path.is_absolute() or not path.parts or ".." in path.parts or "\0" in value:
            raise ValueError("workspace object path is invalid")
        normalized = path.as_posix()
        if normalized in _SENSITIVE_FILES or normalized.startswith(_SENSITIVE_PREFIXES):
            return None
        return normalized


def _batches(items: Sequence[Any], size: int) -> Iterable[list[Any]]:
    for start in range(0, len(items), size):
        yield list(items[start : start + size])
