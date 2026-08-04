"""Credential-bearing MinIO proxy for credential-free execution sandboxes."""

from __future__ import annotations

import hashlib
import io
import threading
from collections.abc import Iterable, Mapping, Sequence
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
        self._sync_lock = threading.RLock()

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
        with self._sync_lock:
            self._hydrate(sandbox)

    def _hydrate(self, sandbox: SandboxFiles) -> None:
        prefix = self._prefix + "/"
        files: list[tuple[str, bytes]] = []
        total_bytes = 0
        # MinIO's recursive listing is lazy and ordered by object name. Keep it
        # lazy so the file-count limit also bounds Worker metadata memory.
        for item in self._client.list_objects(self._bucket, prefix=prefix, recursive=True):
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
        with self._sync_lock:
            self._persist_changes(sandbox, changes)

    def _persist_changes(self, sandbox: SandboxFiles, changes: Sequence[WorkspaceChange]) -> None:
        downloads_to_validate: list[tuple[str, str, WorkspaceChange]] = []
        object_names_to_delete: list[str] = []
        seen_paths: set[str] = set()
        planned_changes: dict[str, WorkspaceChange] = {}
        advertised_total_bytes = 0
        for change in changes:
            relative = self._safe_relative(change.path)
            if relative is None:
                continue
            if relative in seen_paths:
                raise RuntimeError(f"runner manifest contains duplicate path {relative}")
            seen_paths.add(relative)
            planned_changes[relative] = change
            if len(seen_paths) > self._max_files:
                raise RuntimeError(f"workspace exceeds {self._max_files} file persistence limit")
            object_name = f"{self._prefix}/{relative}"
            if change.deleted:
                if change.sha256 is not None or change.size != 0:
                    raise RuntimeError(f"deleted file {relative} has an invalid runner manifest")
                object_names_to_delete.append(object_name)
            else:
                if (
                    isinstance(change.size, bool)
                    or not isinstance(change.size, int)
                    or change.size < 0
                    or change.size > self._max_file_bytes
                    or change.sha256 is None
                ):
                    raise RuntimeError(f"file {relative} has an invalid runner manifest")
                advertised_total_bytes += change.size
                if advertised_total_bytes > self._max_total_bytes:
                    raise RuntimeError("workspace exceeds total persistence size limit")
                downloads_to_validate.append((f"/workspace/{relative}", object_name, change))

        self._validate_projected_workspace(planned_changes)

        objects_to_upload: list[tuple[str, bytes]] = []
        total_bytes = 0
        for batch in _batches(downloads_to_validate, 128):
            requested_paths = [path for path, _object_name, _change in batch]
            downloads = sandbox.download_files(requested_paths)
            if len(downloads) != len(batch):
                raise RuntimeError("sandbox returned an incomplete workspace download response")
            for (requested_path, object_name, change), result in zip(batch, downloads, strict=True):
                if result.path != requested_path:
                    raise RuntimeError(
                        f"sandbox returned {result.path} while persisting {requested_path}"
                    )
                if result.error or result.content is None:
                    raise RuntimeError(f"sandbox workspace download failed for {result.path}: {result.error}")
                content = result.content
                digest = hashlib.sha256(content).hexdigest()
                if len(content) != change.size or digest != change.sha256:
                    raise RuntimeError(
                        f"sandbox content for {result.path} does not match runner manifest"
                    )
                total_bytes += len(content)
                if total_bytes > self._max_total_bytes:
                    raise RuntimeError("workspace exceeds total persistence size limit")
                objects_to_upload.append((object_name, content))

        # Validate the complete manifest before mutating MinIO. Uploads precede
        # deletions so an object-store failure cannot delete durable files first.
        for object_name, content in objects_to_upload:
            self._client.put_object(
                self._bucket,
                object_name,
                io.BytesIO(content),
                len(content),
            )
        for object_name in object_names_to_delete:
            self._client.remove_object(
                self._bucket,
                object_name,
            )

    def _validate_projected_workspace(self, changes: Mapping[str, WorkspaceChange]) -> None:
        prefix = self._prefix + "/"
        projected_files = 0
        projected_bytes = 0
        matched_changes: set[str] = set()
        for item in self._client.list_objects(self._bucket, prefix=prefix, recursive=True):
            object_name = str(item.object_name)
            if not object_name.startswith(prefix):
                raise RuntimeError("MinIO returned an object outside the requested workspace prefix")
            relative = self._safe_relative(object_name[len(prefix) :])
            if relative is None:
                continue
            advertised_size = int(getattr(item, "size", 0) or 0)
            if advertised_size < 0:
                raise RuntimeError(f"workspace object {relative} has an invalid size")
            change = changes.get(relative)
            if change is not None:
                matched_changes.add(relative)
                if change.deleted:
                    continue
                advertised_size = change.size
            elif advertised_size > self._max_file_bytes:
                raise RuntimeError(
                    f"workspace file {relative} exceeds final persistence file size limit"
                )
            projected_files += 1
            projected_bytes += advertised_size

        for relative, change in changes.items():
            if relative in matched_changes or change.deleted:
                continue
            projected_files += 1
            projected_bytes += change.size

        if projected_files > self._max_files:
            raise RuntimeError(f"workspace exceeds {self._max_files} final persistence file limit")
        if projected_bytes > self._max_total_bytes:
            raise RuntimeError("workspace exceeds final persistence size limit")

    @staticmethod
    def _safe_relative(value: str) -> str | None:
        path = PurePosixPath(value)
        if path.is_absolute() or not path.parts or ".." in path.parts or "\0" in value:
            raise ValueError("workspace object path is invalid")
        normalized = path.as_posix()
        if normalized != value:
            raise ValueError("workspace object path is invalid")
        if normalized in _SENSITIVE_FILES or normalized.startswith(_SENSITIVE_PREFIXES):
            return None
        return normalized


def _batches(items: Sequence[Any], size: int) -> Iterable[list[Any]]:
    for start in range(0, len(items), size):
        yield list(items[start : start + size])
