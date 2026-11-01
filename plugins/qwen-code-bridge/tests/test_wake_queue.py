# -*- coding: utf-8 -*-
"""Durable wake queue: a busy chat (probe or 409) must delay a wake, never drop it."""

import asyncio
import http.server
import json
import threading
import time

import pytest

from qwen_code_bridge import wake_queue


@pytest.fixture(autouse=True)
def _isolate(monkeypatch, tmp_path):
    queue_file = tmp_path / "wake-queue.json"
    monkeypatch.setattr(wake_queue, "_path", lambda: queue_file)
    wake_queue.stop()
    monkeypatch.setattr(wake_queue, "_on_delivered", None)
    monkeypatch.setattr(wake_queue, "_sender", None)
    yield
    wake_queue.stop()


def _task(n: str) -> str:
    return n * 32


def test_enqueue_persists_and_dedups() -> None:
    wake_queue.enqueue({"task_id": _task("a"), "text": "first", "session_id": "s"})
    wake_queue.enqueue({"task_id": _task("a"), "text": "second", "session_id": "s"})
    records = wake_queue.load()
    assert len(records) == 1
    assert records[0]["text"] == "second"


def test_drain_success_removes_and_calls_callback() -> None:
    delivered: list[str] = []
    calls: list[tuple] = []

    def sender(session_id, agent_id, base_url, text, channel, user_id, bell,
               approval_level=""):
        calls.append((session_id, text, approval_level))

    wake_queue._on_delivered = lambda task_id: delivered.append(task_id)
    wake_queue.enqueue(
        {
            "task_id": _task("b"),
            "text": "done",
            "session_id": "s-1",
            "approval_level": "AUTO",
        }
    )

    stats = asyncio.run(wake_queue.drain_once(sender))

    assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}
    assert wake_queue.load() == []
    assert delivered == [_task("b")]
    assert calls and calls[0][2] == "AUTO"


def test_drain_409_defers_as_busy() -> None:
    """A 409 means a chat run is already in progress: retry soon, never drop.

    Treating it as a failure used to re-inject the same text with the failure
    backoff (the duplicate-wake reports)."""

    def sender(*_args, **_kwargs):
        raise RuntimeError("wake inject failed: http 409")

    wake_queue.enqueue({"task_id": _task("c"), "text": "done", "session_id": "s"})
    before = time.time()

    stats = asyncio.run(wake_queue.drain_once(sender))

    assert stats == {"sent": 0, "failed": 0, "dropped": 0, "busy": 1}
    record = wake_queue.load()[0]
    assert record["busy_retries"] == 1
    assert record["next_at"] <= before + wake_queue.BUSY_RETRY_S + 1
    assert "attempts" not in record or record["attempts"] == 0


def test_drain_failure_reschedules_with_backoff() -> None:
    def sender(*_args, **_kwargs):
        raise RuntimeError("wake inject failed: connection refused")

    wake_queue.enqueue({"task_id": _task("c2"), "text": "done", "session_id": "s"})
    before = time.time()

    stats = asyncio.run(wake_queue.drain_once(sender))

    assert stats == {"sent": 0, "failed": 1, "dropped": 0, "busy": 0}
    record = wake_queue.load()[0]
    assert record["attempts"] == 1
    assert record["next_at"] >= before + wake_queue.BACKOFF_S[0]


def test_drain_drops_an_already_delivered_wake() -> None:
    """After a crash, a record whose task already reports 'sent' is dropped."""
    calls = []
    wake_queue.enqueue({"task_id": _task("x"), "text": "done", "session_id": "s"})

    stats = asyncio.run(
        wake_queue.drain_once(
            lambda *a, **k: calls.append(a), is_delivered=lambda task_id: True
        )
    )

    assert stats == {"sent": 0, "failed": 0, "dropped": 1, "busy": 0}
    assert wake_queue.load() == []
    assert calls == []


def test_drain_drops_after_max_attempts() -> None:
    wake_queue.enqueue(
        {"task_id": _task("m"), "text": "done", "session_id": "s", "attempts": wake_queue.MAX_ATTEMPTS}
    )
    stats = asyncio.run(wake_queue.drain_once(lambda *a, **k: None))
    assert stats["dropped"] == 1
    assert wake_queue.load() == []


def test_drain_drops_after_max_age() -> None:
    wake_queue.enqueue({"task_id": _task("d"), "text": "done", "session_id": "s"})
    records = wake_queue.load()
    records[0]["created_at"] = time.time() - wake_queue.MAX_AGE_S - 60
    wake_queue._save(records)

    called = []
    stats = asyncio.run(
        wake_queue.drain_once(lambda *a, **k: called.append(a))
    )

    assert stats == {"sent": 0, "failed": 0, "dropped": 1, "busy": 0}
    assert wake_queue.load() == []
    assert called == []


