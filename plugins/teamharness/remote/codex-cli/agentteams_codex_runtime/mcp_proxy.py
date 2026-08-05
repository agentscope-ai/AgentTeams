"""Loopback-only MCP capability proxy that keeps service secrets out of Codex."""

from __future__ import annotations

import json
import logging
import os
import secrets
import socket
import socketserver
import subprocess
import sys
import threading
from pathlib import Path

try:
    from .security import Redactor
except ImportError:  # Executed directly as the Codex MCP stdio command.
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
    from agentteams_codex_runtime.security import Redactor


LOG = logging.getLogger(__name__)


class _ProxyServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


class _ProxyHandler(socketserver.StreamRequestHandler):
    def handle(self) -> None:
        proxy: "McpCapabilityProxy" = self.server.proxy  # type: ignore[attr-defined]
        authentication = self.rfile.readline(4096).decode("utf-8", errors="replace")
        try:
            received = str(json.loads(authentication).get("token") or "")
        except (json.JSONDecodeError, AttributeError):
            return
        if not secrets.compare_digest(received, proxy.token):
            return

        environment = dict(proxy.server_environment)
        environment["PYTHONUTF8"] = "1"
        environment["PYTHONIOENCODING"] = "utf-8"
        try:
            process = subprocess.Popen(
                [sys.executable, str(proxy.mcp_server)],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=environment,
            )
        except OSError as exc:
            LOG.error(
                "unable to start scoped MCP server: %s", proxy.redactor.redact(exc)
            )
            return

        assert process.stdin and process.stdout and process.stderr

        def socket_to_process() -> None:
            try:
                while chunk := self.rfile.readline():
                    process.stdin.write(chunk)
                    process.stdin.flush()
            except (OSError, BrokenPipeError):
                pass
            finally:
                try:
                    process.stdin.close()
                except OSError:
                    pass

        def process_to_socket() -> None:
            try:
                while chunk := process.stdout.readline():
                    self.wfile.write(chunk)
                    self.wfile.flush()
            except (OSError, BrokenPipeError):
                pass

        inbound = threading.Thread(target=socket_to_process, daemon=True)
        outbound = threading.Thread(target=process_to_socket, daemon=True)
        inbound.start()
        outbound.start()
        for raw in process.stderr:
            detail = proxy.redactor.redact(
                raw.decode("utf-8", errors="replace").rstrip()
            )
            if detail:
                LOG.debug("teamharness MCP stderr: %s", detail)
        process.wait()
        inbound.join(timeout=1)
        outbound.join(timeout=1)


class McpCapabilityProxy:
    """Expose one MCP server through an authenticated loopback socket."""

    def __init__(
        self,
        mcp_server: Path,
        *,
        server_environment: dict[str, str] | None = None,
        secret_values: tuple[str, ...] = (),
    ) -> None:
        self.mcp_server = mcp_server.resolve()
        self.server_environment = dict(
            os.environ if server_environment is None else server_environment
        )
        self.redactor = Redactor(secret_values)
        self.token = secrets.token_urlsafe(32)
        self.server: _ProxyServer | None = None
        self.thread: threading.Thread | None = None

    @property
    def address(self) -> tuple[str, int]:
        if self.server is None:
            raise RuntimeError("MCP capability proxy is not running")
        host, port = self.server.server_address
        return str(host), int(port)

    def start(self) -> None:
        if self.server is not None:
            return
        server = _ProxyServer(("127.0.0.1", 0), _ProxyHandler)
        server.proxy = self  # type: ignore[attr-defined]
        self.server = server
        self.thread = threading.Thread(target=server.serve_forever, daemon=True)
        self.thread.start()

    def close(self) -> None:
        if self.server is None:
            return
        self.server.shutdown()
        self.server.server_close()
        self.server = None
        if self.thread:
            self.thread.join(timeout=2)
            self.thread = None


def run_client(host: str, port: int, token: str) -> int:
    """Forward MCP stdio to the authenticated local capability socket."""

    connection = socket.create_connection((host, port), timeout=10)
    connection.settimeout(None)
    connection.sendall(
        (json.dumps({"token": token}, separators=(",", ":")) + "\n").encode("utf-8")
    )

    def stdin_to_socket() -> None:
        try:
            while chunk := sys.stdin.buffer.readline():
                connection.sendall(chunk)
        except (OSError, BrokenPipeError):
            pass
        finally:
            try:
                connection.shutdown(socket.SHUT_WR)
            except OSError:
                pass

    inbound = threading.Thread(target=stdin_to_socket, daemon=True)
    inbound.start()
    try:
        while chunk := connection.recv(65536):
            sys.stdout.buffer.write(chunk)
            sys.stdout.buffer.flush()
    except OSError:
        pass
    finally:
        connection.close()
    inbound.join(timeout=1)
    return 0


def main(argv: list[str] | None = None) -> int:
    arguments = sys.argv[1:] if argv is None else argv
    if len(arguments) != 3:
        print("usage: mcp_proxy.py HOST PORT TOKEN", file=sys.stderr)
        return 2
    return run_client(arguments[0], int(arguments[1]), arguments[2])


if __name__ == "__main__":
    raise SystemExit(main())
