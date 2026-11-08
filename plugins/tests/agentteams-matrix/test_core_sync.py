"""SyncEngine tests: the sync loop moved out of the QwenPaw channel (M4b).

Drives ``SyncEngine.run()`` with a fake client that scripts ``sync()``
responses (recording the ``timeout`` / ``since`` / ``full_state``
arguments) and fires the currently registered event callbacks while a
scripted response is returned — the way nio's ``receive_response()``
fires room callbacks inside ``sync()``.  Pins the moved behaviour: the
token-less initial sync (``since=None, timeout=0``), the incremental
sync (previous ``next_batch``, configured long-poll timeout), the token
persistence, the catch-up callback-suppression window, the error
back-off and the stop semantics.
"""

from __future__ import annotations

import asyncio
import contextlib
import inspect
from typing import Any, List, Optional, Tuple

import pytest

pytest.importorskip("nio")

from nio import SyncResponse as _NioSyncResponse  # noqa: E402
from nio.responses import (  # noqa: E402
    DeviceList,
    DeviceOneTimeKeyCount,
    Rooms,
    SyncError,
)


def SyncResponse(next_batch=None, **_kwargs):
    """Build a real ``nio.SyncResponse`` with the current nio signature.

    The engine type-checks against ``nio.SyncResponse``; only the fields it
    reads (``next_batch``, ``rooms.invite``) carry values.
    """
    return _NioSyncResponse(
        next_batch=next_batch,
        rooms=Rooms(invite={}, join={}, leave={}),
        device_key_count=DeviceOneTimeKeyCount(curve25519=0, signed_curve25519=0),
        device_list=DeviceList(changed=[], left=[]),
        to_device_events=[],
        presence_events=[],
    )


from core.sync import SyncEngine  # noqa: E402

TIMEOUT_MS = 30000


class FakeClient:
    """matrix-nio ``AsyncClient`` stand-in for the sync engine.

    ``script`` entries are ``(response, [(room, event), ...])``: the
    events fire through the currently registered ``event_callbacks``
    when that scripted ``sync()`` call returns.  A callback list that
    was cleared for the duration of a sync therefore never sees the
    events of that sync (catch-up suppression).  Once the script is
    exhausted, a plain ``SyncResponse()`` is returned.
    """

    def __init__(self, script: Optional[List[Tuple[Any, List[Tuple[str, str]]]]] = None):
        self.script = list(script or [])
        self.event_callbacks: List[tuple] = []
        self.sync_calls: List[dict] = []
        self.joined: List[str] = []
        self.access_token = "tok-1"

    def add_event_callback(self, callback, *event_types):
        self.event_callbacks.append((callback, event_types))

    async def sync(self, timeout=30000, full_state=False, since=None, **_kw):
        self.sync_calls.append(
            {"timeout": timeout, "since": since, "full_state": full_state}
        )
        if self.script:
            resp, events = self.script.pop(0)
        else:
            # Long-poll semantics once the script is exhausted: block until
            # the engine task is cancelled, instead of spinning with instant
            # success responses (which would starve the polling helper).
            await asyncio.Event().wait()
        for room, event in events:
            for callback, event_types in self.event_callbacks:
                if event_types and not isinstance(event, event_types):
                    continue
                result = callback(room, event)
                if inspect.isawaitable(result):
                    await result
        # Yield to the event loop: the engine's incremental loop awaits
        # sync() repeatedly, and a coroutine that never yields would starve
        # the test's polling helper and make cancellation unreachable.
        await asyncio.sleep(0)
        return resp

    async def join(self, room_id):
        self.joined.append(room_id)


class FakeStore:
    """``SessionStore`` stand-in recording the sync-token traffic."""

    def __init__(self, token: Optional[str] = None):
        self.token = token
        self.saved: List[str] = []

    def load_sync_token(self):
        return self.token

    def save_sync_token(self, token):
        self.saved.append(token)
        self.token = token

    def load_session_key(self, room_id, sender):
        return None

    def save_session_key(self, room_id, sender, key):
        pass


def _run(scenario):
    """Run an async scenario to completion (no pytest-asyncio)."""
    return asyncio.run(scenario)


async def _wait_until(predicate, timeout=5.0):
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while not predicate():
        if loop.time() > deadline:
            raise TimeoutError("condition not met in time")
        await asyncio.sleep(0.01)


