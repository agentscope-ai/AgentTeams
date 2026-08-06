import base64
import json
import tempfile
from pathlib import Path

import httpx
import pytest
from deepagents.backends.sandbox import BaseSandbox

from deepagents_agentteams.sandbox import AgentTeamsSandbox, SandboxControlClient


def service_account_token() -> str:
    return "service-account-token"


def runner_token() -> str:
    return "r" * 48


def test_sandbox_does_not_advertise_unmounted_capture_offload() -> None:
    assert AgentTeamsSandbox.enable_capture_offload is False


def test_sandbox_overrides_sync_and_async_filesystem_shell_fallbacks() -> None:
    for method_name in (
        "ls",
        "als",
        "read",
        "aread",
        "write",
        "awrite",
        "edit",
        "aedit",
        "delete",
        "adelete",
        "grep",
        "agrep",
        "glob",
        "aglob",
    ):
        assert getattr(AgentTeamsSandbox, method_name) is not getattr(BaseSandbox, method_name)


def test_control_client_polls_until_ready_and_refreshes_service_account_token() -> None:
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        if request.method == "GET" and request.url.path == "/healthz":
            return httpx.Response(200, json={"status": "ok"})
        if len(requests) == 1:
            return httpx.Response(202, json={"name": "exec-worker-hash", "phase": "Pending"})
        return httpx.Response(
            200,
            json={
                "name": "exec-worker-hash",
                "phase": "Ready",
                "endpoint": "http://exec-worker-hash:8080",
                "token": runner_token(),
            },
        )

    with tempfile.TemporaryDirectory() as directory:
        token_path = Path(directory, "token")
        token_path.write_text(service_account_token())
        client = httpx.Client(transport=httpx.MockTransport(handler))
        control = SandboxControlClient(
            controller_url="http://controller:8090",
            worker_name="researcher-cr",
            service_account_token_path=token_path,
            client=client,
            poll_interval_seconds=0,
        )

        lease = control.ensure_ready("atd-thread-hash")

    assert lease.name == "exec-worker-hash"
    assert lease.endpoint == "http://exec-worker-hash:8080"
    assert lease.token == runner_token()
    assert len(requests) == 3
    assert requests[0].headers["Authorization"] == f"Bearer {service_account_token()}"
    assert json.loads(requests[0].content) == {"sessionId": "atd-thread-hash"}


def test_execute_waits_for_runner_health_before_single_side_effecting_post() -> None:
    health_requests = 0
    execute_requests = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal health_requests, execute_requests
        if request.url.host == "controller":
            return httpx.Response(
                200,
                json={
                    "name": "exec-worker-hash",
                    "phase": "Ready",
                    "endpoint": "http://runner:8080",
                    "token": runner_token(),
                },
            )
        if request.method == "GET" and request.url.path == "/healthz":
            health_requests += 1
            if health_requests < 3:
                raise httpx.ConnectError("service endpoint is not ready", request=request)
            return httpx.Response(200, json={"status": "ok"})
        if request.url.path == "/v1/execute":
            execute_requests += 1
            return httpx.Response(
                200,
                json={"output": "done", "exit_code": 0, "truncated": False, "changes": []},
            )
        raise AssertionError(f"unexpected request: {request.method} {request.url}")

    with tempfile.TemporaryDirectory() as directory:
        token_path = Path(directory, "token")
        token_path.write_text(service_account_token())
        client = httpx.Client(transport=httpx.MockTransport(handler))
        control = SandboxControlClient(
            controller_url="http://controller:8090",
            worker_name="researcher-cr",
            service_account_token_path=token_path,
            client=client,
            poll_interval_seconds=0,
        )
        sandbox = AgentTeamsSandbox(control=control, session_id="atd-thread-hash", client=client)

        result = sandbox.execute("printf done")

    assert result.output == "done"
    assert health_requests == 3
    assert execute_requests == 1


