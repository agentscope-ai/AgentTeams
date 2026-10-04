from unittest import mock

from cli_harness_worker.matrix import MatrixLoop, _mentions_user, _split_message


def test_split_message_short():
    assert _split_message("hello") == ["hello"]


def test_split_message_long_chunks():
    text = "x" * 20000
    chunks = _split_message(text, limit=8000)
    assert all(len(c) <= 8000 for c in chunks)
    assert "".join(chunks).replace("\n", "") == text


def test_mentions_user_full_mxid_and_localpart():
    assert _mentions_user("hi @alice:example.org please", "@alice:example.org")
    assert _mentions_user("hi @alice: please", "@alice:example.org")
    assert not _mentions_user("no mention", "@alice:example.org")


def test_seen_event_dedup():
    loop = MatrixLoop(
        homeserver="http://h",
        user_id="@w:x",
        access_token="t",
        own_room_id="!room:x",
        on_task=None,
    )

    class FakeEvent:
        def __init__(self, event_id, sender, body):
            self.event_id = event_id
            self.sender = sender
            self.body = body

    class FakeRoom:
        room_id = "!room:x"

    import asyncio

    async def main():
        await loop._on_message(FakeRoom(), FakeEvent("$1", "@a:x", "task one"))
        await loop._on_message(FakeRoom(), FakeEvent("$1", "@a:x", "task one"))
        await loop._on_message(FakeRoom(), FakeEvent("$2", "@w:x", "self msg"))
        return loop._queue.qsize()

    assert asyncio.run(main()) == 1


def test_catch_up_sync_suppresses_callbacks_only_during_history_sync():
    import asyncio

    loop = MatrixLoop(
        homeserver="http://h",
        user_id="@w:x",
        access_token="t",
        own_room_id="!room:x",
        on_task=None,
    )

    class FakeResponse:
        next_batch = "next-token"

    sync_calls: list[bool] = []

    async def fake_sync(*args, **kwargs):
        sync_calls.append(len(loop.client.event_callbacks))
        return FakeResponse()

    loop.client.event_callbacks = [object(), object()]
    loop.client.sync = fake_sync

    asyncio.run(loop.catch_up_sync())

    assert sync_calls == [0], "historical sync must not dispatch callbacks"
    assert len(loop.client.event_callbacks) == 2


def test_catch_up_sync_restores_callbacks_even_on_failure():
    import asyncio

    loop = MatrixLoop(
        homeserver="http://h",
        user_id="@w:x",
        access_token="t",
        own_room_id="!room:x",
        on_task=None,
    )

    loop.client.event_callbacks = [object()]

    async def failing_sync(*args, **kwargs):
        raise RuntimeError("homeserver down")

    loop.client.sync = failing_sync

    try:
        asyncio.run(loop.catch_up_sync())
    except RuntimeError:
        pass
    else:
        raise AssertionError("expected the sync failure to propagate")
    assert len(loop.client.event_callbacks) == 1


def test_run_starts_catch_up_before_forever_loop():
    import asyncio

    loop = MatrixLoop(
        homeserver="http://h",
        user_id="@w:x",
        access_token="t",
        own_room_id="!room:x",
        on_task=None,
    )

    order: list[str] = []

    async def fake_catch_up():
        order.append("catch-up")

    async def fake_forever(*args, **kwargs):
        order.append("forever")
        return None

    loop.catch_up_sync = fake_catch_up
    loop.client.sync_forever = fake_forever
    loop._worker_task = None

    asyncio.run(loop.run())
    assert order == ["catch-up", "forever"]
