import hashlib
import io
from types import SimpleNamespace

import pytest
from deepagents.backends.protocol import FileDownloadResponse, FileUploadResponse

from deepagents_agentteams.runner_core import WorkspaceChange
from deepagents_agentteams.workspace import MinIOWorkspaceStore


class ObjectResponse(io.BytesIO):
    def close(self) -> None:
        super().close()

    def release_conn(self) -> None:
        return None


class FakeMinIO:
    def __init__(self, objects: dict[str, bytes]) -> None:
        self.objects = objects
        self.puts: dict[str, bytes] = {}
        self.removes: list[str] = []

    def list_objects(self, bucket: str, *, prefix: str, recursive: bool):  # noqa: ANN201
        assert bucket == "agentteams"
        assert recursive is True
        return [
            SimpleNamespace(object_name=name, size=len(content))
            for name, content in self.objects.items()
            if name.startswith(prefix)
        ]

    def get_object(self, bucket: str, object_name: str) -> ObjectResponse:
        assert bucket == "agentteams"
        return ObjectResponse(self.objects[object_name])

    def put_object(self, bucket: str, object_name: str, data, length: int) -> None:  # noqa: ANN001
        assert bucket == "agentteams"
        self.puts[object_name] = data.read(length)

    def remove_object(self, bucket: str, object_name: str) -> None:
        assert bucket == "agentteams"
        self.removes.append(object_name)


class FakeSandbox:
    def __init__(self) -> None:
        self.uploaded: list[tuple[str, bytes]] = []
        self.downloaded: list[str] = []

    def upload_files(self, files: list[tuple[str, bytes]]) -> list[FileUploadResponse]:
        self.uploaded.extend(files)
        return [FileUploadResponse(path=path) for path, _content in files]

    def download_files(self, paths: list[str]) -> list[FileDownloadResponse]:
        self.downloaded.extend(paths)
        return [FileDownloadResponse(path=path, content=f"content:{path}".encode()) for path in paths]


def test_hydrate_excludes_runtime_credentials_and_legacy_secret_configs() -> None:
    client = FakeMinIO(
        {
            "agents/researcher/notes.md": b"notes",
            "agents/researcher/project/main.py": b"print('safe')",
            "agents/researcher/credentials/matrix/password": b"password",
            "agents/researcher/runtime/runtime.yaml": b"runtime",
            "agents/researcher/openclaw.json": b"legacy secrets",
            "agents/researcher/.openclaw/session": b"session",
        }
    )
    store = MinIOWorkspaceStore(
        client=client,
        bucket="agentteams",
        member_prefix="agents/researcher",
    )
    sandbox = FakeSandbox()

    store.hydrate(sandbox)

    assert sandbox.uploaded == [
        ("/workspace/notes.md", b"notes"),
        ("/workspace/project/main.py", b"print('safe')"),
    ]


def test_hydrate_rejects_noncanonical_object_aliases() -> None:
    client = FakeMinIO(
        {
            "agents/researcher/project/main.py": b"canonical",
            "agents/researcher/project//main.py": b"alias",
        }
    )
    store = MinIOWorkspaceStore(
        client=client,
        bucket="agentteams",
        member_prefix="agents/researcher",
    )
    sandbox = FakeSandbox()

    with pytest.raises(ValueError, match="workspace object path is invalid"):
        store.hydrate(sandbox)

    assert sandbox.uploaded == []


def test_persist_changes_uploads_changed_files_and_removes_deleted_objects() -> None:
    client = FakeMinIO({})
    store = MinIOWorkspaceStore(
        client=client,
        bucket="agentteams",
        member_prefix="agents/researcher",
    )
    sandbox = FakeSandbox()
    content = b"content:/workspace/project/main.py"

    store.persist_changes(
        sandbox,
        (
            WorkspaceChange(
                path="project/main.py",
                sha256=hashlib.sha256(content).hexdigest(),
                size=len(content),
                deleted=False,
            ),
            WorkspaceChange(path="old.txt", sha256=None, size=0, deleted=True),
        ),
    )

    assert client.puts == {
        "agents/researcher/project/main.py": b"content:/workspace/project/main.py"
    }
    assert client.removes == ["agents/researcher/old.txt"]