def test_execute_returns_unknown_after_exactly_one_ambiguous_runner_post() -> None:
    runner_payloads: list[dict[str, object]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "controller":
            return httpx.Response(
                200,
                json={
                    "name": "exec-worker-hash",
                    "phase": "Ready",
                    "endpoint": "http://runner:8080",
                    "token": runner_token(),
                },
            )
        if request.method == "GET" and request.url.path == "/healthz":
            return httpx.Response(200, json={"status": "ok"})
        payload = json.loads(request.content)
        runner_payloads.append(payload)
        raise httpx.ReadTimeout("response lost", request=request)

    with tempfile.TemporaryDirectory() as directory:
        token_path = Path(directory, "token")
        token_path.write_text(service_account_token())
        client = httpx.Client(transport=httpx.MockTransport(handler))
        control = SandboxControlClient(
            controller_url="http://controller:8090",
            worker_name="researcher-cr",
            service_account_token_path=token_path,
            client=client,
        )
        sandbox = AgentTeamsSandbox(
            control=control,
            session_id="atd-thread-hash",
            client=client,
        )

        result = sandbox.execute("printf done", timeout=17)

    assert "unknown" in result.output.lower()
    assert result.exit_code is None
    assert len(runner_payloads) == 1
    assert runner_payloads[0]["timeout_seconds"] == 17


def test_execute_fails_closed_when_runner_result_remains_ambiguous() -> None:
    runner_request_ids: list[str] = []
    ensure_requests = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal ensure_requests
        if request.url.host == "controller":
            if request.url.path.endswith("/ensure"):
                ensure_requests += 1
            return httpx.Response(
                200,
                json={
                    "name": "exec-worker-hash",
                    "phase": "Ready",
                    "endpoint": "http://runner:8080",
                    "token": runner_token(),
                },
            )
        if request.method == "GET" and request.url.path == "/healthz":
            return httpx.Response(200, json={"status": "ok"})
        runner_request_ids.append(json.loads(request.content)["request_id"])
        raise httpx.ReadTimeout("result lost", request=request)

    with tempfile.TemporaryDirectory() as directory:
        token_path = Path(directory, "token")
        token_path.write_text(service_account_token())
        client = httpx.Client(transport=httpx.MockTransport(handler))
        control = SandboxControlClient(
            controller_url="http://controller:8090",
            worker_name="researcher-cr",
            service_account_token_path=token_path,
            client=client,
        )
        sandbox = AgentTeamsSandbox(control=control, session_id="atd-thread-hash", client=client)

        result = sandbox.execute("possibly-side-effecting-command")

    assert result.exit_code is None
    assert "unknown" in result.output.lower()
    assert len(runner_request_ids) == 1
    assert ensure_requests == 1


@pytest.mark.parametrize("reclaimed_status", [404, 410])
def test_execute_replaces_reclaimed_lease_before_runner_request(reclaimed_status: int) -> None:
    controller_requests: list[str] = []
    runner_requests: list[httpx.Request] = []

    class RecordingWorkspace:
        def __init__(self) -> None:
            self.hydrated_endpoints: list[str] = []

        def hydrate(self, sandbox: AgentTeamsSandbox) -> None:
            assert sandbox._lease is not None  # noqa: SLF001 - verifies replacement hydration sees the new lease.
            self.hydrated_endpoints.append(sandbox._lease.endpoint)  # noqa: SLF001

        def persist_changes(self, sandbox: AgentTeamsSandbox, changes) -> None:  # noqa: ANN001
            return None

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "controller":
            controller_requests.append(request.url.path)
            if request.url.path.endswith("/ensure"):
                endpoint = (
                    "http://old-runner:8080"
                    if controller_requests.count(request.url.path) == 1
                    else "http://new-runner:8080"
                )
                return httpx.Response(
                    200,
                    json={
                        "name": "exec-worker-hash",
                        "phase": "Ready",
                        "endpoint": endpoint,
                        "token": runner_token(),
                    },
                )
            return httpx.Response(reclaimed_status, request=request)
        if request.method == "GET" and request.url.path == "/healthz":
            return httpx.Response(200, json={"status": "ok"})
        runner_requests.append(request)
        return httpx.Response(
            200,
            json={"output": "done", "exit_code": 0, "truncated": False, "changes": []},
        )

    with tempfile.TemporaryDirectory() as directory:
        token_path = Path(directory, "token")
        token_path.write_text(service_account_token())
        client = httpx.Client(transport=httpx.MockTransport(handler))
        control = SandboxControlClient(
            controller_url="http://controller:8090",
            worker_name="researcher-cr",
            service_account_token_path=token_path,
            client=client,
        )
        workspace = RecordingWorkspace()
        sandbox = AgentTeamsSandbox(
            control=control,
            session_id="atd-thread-hash",
            client=client,
            workspace_store=workspace,
        )

        result = sandbox.execute("printf done")

    assert result.output == "done"
    assert controller_requests.count("/api/v1/workers/researcher-cr/execution-sandboxes/ensure") == 2
    assert controller_requests.count("/api/v1/workers/researcher-cr/execution-sandboxes/atd-thread-hash/heartbeat") == 1
    assert [request.url.host for request in runner_requests] == ["new-runner"]
    assert workspace.hydrated_endpoints == ["http://old-runner:8080", "http://new-runner:8080"]


def test_execute_propagates_non_reclaimed_heartbeat_error_without_runner_request() -> None:
    runner_requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "controller":
            if request.url.path.endswith("/ensure"):
                return httpx.Response(
                    200,
                    json={
                        "name": "exec-worker-hash",
                        "phase": "Ready",
                        "endpoint": "http://runner:8080",
                        "token": runner_token(),
                    },
                )
            return httpx.Response(500, request=request)
        if request.method == "GET" and request.url.path == "/healthz":
            return httpx.Response(200, json={"status": "ok"})
        runner_requests.append(request)
        return httpx.Response(200, json={})

    with tempfile.TemporaryDirectory() as directory:
        token_path = Path(directory, "token")
        token_path.write_text(service_account_token())
        client = httpx.Client(transport=httpx.MockTransport(handler))
        control = SandboxControlClient(
            controller_url="http://controller:8090",
            worker_name="researcher-cr",
            service_account_token_path=token_path,
            client=client,
        )
        sandbox = AgentTeamsSandbox(control=control, session_id="atd-thread-hash", client=client)

        with pytest.raises(httpx.HTTPStatusError):
            sandbox.execute("printf done")

    assert runner_requests == []


def test_file_transfers_use_deepagents_response_contracts() -> None:
    encoded = base64.b64encode(b"downloaded").decode("ascii")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "controller":
            return httpx.Response(
                200,
                json={
                    "name": "exec-worker-hash",
                    "phase": "Ready",
                    "endpoint": "http://runner:8080",
                    "token": runner_token(),
                },
            )
        if request.method == "GET" and request.url.path == "/healthz":
            return httpx.Response(200, json={"status": "ok"})
        if request.url.path == "/v1/files/upload":
            return httpx.Response(
                200,
                json={
                    "files": [
                        {"path": "/workspace/ok.txt", "error": None},
                        {"path": "/workspace/no.txt", "error": "permission_denied"},
                    ]
                },
            )
        return httpx.Response(
            200,
            json={
                "files": [
                    {"path": "/workspace/ok.txt", "content_base64": encoded, "error": None},
                    {"path": "/workspace/no.txt", "content_base64": None, "error": "file_not_found"},
                ]
            },
        )

    with tempfile.TemporaryDirectory() as directory:
        token_path = Path(directory, "token")
        token_path.write_text(service_account_token())
        client = httpx.Client(transport=httpx.MockTransport(handler))
        control = SandboxControlClient(
            controller_url="http://controller:8090",
            worker_name="researcher-cr",
            service_account_token_path=token_path,
            client=client,
        )
        sandbox = AgentTeamsSandbox(control=control, session_id="atd-thread-hash", client=client)

        uploads = sandbox.upload_files(
            [
                ("/workspace/ok.txt", b"ok"),
                ("/workspace/no.txt", b"no"),
            ]
        )
        downloads = sandbox.download_files(["/workspace/ok.txt", "/workspace/no.txt"])

    assert uploads[0].error is None
    assert uploads[1].error == "permission_denied"
    assert downloads[0].content == b"downloaded"
    assert downloads[1].error == "file_not_found"


def test_filesystem_tools_use_bounded_file_apis_without_execute() -> None:
    """A filesystem tool must never create an unapproved command execution."""
    requested_paths: list[str] = []
    files = {"/workspace/note.txt": b"alpha\nbeta\n"}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "controller":
            return httpx.Response(
                200,
                json={
                    "name": "exec-worker-hash",
                    "phase": "Ready",
                    "endpoint": "http://runner:8080",
                    "token": runner_token(),
                },
            )
        if request.method == "GET" and request.url.path == "/healthz":
            return httpx.Response(200, json={"status": "ok"})
        requested_paths.append(request.url.path)
        payload = json.loads(request.content)
        if request.url.path == "/v1/files/download":
            path = payload["paths"][0]
            content = files.get(path)
            if content is None:
                return httpx.Response(200, json={"files": [{"path": path, "error": "file_not_found"}]})
            return httpx.Response(
                200,
                json={
                    "files": [
                        {
                            "path": path,
                            "content_base64": base64.b64encode(content).decode("ascii"),
                            "error": None,
                        }
                    ]
                },
            )
        if request.url.path == "/v1/files/upload":
            item = payload["files"][0]
            files[item["path"]] = base64.b64decode(item["content_base64"])
            return httpx.Response(200, json={"files": [{"path": item["path"], "error": None}]})
        if request.url.path == "/v1/files/list":
            return httpx.Response(
                200,
                json={
                    "entries": [{"path": "/workspace/note.txt", "is_dir": False, "size": 11}],
                    "error": None,
                },
            )
        if request.url.path == "/v1/files/grep":
            return httpx.Response(
                200,
                json={
                    "matches": [{"path": "/workspace/note.txt", "line": 1, "text": "alpha"}],
                    "error": None,
                    "truncated": False,
                },
            )
        if request.url.path == "/v1/files/glob":
            return httpx.Response(
                200,
                json={
                    "matches": [{"path": "/workspace/note.txt", "is_dir": False, "size": 11}],
                    "error": None,
                    "truncated": False,
                },
            )
        if request.url.path == "/v1/files/delete":
            files.pop(payload["path"], None)
            return httpx.Response(
                200,
                json={
                    "path": payload["path"],
                    "error": None,
                    "changes": [
                        {"path": "note.txt", "sha256": None, "size": 0, "deleted": True}
                    ],
                },
            )
        raise AssertionError(f"unexpected request: {request.method} {request.url}")

    with tempfile.TemporaryDirectory() as directory:
        token_path = Path(directory, "token")
        token_path.write_text(service_account_token())
        client = httpx.Client(transport=httpx.MockTransport(handler))
        control = SandboxControlClient(
            controller_url="http://controller:8090",
            worker_name="researcher-cr",
            service_account_token_path=token_path,
            client=client,
        )
        sandbox = AgentTeamsSandbox(control=control, session_id="atd-thread-hash", client=client)

        assert sandbox.read("/workspace/note.txt", offset=1, limit=1).file_data == {
            "content": "beta\n",
            "encoding": "utf-8",
        }
        assert sandbox.write("/workspace/new.txt", "new").path == "/workspace/new.txt"
        assert sandbox.edit("/workspace/new.txt", "new", "updated").occurrences == 1
        assert sandbox.ls("/workspace").entries == [
            {"path": "/workspace/note.txt", "is_dir": False, "size": 11}
        ]
        assert sandbox.grep("alpha", "/workspace").matches == [
            {"path": "/workspace/note.txt", "line": 1, "text": "alpha"}
        ]
        assert sandbox.glob("*.txt", "/workspace").matches == [
            {"path": "/workspace/note.txt", "is_dir": False, "size": 11}
        ]
        assert sandbox.delete("/workspace/note.txt").path == "/workspace/note.txt"

    assert "/v1/execute" not in requested_paths
    assert set(requested_paths) == {
        "/v1/files/download",
        "/v1/files/upload",
        "/v1/files/list",
        "/v1/files/grep",
        "/v1/files/glob",
        "/v1/files/delete",
    }


def test_mutating_file_tools_persist_exact_change_manifests() -> None:
    files = {"/workspace/note.txt": b"before"}

    class RecordingWorkspace:
        def __init__(self) -> None:
            self.persisted: list[tuple] = []

        def hydrate(self, sandbox: AgentTeamsSandbox) -> None:
            return None

        def persist_changes(self, sandbox: AgentTeamsSandbox, changes: tuple) -> None:
            self.persisted.append(changes)

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "controller":
            return httpx.Response(
                200,
                json={
                    "name": "exec-worker-hash",
                    "phase": "Ready",
                    "endpoint": "http://runner:8080",
                    "token": runner_token(),
                },
            )
        if request.method == "GET" and request.url.path == "/healthz":
            return httpx.Response(200, json={"status": "ok"})
        payload = json.loads(request.content)
        if request.url.path == "/v1/files/download":
            path = payload["paths"][0]
            return httpx.Response(
                200,
                json={
                    "files": [
                        {
                            "path": path,
                            "content_base64": base64.b64encode(files[path]).decode("ascii"),
                            "error": None,
                        }
                    ]
                },
            )
        if request.url.path == "/v1/files/upload":
            item = payload["files"][0]
            files[item["path"]] = base64.b64decode(item["content_base64"])
            return httpx.Response(200, json={"files": [{"path": item["path"], "error": None}]})
        if request.url.path == "/v1/files/delete":
            files.pop(payload["path"])
            return httpx.Response(
                200,
                json={
                    "path": payload["path"],
                    "error": None,
                    "changes": [
                        {"path": "note.txt", "sha256": None, "size": 0, "deleted": True}
                    ],
                },
            )
        raise AssertionError(f"unexpected request: {request.method} {request.url}")

    with tempfile.TemporaryDirectory() as directory:
        token_path = Path(directory, "token")
        token_path.write_text(service_account_token())
        client = httpx.Client(transport=httpx.MockTransport(handler))
        control = SandboxControlClient(
            controller_url="http://controller:8090",
            worker_name="researcher-cr",
            service_account_token_path=token_path,
            client=client,
        )
        workspace = RecordingWorkspace()
        sandbox = AgentTeamsSandbox(
            control=control,
            session_id="atd-thread-hash",
            client=client,
            workspace_store=workspace,
        )

        assert sandbox.write("/workspace/new.txt", "created").error is None
        assert sandbox.edit("/workspace/note.txt", "before", "after").error is None
        assert sandbox.delete("/workspace/note.txt").error is None

    assert len(workspace.persisted) == 3
    written, edited, deleted = (batch[0] for batch in workspace.persisted)
    assert (written.path, written.size, written.deleted) == ("new.txt", 7, False)
    assert (edited.path, edited.size, edited.deleted) == ("note.txt", 5, False)
    assert (deleted.path, deleted.sha256, deleted.size, deleted.deleted) == ("note.txt", None, 0, True)


def test_workspace_is_hydrated_once_and_command_changes_are_persisted() -> None:
    class RecordingWorkspace:
        def __init__(self) -> None:
            self.hydrate_calls = 0
            self.persisted = []

        def hydrate(self, sandbox: AgentTeamsSandbox) -> None:
            self.hydrate_calls += 1

        def persist_changes(self, sandbox: AgentTeamsSandbox, changes) -> None:  # noqa: ANN001
            self.persisted.append(changes)

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.host == "controller":
            return httpx.Response(
                200,
                json={
                    "name": "exec-worker-hash",
                    "phase": "Ready",
                    "endpoint": "http://runner:8080",
                    "token": runner_token(),
                },
            )
        if request.method == "GET" and request.url.path == "/healthz":
            return httpx.Response(200, json={"status": "ok"})
        payload = json.loads(request.content)
        return httpx.Response(
            200,
            json={
                "request_id": payload["request_id"],
                "output": "changed",
                "exit_code": 0,
                "truncated": False,
                "changes": [
                    {
                        "path": "report.txt",
                        "sha256": "digest",
                        "size": 7,
                        "deleted": False,
                    }
                ],
            },
        )

    with tempfile.TemporaryDirectory() as directory:
        token_path = Path(directory, "token")
        token_path.write_text(service_account_token())
        client = httpx.Client(transport=httpx.MockTransport(handler))
        control = SandboxControlClient(
            controller_url="http://controller:8090",
            worker_name="researcher-cr",
            service_account_token_path=token_path,
            client=client,
        )
        workspace = RecordingWorkspace()
        sandbox = AgentTeamsSandbox(
            control=control,
            session_id="atd-thread-hash",
            client=client,
            workspace_store=workspace,
        )

        sandbox.execute("first")
        sandbox.execute("second")

    assert workspace.hydrate_calls == 1
    assert len(workspace.persisted) == 2
    assert workspace.persisted[0][0].path == "report.txt"
