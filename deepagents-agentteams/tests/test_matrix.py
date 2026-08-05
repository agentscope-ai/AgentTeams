import asyncio
import json
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


def sync_response(next_batch: str) -> SyncResponse:
    response = SyncResponse.from_dict(
        {
            "next_batch": next_batch,
            "rooms": {"invite": {}, "join": {}, "leave": {}},
            "presence": {"events": []},
            "account_data": {"events": []},
            "to_device": {"events": []},
            "device_lists": {"changed": [], "left": []},
            "device_one_time_keys_count": {},
        }
    )
    assert isinstance(response, SyncResponse)
    return response


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


async def test_sync_journals_event_before_cursor_and_completes_before_read_marker(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    operations: list[str] = []

    class FakeClient:
        olm = None

        async def room_read_markers(self, *_args, **_kwargs) -> None:  # noqa: ANN002, ANN003
            operations.append("read")

    async def handle_message(_message: MatrixMessage) -> None:
        assert (tmp_path / "sync-token").read_text() == "next-batch"
        journal = json.loads((tmp_path / "matrix-events.json").read_text())
        assert journal["events"]["$task"]["status"] == "processing"
        operations.append("handled")

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
        on_message=handle_message,
    )
    transport.client = FakeClient()
    original_save = transport._save_sync_token

    def observe_cursor_save(token: str) -> None:
        journal = json.loads((tmp_path / "matrix-events.json").read_text())
        assert journal["events"]["$task"]["status"] == "pending"
        operations.append("cursor")
        original_save(token)

    monkeypatch.setattr(transport, "_save_sync_token", observe_cursor_save)

    await transport._schedule_message(
        SimpleNamespace(room_id="!allowed:example.org"),
        event(event_id="$task"),
    )
    assert operations == []
    await transport._accept_sync_response(sync_response("next-batch"))

    journal = json.loads((tmp_path / "matrix-events.json").read_text())
    assert journal["events"]["$task"]["status"] == "completed"
    assert operations == ["cursor", "handled", "read"]

    monkeypatch.setattr(transport, "_save_sync_token", original_save)
    await transport._schedule_message(
        SimpleNamespace(room_id="!allowed:example.org"),
        event(event_id="$task"),
    )
    await transport._accept_sync_response(sync_response("later-batch"))
    assert operations == ["cursor", "handled", "read"]


async def test_pending_event_after_cursor_persistence_is_handled_once_after_restart(tmp_path: Path) -> None:
    first = MatrixTransport(
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
    first.client = SimpleNamespace(olm=None)
    await first._schedule_message(
        SimpleNamespace(room_id="!allowed:example.org"),
        event(event_id="$pending"),
    )

    async def crash_before_drain() -> None:
        raise asyncio.CancelledError

    first._drain_journal = crash_before_drain  # type: ignore[method-assign]
    with pytest.raises(asyncio.CancelledError):
        await first._accept_sync_response(sync_response("pending-cursor"))

    handled: list[str] = []
    read: list[str] = []

    class RecoveryClient:
        async def room_read_markers(self, _room_id, *, fully_read_event, read_event) -> None:  # noqa: ANN001
            assert fully_read_event == read_event
            read.append(read_event)

    async def handle(message: MatrixMessage) -> None:
        handled.append(message.event_id)

    recovered = MatrixTransport(
        config=first._config,  # noqa: SLF001 - restart uses the exact same projected runtime.
        allowed_room_ids=frozenset({"!allowed:example.org"}),
        state_dir=tmp_path,
        on_message=handle,
    )
    recovered.client = RecoveryClient()
    await recovered._drain_journal()
    await recovered._drain_journal()

    assert handled == ["$pending"]
    assert read == ["$pending"]


@pytest.mark.parametrize(
    ("event_id", "body"),
    [("$task", "please investigate"), ("$approval", "approve 1")],
)
async def test_restart_fails_closed_for_processing_event_without_reinvoking_handler(
    tmp_path: Path,
    event_id: str,
    body: str,
) -> None:
    handler_started = asyncio.Event()
    never_complete = asyncio.Event()
    handler_calls: list[str] = []

    class FirstClient:
        olm = None

        async def room_read_markers(self, *_args, **_kwargs) -> None:  # noqa: ANN002, ANN003
            raise AssertionError("processing events must not be marked read")

    async def interrupted_handler(message: MatrixMessage) -> None:
        handler_calls.append(message.event_id)
        handler_started.set()
        await never_complete.wait()

    first = MatrixTransport(
        config=MatrixConfig(
            homeserver_url="https://matrix.example.org",
            user_id="@worker:example.org",
            room_id="!allowed:example.org",
            access_token=matrix_token(),
            encryption_enabled=False,
        ),
        allowed_room_ids=frozenset({"!allowed:example.org"}),
        state_dir=tmp_path,
        on_message=interrupted_handler,
    )
    first.client = FirstClient()
    await first._schedule_message(
        SimpleNamespace(room_id="!allowed:example.org"),
        event(event_id=event_id, content={"msgtype": "m.text", "body": body}),
    )
    accepting = asyncio.create_task(first._accept_sync_response(sync_response("durable-cursor")))
    await handler_started.wait()
    accepting.cancel()
    with pytest.raises(asyncio.CancelledError):
        await accepting

    persisted = json.loads((tmp_path / "matrix-events.json").read_text())
    assert persisted["events"][event_id]["status"] == "processing"
    assert (tmp_path / "sync-token").read_text() == "durable-cursor"

    recovery_operations: list[str] = []

    class RecoveryClient:
        olm = None

        async def room_send(self, **kwargs):  # noqa: ANN003, ANN202
            recovery_operations.append(f"reply:{kwargs['content']['m.relates_to']['event_id']}")
            assert "unknown" in kwargs["content"]["body"].lower()
            return RoomSendResponse("$unknown-reply", kwargs["room_id"])

        async def room_read_markers(self, *_args, **_kwargs) -> None:  # noqa: ANN002, ANN003
            recovery_operations.append("read")

    async def must_not_repeat(_message: MatrixMessage) -> None:
        raise AssertionError("a recovered processing event must never be executed again")

    recovered = MatrixTransport(
        config=first._config,  # noqa: SLF001 - restart uses the exact same projected runtime.
        allowed_room_ids=frozenset({"!allowed:example.org"}),
        state_dir=tmp_path,
        on_message=must_not_repeat,
    )
    recovered.client = RecoveryClient()
    await recovered._drain_journal()

    persisted = json.loads((tmp_path / "matrix-events.json").read_text())
    assert persisted["events"][event_id]["status"] == "completed"
    assert handler_calls == [event_id]
    assert recovery_operations == [f"reply:{event_id}", "read"]

    await recovered._schedule_message(
        SimpleNamespace(room_id="!allowed:example.org"),
        event(event_id=event_id, content={"msgtype": "m.text", "body": body}),
    )
    await recovered._accept_sync_response(sync_response("later-cursor"))
    assert handler_calls == [event_id]


@pytest.mark.parametrize("saved_token", [None, "previous-batch"])
async def test_signals_synchronized_once_after_initial_or_resumed_token_is_saved(
    tmp_path: Path,
    saved_token: str | None,
) -> None:
    if saved_token is not None:
        (tmp_path / "sync-token").write_text(saved_token)

    response = sync_response("durable-next-batch")
    later_response = sync_response("later-next-batch")

    class FakeClient:
        def __init__(self) -> None:
            self.event_callbacks = ["message-callback"]
            self.next_batch = None
            self.olm = None
            self.sync_calls: list[dict] = []

        async def sync(self, **kwargs):  # noqa: ANN003, ANN202
            self.sync_calls.append(kwargs)
            if len(self.sync_calls) == 1:
                return response
            if len(self.sync_calls) == 2:
                return later_response
            raise asyncio.CancelledError

    synchronized_tokens: list[str] = []

    async def synchronized() -> None:
        synchronized_tokens.append((tmp_path / "sync-token").read_text())

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
        on_synchronized=synchronized,
    )
    transport.client = client

    with pytest.raises(asyncio.CancelledError):
        await transport._sync_loop()

    assert synchronized_tokens == ["durable-next-batch"]
    assert (tmp_path / "sync-token").read_text() == "later-next-batch"
    if saved_token is None:
        assert client.sync_calls[0] == {"timeout": 30_000, "full_state": True}
    else:
        assert client.sync_calls[0] == {
            "timeout": 30_000,
            "since": "previous-batch",
            "full_state": False,
        }


