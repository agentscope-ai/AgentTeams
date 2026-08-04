import asyncio
import logging
from pathlib import Path
from types import SimpleNamespace

import httpx
import pytest
from nio import JoinError, JoinResponse, RoomSendError, RoomSendResponse, SyncResponse

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

        async def room_send(self, **kwargs):  # noqa: ANN003, ANN202
            self.sent.append(kwargs)
            return RoomSendResponse("$reply", kwargs["room_id"])

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


async def test_send_reply_raises_when_matrix_rejects_the_message(tmp_path: Path) -> None:
    class FakeClient:
        async def room_send(self, **_kwargs):  # noqa: ANN003, ANN202
            return RoomSendError("denied", "M_FORBIDDEN", room_id="!room:example.org")

    transport = MatrixTransport(
        config=MatrixConfig(
            homeserver_url="https://matrix.example.org",
            user_id="@worker:example.org",
            room_id="!room:example.org",
            access_token=matrix_token(),
            encryption_enabled=False,
        ),
        allowed_room_ids=frozenset({"!room:example.org"}),
        state_dir=tmp_path,
        on_message=lambda _message: None,
    )
    transport.client = FakeClient()
    incoming = MatrixMessage(
        room_id="!room:example.org",
        event_id="$request",
        thread_root_event_id="$root",
        sender="@human:example.org",
        body="task",
    )

    with pytest.raises(RuntimeError, match="Matrix room_send failed"):
        await transport.send_reply(incoming, "completed")


async def test_send_reply_refreshes_expired_token_once(tmp_path: Path) -> None:
    class FakeClient:
        def __init__(self) -> None:
            self.access_token = expired_matrix_token()
            self.sent_tokens: list[str] = []

        async def room_send(self, **kwargs):  # noqa: ANN003, ANN202
            self.sent_tokens.append(self.access_token)
            if self.access_token == expired_matrix_token():
                return RoomSendError(
                    "expired",
                    "M_UNKNOWN_TOKEN",
                    room_id=kwargs["room_id"],
                )
            return RoomSendResponse("$reply", kwargs["room_id"])

    async def refresh_token() -> str:
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

    assert client.sent_tokens == [expired_matrix_token(), fresh_matrix_token()]


async def test_accept_sync_joins_only_controller_projected_rooms(tmp_path: Path) -> None:
    class FakeClient:
        def __init__(self) -> None:
            self.joined: list[str] = []
            self.olm = None

        async def join(self, room_id: str):  # noqa: ANN202
            self.joined.append(room_id)
            return JoinResponse(room_id)

    response = SyncResponse.from_dict(
        {
            "next_batch": "next-batch",
            "rooms": {
                "invite": {
                    "!allowed:example.org": {"invite_state": {"events": []}},
                    "!untrusted:example.org": {"invite_state": {"events": []}},
                },
                "join": {},
                "leave": {},
            },
            "presence": {"events": []},
            "account_data": {"events": []},
            "to_device": {"events": []},
            "device_lists": {"changed": [], "left": []},
            "device_one_time_keys_count": {},
        }
    )
    assert isinstance(response, SyncResponse)
    client = FakeClient()
    transport = MatrixTransport(
        config=MatrixConfig(
            homeserver_url="https://matrix.example.org",
            user_id="@worker:example.org",
            room_id="!allowed:example.org",
            access_token=matrix_token(),
            encryption_enabled=False,
        ),
        allowed_room_ids=frozenset({"!allowed:example.org"}),
        state_dir=tmp_path,
        on_message=lambda _message: None,
    )
    transport.client = client

    next_batch = await transport._accept_sync_response(response)

    assert next_batch == "next-batch"
    assert client.joined == ["!allowed:example.org"]
    assert (tmp_path / "sync-token").read_text() == "next-batch"


async def test_initial_sync_refreshes_token_when_allowed_room_join_is_unauthorized(
    tmp_path: Path,
) -> None:
    response = SyncResponse.from_dict(
        {
            "next_batch": "next-batch",
            "rooms": {
                "invite": {"!allowed:example.org": {"invite_state": {"events": []}}},
                "join": {},
                "leave": {},
            },
            "presence": {"events": []},
            "account_data": {"events": []},
            "to_device": {"events": []},
            "device_lists": {"changed": [], "left": []},
            "device_one_time_keys_count": {},
        }
    )
    assert isinstance(response, SyncResponse)

    class FakeClient:
        def __init__(self) -> None:
            self.access_token = expired_matrix_token()
            self.event_callbacks = ["callback"]
            self.next_batch = "matrix-nio-implicit-token"
            self.olm = None
            self.sync_calls: list[dict] = []
            self.join_tokens: list[str] = []

        async def sync(self, **kwargs):  # noqa: ANN003, ANN202
            self.sync_calls.append(kwargs)
            if len(self.sync_calls) > 1:
                raise asyncio.CancelledError
            assert self.next_batch is None
            self.next_batch = response.next_batch
            return response

        async def join(self, room_id: str):  # noqa: ANN202
            self.join_tokens.append(self.access_token)
            if self.access_token == expired_matrix_token():
                return JoinError("expired", "M_UNKNOWN_TOKEN")
            return JoinResponse(room_id)

    async def refresh_token() -> str:
        return fresh_matrix_token()

    client = FakeClient()
    transport = MatrixTransport(
        config=MatrixConfig(
            homeserver_url="https://matrix.example.org",
            user_id="@worker:example.org",
            room_id="!allowed:example.org",
            access_token=expired_matrix_token(),
            encryption_enabled=False,
        ),
        allowed_room_ids=frozenset({"!allowed:example.org"}),
        state_dir=tmp_path,
        on_message=lambda _message: None,
        refresh_access_token=refresh_token,
    )
    transport.client = client

    with pytest.raises(asyncio.CancelledError):
        await transport._sync_loop()

    assert client.join_tokens == [expired_matrix_token(), fresh_matrix_token()]
    assert client.event_callbacks == ["callback"]
    assert client.sync_calls == [
        {"timeout": 30_000, "full_state": True},
        {"timeout": 30_000, "since": "next-batch", "full_state": False},
    ]


async def test_message_handler_failure_is_retrieved_and_logged(
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
) -> None:
    class FakeClient:
        async def room_read_markers(self, *_args, **_kwargs) -> None:  # noqa: ANN002, ANN003
            return None

    async def fail_message(_message: MatrixMessage) -> None:
        raise RuntimeError("handler failed")

    transport = MatrixTransport(
        config=MatrixConfig(
            homeserver_url="https://matrix.example.org",
            user_id="@worker:example.org",
            room_id="!allowed:example.org",
            access_token=matrix_token(),
            encryption_enabled=False,
        ),
        allowed_room_ids=frozenset({"!allowed:example.org"}),
        state_dir=tmp_path,
        on_message=fail_message,
    )
    transport.client = FakeClient()

    with caplog.at_level(logging.ERROR, logger="deepagents_agentteams.matrix"):
        await transport._schedule_message(
            SimpleNamespace(room_id="!allowed:example.org"),
            event(),
        )
        tasks = tuple(transport._event_tasks)
        await asyncio.gather(*tasks, return_exceptions=True)

    assert transport._event_tasks == set()
    assert "Matrix message handling failed" in caplog.text
    assert "RuntimeError: handler failed" in caplog.text


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
