import base64
import json
import tempfile
from pathlib import Path

import httpx
import pytest

from deepagents_agentteams.sandbox import AgentTeamsSandbox, SandboxControlClient


def service_account_token() -> str:
    return "service-account-token"


def runner_token() -> str:
    return "r" * 48


def test_control_client_polls_until_ready_and_refreshes_service_account_token() -> None:
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
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
    assert len(requests) == 2
    assert requests[0].headers["Authorization"] == f"Bearer {service_account_token()}"
    assert json.loads(requests[0].content) == {"sessionId": "atd-thread-hash"}


def test_execute_retries_transport_failure_with_the_same_request_id() -> None:
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
        payload = json.loads(request.content)
        runner_payloads.append(payload)
        if len(runner_payloads) == 1:
            raise httpx.ReadTimeout("response lost", request=request)
        return httpx.Response(
            200,
            json={
                "request_id": payload["request_id"],
                "output": "done",
                "exit_code": 0,
                "truncated": False,
                "changes": [],
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
        sandbox = AgentTeamsSandbox(
            control=control,
            session_id="atd-thread-hash",
            client=client,
        )

        result = sandbox.execute("printf done", timeout=17)

    assert result.output == "done"
    assert result.exit_code == 0
    assert len(runner_payloads) == 2
    assert runner_payloads[0]["request_id"] == runner_payloads[1]["request_id"]
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
    assert len(runner_request_ids) == 2
    assert runner_request_ids[0] == runner_request_ids[1]
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
