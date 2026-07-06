import asyncio
import json

import pytest

from copaw_worker import worker_api
from copaw_worker.worker_api import WorkerAPIServer


class _FakeReader:
    def __init__(self, lines):
        self._lines = list(lines)

    async def readline(self):
        if self._lines:
            return self._lines.pop(0)
        return b""


class _StallingReader:
    """Simulates a slowloris client: readline() never completes."""

    async def readline(self):
        await asyncio.Event().wait()


class _FakeWriter:
    def __init__(self):
        self.body = bytearray()
        self.closed = False

    def write(self, data):
        self.body.extend(data)

    async def drain(self):
        return None

    def close(self):
        self.closed = True

    async def wait_closed(self):
        return None


@pytest.fixture
def anyio_backend():
    return "asyncio"


@pytest.mark.anyio
async def test_worker_api_serves_liveness():
    async def liveness_handler():
        return {
            "liveness": "alive",
            "message": "worker api alive",
        }

    async def readiness_handler():
        raise AssertionError("readiness handler should not run")

    server = WorkerAPIServer(
        host="127.0.0.1",
        port=0,
        liveness_handler=liveness_handler,
        readiness_handler=readiness_handler,
    )
    reader = _FakeReader(
        [
            b"GET /worker/livez HTTP/1.1\r\n",
            b"Host: 127.0.0.1\r\n",
            b"\r\n",
        ]
    )
    writer = _FakeWriter()

    await server._handle(reader, writer)

    raw = bytes(writer.body)
    headers, body = raw.split(b"\r\n\r\n", 1)
    assert b"HTTP/1.1 200 OK" in headers
    assert writer.closed is True
    assert json.loads(body) == {
        "liveness": "alive",
        "message": "worker api alive",
    }


@pytest.mark.anyio
async def test_worker_api_serves_readyz_with_200_when_ready():
    async def liveness_handler():
        raise AssertionError("liveness handler should not run")

    async def readiness_handler():
        return {
            "readiness": "ready",
            "healthiness": "healthy",
            "message": "worker ready",
            "components": {},
        }

    server = WorkerAPIServer(
        host="127.0.0.1",
        port=0,
        liveness_handler=liveness_handler,
        readiness_handler=readiness_handler,
    )
    reader = _FakeReader(
        [
            b"GET /worker/readyz HTTP/1.1\r\n",
            b"Host: 127.0.0.1\r\n",
            b"\r\n",
        ]
    )
    writer = _FakeWriter()

    await server._handle(reader, writer)

    raw = bytes(writer.body)
    headers, body = raw.split(b"\r\n\r\n", 1)
    assert b"HTTP/1.1 200 OK" in headers
    assert json.loads(body) == {
        "readiness": "ready",
        "healthiness": "healthy",
        "message": "worker ready",
        "components": {},
    }


@pytest.mark.anyio
async def test_worker_api_serves_readyz_with_503_when_not_ready():
    async def liveness_handler():
        raise AssertionError("liveness handler should not run")

    async def readiness_handler():
        return {
            "readiness": "not_ready",
            "healthiness": "unhealthy",
            "message": "model unhealthy",
            "components": {},
        }

    server = WorkerAPIServer(
        host="127.0.0.1",
        port=0,
        liveness_handler=liveness_handler,
        readiness_handler=readiness_handler,
    )
    reader = _FakeReader(
        [
            b"GET /worker/readyz HTTP/1.1\r\n",
            b"Host: 127.0.0.1\r\n",
            b"\r\n",
        ]
    )
    writer = _FakeWriter()

    await server._handle(reader, writer)

    raw = bytes(writer.body)
    headers, body = raw.split(b"\r\n\r\n", 1)
    assert b"HTTP/1.1 503 Service Unavailable" in headers
    assert json.loads(body) == {
        "readiness": "not_ready",
        "healthiness": "unhealthy",
        "message": "model unhealthy",
        "components": {},
    }


@pytest.mark.anyio
async def test_worker_api_returns_404_for_unknown_path():
    async def liveness_handler():
        raise AssertionError("liveness handler should not run")

    async def readiness_handler():
        raise AssertionError("readiness handler should not run")

    server = WorkerAPIServer(
        host="127.0.0.1",
        port=0,
        liveness_handler=liveness_handler,
        readiness_handler=readiness_handler,
    )
    reader = _FakeReader(
        [
            b"GET /nope HTTP/1.1\r\n",
            b"Host: 127.0.0.1\r\n",
            b"\r\n",
        ]
    )
    writer = _FakeWriter()

    await server._handle(reader, writer)

    raw = bytes(writer.body)
    headers, body = raw.split(b"\r\n\r\n", 1)
    assert b"HTTP/1.1 404 Not Found" in headers
    assert writer.closed is True
    assert json.loads(body) == {"message": "not found"}


@pytest.mark.anyio
async def test_worker_api_closes_connection_on_header_read_timeout(monkeypatch):
    """A stalled client (slowloris) must not hold the connection open forever:
    the header-read phase is bounded and the socket is closed on timeout."""
    monkeypatch.setattr(worker_api, "_HEADER_READ_TIMEOUT", 0.05)

    async def liveness_handler():
        raise AssertionError("liveness handler should not run")

    async def readiness_handler():
        raise AssertionError("readiness handler should not run")

    server = WorkerAPIServer(
        host="127.0.0.1",
        port=0,
        liveness_handler=liveness_handler,
        readiness_handler=readiness_handler,
    )
    reader = _StallingReader()
    writer = _FakeWriter()

    await asyncio.wait_for(server._handle(reader, writer), timeout=2)

    assert writer.closed is True
    assert bytes(writer.body) == b""