async def test_invalid_sync_never_signals_synchronized(tmp_path: Path) -> None:
    synchronized = 0

    async def signal() -> None:
        nonlocal synchronized
        synchronized += 1

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
        on_synchronized=signal,
    )

    with pytest.raises(RuntimeError, match="Matrix sync failed"):
        await transport._accept_sync_response(SimpleNamespace(status_code=500))

    assert synchronized == 0
    assert not (tmp_path / "sync-token").exists()


async def test_sync_token_save_failure_never_signals_synchronized(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    synchronized = 0

    async def signal() -> None:
        nonlocal synchronized
        synchronized += 1

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
        on_synchronized=signal,
    )
    transport.client = SimpleNamespace(olm=None)
    monkeypatch.setattr(
        transport,
        "_save_sync_token",
        lambda _token: (_ for _ in ()).throw(OSError("disk unavailable")),
    )

    with pytest.raises(OSError, match="disk unavailable"):
        await transport._accept_sync_response(sync_response("not-durable"))

    assert synchronized == 0


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


async def test_handler_failure_remains_processing_for_fail_closed_restart(
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

    await transport._schedule_message(
        SimpleNamespace(room_id="!allowed:example.org"),
        event(),
    )
    with caplog.at_level(logging.ERROR, logger="deepagents_agentteams.matrix"), pytest.raises(
        RuntimeError, match="handler failed"
    ):
        await transport._accept_sync_response(sync_response("handler-failed-cursor"))

    journal = json.loads((tmp_path / "matrix-events.json").read_text())
    assert journal["events"]["$event"]["status"] == "processing"
    assert "Matrix message handling failed" in caplog.text
    assert "RuntimeError: handler failed" in caplog.text

    recovery_operations: list[str] = []

    class RecoveryClient:
        async def room_send(self, **kwargs):  # noqa: ANN003, ANN202
            recovery_operations.append("unknown")
            return RoomSendResponse("$unknown", kwargs["room_id"])

        async def room_read_markers(self, *_args, **_kwargs) -> None:  # noqa: ANN002, ANN003
            recovery_operations.append("read")

    async def must_not_repeat(_message: MatrixMessage) -> None:
        raise AssertionError("must not repeat")

    recovered = MatrixTransport(
        config=transport._config,  # noqa: SLF001 - restart uses the exact same projected runtime.
        allowed_room_ids=frozenset({"!allowed:example.org"}),
        state_dir=tmp_path,
        on_message=must_not_repeat,
    )
    recovered.client = RecoveryClient()
    await recovered._drain_journal()

    assert recovery_operations == ["unknown", "read"]
    journal = json.loads((tmp_path / "matrix-events.json").read_text())
    assert journal["events"]["$event"]["status"] == "completed"


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
