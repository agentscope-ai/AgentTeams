import io
from types import SimpleNamespace

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

    def upload_files(self, files: list[tuple[str, bytes]]) -> list[FileUploadResponse]:
        self.uploaded.extend(files)
        return [FileUploadResponse(path=path) for path, _content in files]

    def download_files(self, paths: list[str]) -> list[FileDownloadResponse]:
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


def test_persist_changes_uploads_changed_files_and_removes_deleted_objects() -> None:
    client = FakeMinIO({})
    store = MinIOWorkspaceStore(
        client=client,
        bucket="agentteams",
        member_prefix="agents/researcher",
    )
    sandbox = FakeSandbox()

    store.persist_changes(
        sandbox,
        (
            WorkspaceChange(path="project/main.py", sha256="digest", size=12, deleted=False),
            WorkspaceChange(path="old.txt", sha256=None, size=0, deleted=True),
        ),
    )

    assert client.puts == {
        "agents/researcher/project/main.py": b"content:/workspace/project/main.py"
    }
    assert client.removes == ["agents/researcher/old.txt"]
