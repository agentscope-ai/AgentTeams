from pathlib import Path
from types import SimpleNamespace

import httpx

from deepagents_agentteams.config import MatrixConfig
from deepagents_agentteams.matrix import (
    ControllerMatrixTokenProvider,
    MatrixMessage,
    MatrixTransport,
    matrix_message_from_event,
)


def matrix_token() -> str:
    return "matrix-test-credential"


def expired_matrix_token() -> str:
    return "expired-matrix-test-credential"


def fresh_matrix_token() -> str:
    return "fresh-matrix-test-credential"


def event(
    *,
    sender: str = "@human:example.org",
    event_id: str = "$event",
    content: dict | None = None,
) -> SimpleNamespace:
    event_content = content or {"msgtype": "m.text", "body": "please investigate"}
    return SimpleNamespace(
        sender=sender,
        event_id=event_id,
        body=event_content["body"],
        source={"content": event_content},
    )


def test_extracts_stable_matrix_thread_root_and_filters_rooms_and_self() -> None:
    room = SimpleNamespace(room_id="!allowed:example.org")
    threaded = event(
        event_id="$reply",
        content={
            "msgtype": "m.text",
            "body": "approve 1",
            "m.relates_to": {"rel_type": "m.thread", "event_id": "$root"},
        },
    )

    message = matrix_message_from_event(
        room,
        threaded,
        allowed_room_ids=frozenset({"!allowed:example.org"}),
        own_user_id="@worker:example.org",
    )

    assert message is not None
    assert message.thread_root_event_id == "$root"
    assert message.body == "approve 1"
    assert (
        matrix_message_from_event(
            SimpleNamespace(room_id="!other:example.org"),
            threaded,
            allowed_room_ids=frozenset({"!allowed:example.org"}),
            own_user_id="@worker:example.org",
        )
        is None
    )
    assert (
        matrix_message_from_event(
            room,
            event(sender="@worker:example.org"),
            allowed_room_ids=frozenset({"!allowed:example.org"}),
            own_user_id="@worker:example.org",
        )
        is None
    )


async def test_send_reply_preserves_thread_and_structured_mention(tmp_path: Path) -> None:
    class FakeClient:
        def __init__(self) -> None:
            self.sent = []

        async def room_send(self, **kwargs) -> None:  # noqa: ANN003
            self.sent.append(kwargs)

    client = FakeClient()
    transport = MatrixTransport(
        config=MatrixConfig(
            homeserver_url="https://matrix.example.org",
            user_id="@worker:example.org",
            room_id="!room:example.org",
            access_token=matrix_token(),
            encryption_enabled=True,
        ),
        allowed_room_ids=frozenset({"!room:example.org"}),
        state_dir=tmp_path,
        on_message=lambda _message: None,
    )
    transport.client = client
    incoming = MatrixMessage(
        room_id="!room:example.org",
        event_id="$request",
        thread_root_event_id="$root",
        sender="@human:example.org",
        body="task",
    )

    await transport.send_reply(incoming, "completed")

    content = client.sent[0]["content"]
    assert content["m.relates_to"]["rel_type"] == "m.thread"
    assert content["m.relates_to"]["event_id"] == "$root"
    assert content["m.mentions"] == {"user_ids": ["@human:example.org"]}
    assert client.sent[0]["room_id"] == "!room:example.org"


async def test_controller_matrix_token_provider_reads_fresh_service_account_token(tmp_path: Path) -> None:
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, json={"access_token": fresh_matrix_token()})

    token_path = tmp_path / "token"
    token_path.write_text("first-service-account-token")
    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
        provider = ControllerMatrixTokenProvider(
            controller_url="http://controller:8090",
            service_account_token_path=token_path,
            client=client,
        )
        token_path.write_text("rotated-service-account-token")
        token = await provider.refresh()

    assert token == fresh_matrix_token()
    assert requests[0].url.path == "/api/v1/credentials/matrix-token"
    assert requests[0].headers["Authorization"] == "Bearer rotated-service-account-token"


async def test_connect_refreshes_expired_matrix_token_once(tmp_path: Path) -> None:
    class FakeClient:
        def __init__(self) -> None:
            self.access_token = ""
            self.user_id = ""
            self.user = ""
            self.device_id = ""
            self.event_callbacks = []
            self.whoami_tokens: list[str] = []

        async def whoami(self):  # noqa: ANN202
            self.whoami_tokens.append(self.access_token)
            if self.access_token == expired_matrix_token():
                return SimpleNamespace(status_code=401, errcode="M_UNKNOWN_TOKEN")
            from nio import WhoamiResponse

            return WhoamiResponse("@worker:example.org", "DEVICE", False)

        def add_event_callback(self, callback, event_filter) -> None:  # noqa: ANN001
            self.event_callbacks.append((callback, event_filter))

    refreshes = 0

    async def refresh_token() -> str:
        nonlocal refreshes
        refreshes += 1
        return fresh_matrix_token()

    client = FakeClient()
    transport = MatrixTransport(
        config=MatrixConfig(
            homeserver_url="https://matrix.example.org",
            user_id="@worker:example.org",
            room_id="!room:example.org",
            access_token=expired_matrix_token(),
            encryption_enabled=False,
        ),
        allowed_room_ids=frozenset({"!room:example.org"}),
        state_dir=tmp_path,
        on_message=lambda _message: None,
        refresh_access_token=refresh_token,
        client_factory=lambda *_args, **_kwargs: client,
    )

    await transport._connect()

    assert refreshes == 1
    assert client.whoami_tokens == [expired_matrix_token(), fresh_matrix_token()]