def test_persist_changes_rejects_manifest_mismatch_before_mutating_minio() -> None:
    client = FakeMinIO({})
    store = MinIOWorkspaceStore(
        client=client,
        bucket="agentteams",
        member_prefix="agents/researcher",
    )
    sandbox = FakeSandbox()

    with pytest.raises(RuntimeError, match="does not match runner manifest"):
        store.persist_changes(
            sandbox,
            (
                WorkspaceChange(
                    path="project/main.py",
                    sha256="0" * 64,
                    size=len(b"content:/workspace/project/main.py"),
                    deleted=False,
                ),
                WorkspaceChange(path="old.txt", sha256=None, size=0, deleted=True),
            ),
        )

    assert client.puts == {}
    assert client.removes == []


def test_persist_changes_enforces_total_size_before_mutating_minio() -> None:
    client = FakeMinIO({})
    store = MinIOWorkspaceStore(
        client=client,
        bucket="agentteams",
        member_prefix="agents/researcher",
        max_total_bytes=10,
    )
    sandbox = FakeSandbox()
    content = b"content:/workspace/project/main.py"

    with pytest.raises(RuntimeError, match="total persistence size limit"):
        store.persist_changes(
            sandbox,
            (
                WorkspaceChange(
                    path="project/main.py",
                    sha256=hashlib.sha256(content).hexdigest(),
                    size=len(content),
                    deleted=False,
                ),
                WorkspaceChange(path="old.txt", sha256=None, size=0, deleted=True),
            ),
        )

    assert client.puts == {}
    assert client.removes == []
    assert sandbox.downloaded == []


def test_persist_changes_counts_existing_objects_in_final_workspace_limit() -> None:
    client = FakeMinIO({"agents/researcher/existing.txt": b"0123456789"})
    store = MinIOWorkspaceStore(
        client=client,
        bucket="agentteams",
        member_prefix="agents/researcher",
        max_total_bytes=25,
    )
    sandbox = FakeSandbox()
    content = b"content:/workspace/a"

    with pytest.raises(RuntimeError, match="final persistence size limit"):
        store.persist_changes(
            sandbox,
            (
                WorkspaceChange(
                    path="a",
                    sha256=hashlib.sha256(content).hexdigest(),
                    size=len(content),
                    deleted=False,
                ),
            ),
        )

    assert client.puts == {}
    assert client.removes == []
    assert sandbox.downloaded == []


def test_persist_changes_enforces_final_file_count_with_existing_objects() -> None:
    client = FakeMinIO({"agents/researcher/existing.txt": b"existing"})
    store = MinIOWorkspaceStore(
        client=client,
        bucket="agentteams",
        member_prefix="agents/researcher",
        max_files=1,
    )
    sandbox = FakeSandbox()
    content = b"content:/workspace/new.txt"

    with pytest.raises(RuntimeError, match="final persistence file limit"):
        store.persist_changes(
            sandbox,
            (
                WorkspaceChange(
                    path="new.txt",
                    sha256=hashlib.sha256(content).hexdigest(),
                    size=len(content),
                    deleted=False,
                ),
            ),
        )

    assert client.puts == {}
    assert client.removes == []
    assert sandbox.downloaded == []


def test_persist_changes_allows_deletion_that_restores_final_file_limit() -> None:
    client = FakeMinIO(
        {
            "agents/researcher/keep.txt": b"keep",
            "agents/researcher/delete.txt": b"delete",
        }
    )
    store = MinIOWorkspaceStore(
        client=client,
        bucket="agentteams",
        member_prefix="agents/researcher",
        max_files=1,
    )
    sandbox = FakeSandbox()

    store.persist_changes(
        sandbox,
        (WorkspaceChange(path="delete.txt", sha256=None, size=0, deleted=True),),
    )

    assert client.puts == {}
    assert client.removes == ["agents/researcher/delete.txt"]
    assert sandbox.downloaded == []


def test_persist_changes_rejects_existing_noncanonical_object_alias() -> None:
    client = FakeMinIO({"agents/researcher/project//main.py": b"alias"})
    store = MinIOWorkspaceStore(
        client=client,
        bucket="agentteams",
        member_prefix="agents/researcher",
    )
    sandbox = FakeSandbox()

    with pytest.raises(ValueError, match="workspace object path is invalid"):
        store.persist_changes(
            sandbox,
            (WorkspaceChange(path="project/main.py", sha256=None, size=0, deleted=True),),
        )

    assert client.removes == []
