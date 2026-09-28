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
