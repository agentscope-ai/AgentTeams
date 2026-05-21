"""HiClaw worker adapter API served beside the CoPaw app."""

from __future__ import annotations

import asyncio
from contextlib import suppress
import json
import logging
from typing import Any, Awaitable, Callable

logger = logging.getLogger(__name__)


class WorkerAPIServer:
    """Small HTTP server for HiClaw worker adapter endpoints."""

    def __init__(
        self,
        *,
        host: str,
        port: int,
        liveness_handler: Callable[[], Awaitable[dict[str, Any]]],
        readiness_handler: Callable[[], Awaitable[dict[str, Any]]],
    ) -> None:
        self.host = host
        self.port = port
        self._liveness_handler = liveness_handler
        self._readiness_handler = readiness_handler
        self._server: asyncio.AbstractServer | None = None

    async def start(self) -> None:
        if self._server is not None:
            return
        self._server = await asyncio.start_server(self._handle, self.host, self.port)
        logger.info("worker API server listening host=%s port=%s", self.host, self.port)

    @property
    def bound_port(self) -> int:
        if self._server is None or not self._server.sockets:
            return self.port
        return int(self._server.sockets[0].getsockname()[1])

    async def stop(self) -> None:
        if self._server is None:
            return
        self._server.close()
        await self._server.wait_closed()
        self._server = None
        logger.info("worker API server stopped host=%s port=%s", self.host, self.port)

    async def _handle(
        self,
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
    ) -> None:
        try:
            request_line = await reader.readline()
            method, path = _parse_request_line(request_line)
            while True:
                line = await reader.readline()
                if not line or line in (b"\r\n", b"\n"):
                    break

            if method == "GET" and path == "/worker/livez":
                payload = await self._liveness_handler()
                _write_json(writer, 200, payload)
            elif method == "GET" and path == "/worker/readyz":
                payload = await self._readiness_handler()
                status = 200 if payload.get("readiness") == "ready" else 503
                _write_json(writer, status, payload)
            else:
                _write_json(writer, 404, {"message": "not found"})
            await writer.drain()
        except Exception:
            logger.exception("worker API request failed")
            with suppress(Exception):
                _write_json(writer, 500, {"message": "internal server error"})
                await writer.drain()
        finally:
            writer.close()
            with suppress(Exception):
                await writer.wait_closed()


def _parse_request_line(request_line: bytes) -> tuple[str, str]:
    parts = request_line.decode("ascii", errors="replace").strip().split()
    if len(parts) < 2:
        return "", ""
    return parts[0], parts[1].split("?", 1)[0]


def _write_json(
    writer: asyncio.StreamWriter,
    status: int,
    payload: dict[str, Any],
) -> None:
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    reason = {
        200: "OK",
        404: "Not Found",
        500: "Error",
        503: "Service Unavailable",
    }.get(status, "Error")
    writer.write(
        b"".join(
            [
                f"HTTP/1.1 {status} {reason}\r\n".encode("ascii"),
                b"Content-Type: application/json\r\n",
                f"Content-Length: {len(body)}\r\n".encode("ascii"),
                b"Connection: close\r\n",
                b"\r\n",
                body,
            ]
        )
    )