def test_discard_removes_only_target() -> None:
    wake_queue.enqueue({"task_id": _task("e"), "text": "x", "session_id": "s"})
    wake_queue.enqueue({"task_id": _task("f"), "text": "y", "session_id": "s"})
    wake_queue.discard(_task("e"))
    assert [r["task_id"] for r in wake_queue.load()] == [_task("f")]

def test_drain_defers_while_chat_busy(monkeypatch) -> None:
    monkeypatch.setattr(wake_queue, "chat_is_busy", lambda *a: True)
    wake_queue.enqueue(
        {
            "task_id": _task("g"),
            "text": "done",
            "session_id": "s",
            "chat_id": "chat-1",
            "base_url": "http://127.0.0.1:1",
        }
    )

    sent = []
    before = time.time()
    stats = asyncio.run(
        wake_queue.drain_once(lambda *a, **k: sent.append(a))
    )

    assert stats == {"sent": 0, "failed": 0, "dropped": 0, "busy": 1}
    assert sent == []
    record = wake_queue.load()[0]
    assert record["attempts"] == 0          # busy is not a failure
    assert record["busy_retries"] == 1
    assert record["next_at"] >= before + wake_queue.BUSY_RETRY_S


def test_chat_is_busy_probe_failure_falls_through(monkeypatch) -> None:
    # No base_url -> never considered busy (POST keeps its 409 guard).
    assert wake_queue.chat_is_busy("", "a", "c") is False
    assert wake_queue.chat_is_busy("http://127.0.0.1:1", "a", "") is False


class _StatusHandler(http.server.BaseHTTPRequestHandler):
    """Real local /status endpoint; ``status`` is flipped by the test."""

    status = "running"

    def do_GET(self) -> None:  # noqa: N802 (http.server API)
        body = json.dumps({"status": self.status}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args) -> None:
        pass


@pytest.mark.parametrize("prior_busy", [20, 25])
def test_busy_wake_is_retained_then_delivered_when_idle(prior_busy) -> None:
    """The maintainer's sequence: a record that has been busy for six
    minutes (20/25 busy retries under the old cap) must survive, not be
    dropped, and land exactly once when the chat goes idle."""
    handler = _StatusHandler
    handler.status = "running"
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    base_url = f"http://127.0.0.1:{server.server_address[1]}"
    try:
        task_id = _task("p")
        wake_queue.enqueue(
            {
                "task_id": task_id,
                "text": "done",
                "session_id": "s",
                "chat_id": "c-1",
                "agent_id": "AgentTeamGuide",
                "base_url": base_url,
            }
        )
        # Six minutes of ordinary chat activity: the record already carries
        # the old cap (or more) of busy retries, which used to drop it here.
        records = wake_queue.load()
        records[0]["created_at"] = time.time() - 6 * 60
        records[0]["busy_retries"] = prior_busy
        records[0]["next_at"] = time.time()
        wake_queue._save(records)

        injected = []
        stats = asyncio.run(wake_queue.drain_once(lambda *a, **k: injected.append(a)))

        assert stats["busy"] >= 1
        assert stats["sent"] == 0 and stats["dropped"] == 0 and stats["failed"] == 0
        assert injected == []
        kept = wake_queue.load()
        assert len(kept) == 1
        assert kept[0]["task_id"] == task_id
        assert kept[0]["busy_retries"] == prior_busy + 1
        assert kept[0].get("attempts", 0) == 0

        # The chat goes idle: make it due again — delivered exactly once.
        handler.status = "idle"
        records = wake_queue.load()
        records[0]["next_at"] = 0.0
        wake_queue._save(records)

        stats = asyncio.run(wake_queue.drain_once(lambda *a, **k: injected.append(a)))
        assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}
        assert wake_queue.load() == []
        assert len(injected) == 1
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_409_storm_keeps_record_until_success() -> None:
    """Thirty straight 409s must never drop the record; once the send
    succeeds it is delivered exactly once (a single injection)."""
    state = {"n": 0}
    injected = []

    def sender(*args, **kwargs):
        state["n"] += 1
        if state["n"] <= 30:
            raise RuntimeError("wake inject failed: http 409")
        injected.append(args)

    wake_queue.enqueue({"task_id": _task("k"), "text": "done", "session_id": "s"})
    for _ in range(30):
        stats = asyncio.run(wake_queue.drain_once(sender))
        assert stats == {"sent": 0, "failed": 0, "dropped": 0, "busy": 1}
        record = wake_queue.load()[0]
        assert record.get("attempts", 0) == 0  # 409 is not a failure
        record["next_at"] = 0.0
        wake_queue._save([record])
    assert wake_queue.load()[0]["busy_retries"] == 30

    stats = asyncio.run(wake_queue.drain_once(sender))
    assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}
    assert wake_queue.load() == []
    assert len(injected) == 1