async def _close(task):
    """Collect the engine task after the scenario assertions."""
    if not task.done():
        task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task


def _invited_response(
    next_batch: str, room_id: str = "!invited:hs.example.com"
) -> SyncResponse:
    resp = SyncResponse(next_batch=next_batch)
    resp.rooms.invite[room_id] = None
    return resp


def test_initial_sync_without_token_then_incremental():
    """No persisted token: the first sync is ``since=None, timeout=0``
    (catch-up, full state); the next one resumes from the returned
    ``next_batch`` with the configured long-poll timeout and no full
    state; both tokens are persisted as the syncs return."""

    async def scenario():
        client = FakeClient(
            script=[
                (SyncResponse(next_batch="t1"), []),
                (SyncResponse(next_batch="t2"), []),
            ]
        )
        store = FakeStore(token=None)
        engine = SyncEngine(
            client=client,
            store=store,
            sync_timeout_ms=TIMEOUT_MS,
        )
        task = engine.start()
        try:
            await _wait_until(lambda: len(client.sync_calls) >= 2)
            first, second = client.sync_calls[0], client.sync_calls[1]
            assert first == {"timeout": 0, "since": None, "full_state": True}
            assert second == {
                "timeout": TIMEOUT_MS,
                "since": "t1",
                "full_state": False,
            }
            assert store.saved[:2] == ["t1", "t2"]
            await engine.stop()
        finally:
            await _close(task)

    _run(scenario())


def test_on_event_suppressed_during_initial_sync():
    """The catch-up sync clears the client's event callbacks: the
    ``on_event`` sink registered on the engine does not fire for events
    received during the initial sync, and fires once the incremental
    sync runs with the callbacks restored."""

    async def scenario():
        seen: List[Tuple[str, str]] = []

        async def on_event(room, event):
            seen.append((room, event))

        client = FakeClient(
            script=[
                (SyncResponse(next_batch="t1"), [("room-a", "ev-catchup")]),
                (SyncResponse(next_batch="t2"), [("room-b", "ev-incremental")]),
            ]
        )
        store = FakeStore(token=None)
        engine = SyncEngine(
            client=client,
            store=store,
            sync_timeout_ms=TIMEOUT_MS,
            on_event=on_event,
        )
        task = engine.start()
        try:
            await _wait_until(lambda: len(client.sync_calls) >= 2)
            assert seen == [("room-b", "ev-incremental")]
            await engine.stop()
        finally:
            await _close(task)

    _run(scenario())


def test_restored_token_full_state_then_incremental():
    """A persisted token skips the suppressed catch-up: the first sync
    resumes from the restored token with the long-poll timeout and full
    state, the next one is incremental."""

    async def scenario():
        client = FakeClient(
            script=[
                (SyncResponse(next_batch="t1"), []),
                (SyncResponse(next_batch="t2"), []),
            ]
        )
        store = FakeStore(token="t0")
        engine = SyncEngine(
            client=client,
            store=store,
            sync_timeout_ms=TIMEOUT_MS,
        )
        task = engine.start()
        try:
            await _wait_until(lambda: len(client.sync_calls) >= 2)
            first, second = client.sync_calls[0], client.sync_calls[1]
            assert first == {
                "timeout": TIMEOUT_MS,
                "since": "t0",
                "full_state": True,
            }
            assert second == {
                "timeout": TIMEOUT_MS,
                "since": "t1",
                "full_state": False,
            }
            assert store.saved == ["t1", "t2"]
            await engine.stop()
        finally:
            await _close(task)

    _run(scenario())


def test_invited_rooms_auto_joined():
    """Invited rooms are auto-joined both during the catch-up sync and
    in the incremental loop."""

    async def scenario():
        client = FakeClient(
            script=[
                (_invited_response("t1", "!inv-a:hs.example.com"), []),
                (_invited_response("t2", "!inv-b:hs.example.com"), []),
            ]
        )
        store = FakeStore(token=None)
        engine = SyncEngine(
            client=client,
            store=store,
            sync_timeout_ms=TIMEOUT_MS,
        )
        task = engine.start()
        try:
            await _wait_until(lambda: len(client.joined) == 2)
            assert client.joined == ["!inv-a:hs.example.com", "!inv-b:hs.example.com"]
            await engine.stop()
        finally:
            await _close(task)

    _run(scenario())


