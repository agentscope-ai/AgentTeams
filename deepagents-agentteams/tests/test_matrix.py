from pathlib import Path
from types import SimpleNamespace

from deepagents_agentteams.config import MatrixConfig
from deepagents_agentteams.matrix import MatrixMessage, MatrixTransport, matrix_message_from_event


def matrix_token() -> str:
    return "matrix-test-credential"


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