def test_busy_record_survives_restart(monkeypatch) -> None:
    """Only the JSON survives a process restart; a fresh module must still
    deliver the busy record once the chat is idle."""
    wake_queue.enqueue(
        {
            "task_id": _task("r"),
            "text": "done",
            "session_id": "s",
            "chat_id": "c-9",
            "base_url": "http://127.0.0.1:1",
        }
    )
    records = wake_queue.load()
    records[0]["busy_retries"] = 25
    records[0]["next_at"] = time.time()  # due right after the restart
    wake_queue._save(records)

    # Simulate a new process: every in-memory global resets to its
    # import-time value; the queue file on disk is the only surviving state.
    wake_queue.stop()
    monkeypatch.setattr(wake_queue, "_lock", None)
    monkeypatch.setattr(wake_queue, "_loop_task", None)
    monkeypatch.setattr(wake_queue, "_sender", None)
    monkeypatch.setattr(wake_queue, "_on_delivered", None)
    monkeypatch.setattr(wake_queue, "_is_delivered", None)
    monkeypatch.setattr(wake_queue, "chat_is_busy", lambda *a: False)

    injected = []
    stats = asyncio.run(wake_queue.drain_once(lambda *a, **k: injected.append(a)))
    assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}
    assert wake_queue.load() == []
    assert len(injected) == 1


def test_sent_state_suppresses_duplicates() -> None:
    """Duplicates are the durable sent state's job: a record whose task
    already reports 'sent' is dropped without a send, and a second drain
    after a successful send injects nothing."""
    injected = []
    wake_queue.enqueue({"task_id": _task("u"), "text": "done", "session_id": "s"})

    stats = asyncio.run(
        wake_queue.drain_once(
            lambda *a, **k: injected.append(a), is_delivered=lambda tid: False
        )
    )
    assert stats["sent"] == 1
    assert len(injected) == 1

    # Crash between the send and the durable removal: the record is back on
    # disk and the task file now reports wake_status == "sent".
    wake_queue.enqueue({"task_id": _task("u"), "text": "done", "session_id": "s"})
    stats = asyncio.run(
        wake_queue.drain_once(
            lambda *a, **k: injected.append(a), is_delivered=lambda tid: True
        )
    )
    assert stats == {"sent": 0, "failed": 0, "dropped": 1, "busy": 0}
    assert wake_queue.load() == []
    assert len(injected) == 1


def test_contract_bounds_still_enforced(monkeypatch) -> None:
    """Busy waiting is bounded only by the 24h contract; the two hard
    bounds still drop, and busy/409 never increments attempts."""
    monkeypatch.setattr(wake_queue, "chat_is_busy", lambda *a: True)

    # Age > 24h drops even while the chat is busy.
    wake_queue.enqueue(
        {
            "task_id": _task("o"),
            "text": "t",
            "session_id": "s",
            "chat_id": "c",
            "base_url": "http://127.0.0.1:1",
        }
    )
    records = wake_queue.load()
    records[0]["created_at"] = time.time() - wake_queue.MAX_AGE_S - 60
    wake_queue._save(records)
    stats = asyncio.run(wake_queue.drain_once(lambda *a, **k: None))
    assert stats == {"sent": 0, "failed": 0, "dropped": 1, "busy": 0}
    assert wake_queue.load() == []

    # attempts >= MAX_ATTEMPTS (real send failures) drops even while busy.
    wake_queue.enqueue(
        {
            "task_id": _task("h"),
            "text": "t",
            "session_id": "s",
            "chat_id": "c",
            "base_url": "http://127.0.0.1:1",
            "attempts": wake_queue.MAX_ATTEMPTS,
        }
    )
    stats = asyncio.run(wake_queue.drain_once(lambda *a, **k: None))
    assert stats == {"sent": 0, "failed": 0, "dropped": 1, "busy": 0}
    assert wake_queue.load() == []

    # Busy/409 keeps the record and never touches attempts.
    monkeypatch.setattr(wake_queue, "chat_is_busy", lambda *a: False)
    state = {"n": 0}
    injected = []

    def flaky(*args, **kwargs):
        state["n"] += 1
        if state["n"] <= 3:
            raise RuntimeError("wake inject failed: http 409")
        injected.append(args)

    wake_queue.enqueue(
        {
            "task_id": _task("n"),
            "text": "t",
            "session_id": "s",
            "chat_id": "c",
            "base_url": "http://127.0.0.1:1",
        }
    )
    for _ in range(3):
        stats = asyncio.run(wake_queue.drain_once(flaky))
        assert stats == {"sent": 0, "failed": 0, "dropped": 0, "busy": 1}
        record = wake_queue.load()[0]
        assert record.get("attempts", 0) == 0
        record["next_at"] = 0.0
        wake_queue._save([record])
    stats = asyncio.run(wake_queue.drain_once(flaky))
    assert stats == {"sent": 1, "failed": 0, "dropped": 0, "busy": 0}
    assert len(injected) == 1