def test_invalid_token_stops_loop_and_clears():
    """A ``SyncError`` with ``M_UNKNOWN_TOKEN`` clears the client's
    access token, fires ``on_token_invalid``, and stops the loop on its
    own (no retry, no cancellation)."""

    async def scenario():
        invalidated: List[bool] = []
        client = FakeClient(
            script=[
                (SyncResponse(next_batch="t1"), []),
                (SyncError(status_code="M_UNKNOWN_TOKEN", message="unknown token"), []),
            ]
        )
        store = FakeStore(token=None)
        engine = SyncEngine(
            client=client,
            store=store,
            sync_timeout_ms=TIMEOUT_MS,
            on_token_invalid=lambda: invalidated.append(True),
        )
        task = engine.start()
        try:
            await _wait_until(task.done)
            assert client.access_token == ""
            assert invalidated == [True]
            # No retry sync after the token rejection.
            assert len(client.sync_calls) == 2
            assert task.cancelled() is False
            assert task.exception() is None
        finally:
            await _close(task)
        await engine.stop()

    _run(scenario())


def test_failed_sync_backs_off_five_seconds_and_retries(monkeypatch):
    """Any other failed sync logs, backs off 5 s and retries from the
    same token."""
    sleeps: List[float] = []
    real_sleep = asyncio.sleep

    async def fake_sleep(delay):
        sleeps.append(delay)
        # Keep yielding so the engine loop and the polling helper can run.
        await real_sleep(0)

    monkeypatch.setattr(asyncio, "sleep", fake_sleep)

    async def scenario():
        client = FakeClient(
            script=[
                (SyncResponse(next_batch="t1"), []),
                (SyncError(status_code=500, message="server error"), []),
                (SyncResponse(next_batch="t2"), []),
            ]
        )
        store = FakeStore(token=None)
        engine = SyncEngine(
            client=client,
            store=store,
            sync_timeout_ms=TIMEOUT_MS,
        )
        task = engine.start()
        try:
            await _wait_until(lambda: len(client.sync_calls) >= 3)
            first_failed = client.sync_calls[1]
            retry = client.sync_calls[2]
            assert first_failed == {
                "timeout": TIMEOUT_MS,
                "since": "t1",
                "full_state": False,
            }
            # The retry resumes from the same token.
            assert retry["since"] == "t1"
            assert retry["full_state"] is False
            # Exactly one 5 s back-off for the one failed sync (the
            # 0.01 s wait-poll entries come from this test's own
            # polling helper).
            assert sleeps.count(5) == 1
            await engine.stop()
        finally:
            await _close(task)

    _run(scenario())


def test_ready_and_maintenance_hook_ordering():
    """Pins the hook positions: ``on_maintenance`` after each successful
    sync (catch-up included), then ``on_ready`` between the initial
    phase and the incremental loop."""

    async def scenario():
        order: List[str] = []

        async def maintenance():
            order.append("maintenance")

        client = FakeClient(
            script=[
                (SyncResponse(next_batch="t1"), []),
                (SyncResponse(next_batch="t2"), []),
            ]
        )
        store = FakeStore(token=None)
        engine = SyncEngine(
            client=client,
            store=store,
            sync_timeout_ms=TIMEOUT_MS,
            on_ready=lambda: order.append("ready"),
            on_maintenance=maintenance,
        )
        task = engine.start()
        try:
            await _wait_until(
                lambda: order.count("maintenance") >= 2 and "ready" in order
            )
            assert order == ["maintenance", "ready", "maintenance"]
            await engine.stop()
        finally:
            await _close(task)

    _run(scenario())


def test_stop_exits_loop_and_is_idempotent():
    """``stop()`` cancels the running loop task; no sync runs after it,
    and a second ``stop()`` is a no-op."""

    async def scenario():
        client = FakeClient(
            script=[(SyncResponse(next_batch="t1"), [])]
        )
        store = FakeStore(token=None)
        engine = SyncEngine(
            client=client,
            store=store,
            sync_timeout_ms=TIMEOUT_MS,
        )
        task = engine.start()
        await _wait_until(lambda: len(client.sync_calls) >= 1)
        await engine.stop()
        assert task.cancelled() is True
        calls_after_stop = len(client.sync_calls)
        # Give a would-be next sync a chance to run — none may.
        await asyncio.sleep(0.05)
        assert len(client.sync_calls) == calls_after_stop
        await engine.stop()  # idempotent no-op

    _run(scenario())
