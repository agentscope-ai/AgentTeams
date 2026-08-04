"""Minimal Codex app-server v2 JSON-RPC client.

The implementation is based on the public Codex app-server protocol. It does
not copy implementation code from other clients.
"""

from __future__ import annotations

from dataclasses import dataclass
import json
import logging
import os
from pathlib import Path
import queue
import shutil
import subprocess
import sys
import threading
import time
from typing import Any, Callable, Iterable, Sequence

from .security import Redactor


LOG = logging.getLogger(__name__)


class CodexError(RuntimeError):
    """Base error for app-server startup, protocol, and turn failures."""


class CodexTimeout(CodexError):
    """Raised when an app-server request or turn exceeds its deadline."""


class CodexProtocolError(CodexError):
    """Raised for malformed or rejected JSON-RPC messages."""


class CodexPermissionDenied(CodexError):
    """Raised after Codex requests permissions outside the configured sandbox."""


@dataclass(frozen=True)
class ExecutionResult:
    thread_id: str
    turn_id: str
    status: str
    output: str
    approvals_denied: int


def resolve_codex_command(command: str) -> str:
    candidate = shutil.which(command)
    if candidate:
        return candidate
    path = Path(command).expanduser()
    if path.is_file():
        return str(path.resolve())
    raise CodexError(f"Codex command not found: {command}")


def _toml_string(value: str) -> str:
    return json.dumps(value, ensure_ascii=False)


def _object_id(value: Any, *container_names: str) -> str:
    current = value if isinstance(value, dict) else {}
    for container in container_names:
        nested = current.get(container)
        if isinstance(nested, dict):
            current = nested
            break
    for key in ("id", "threadId", "turnId"):
        result = current.get(key)
        if result:
            return str(result)
    return ""


def _agent_message_text(item: Any) -> str:
    if not isinstance(item, dict):
        return ""
    if str(item.get("type") or "") not in {"agentMessage", "agent_message"}:
        return ""
    for key in ("text", "message"):
        value = item.get(key)
        if isinstance(value, str):
            return value
    content = item.get("content")
    if isinstance(content, list):
        return "".join(
            str(part.get("text") or "")
            for part in content
            if isinstance(part, dict)
        )
    return ""


class CodexAppServer:
    def __init__(
        self,
        *,
        codex_command: str = "codex",
        mcp_server: Path | None = None,
        handshake_timeout: float = 90.0,
        turn_timeout: float = 1800.0,
        secret_values: Iterable[str] = (),
        launch_command: Sequence[str] | None = None,
    ) -> None:
        self.codex_command = codex_command
        self.mcp_server = mcp_server
        self.handshake_timeout = handshake_timeout
        self.turn_timeout = turn_timeout
        self.redactor = Redactor(secret_values)
        self.launch_command = list(launch_command) if launch_command else None
        self.process: subprocess.Popen[str] | None = None
        self._next_id = 1
        self._pending: dict[str, queue.Queue[dict[str, Any]]] = {}
        self._pending_lock = threading.Lock()
        self._write_lock = threading.Lock()
        self._events: queue.Queue[dict[str, Any]] = queue.Queue()
        self._stderr_tail: list[str] = []
        self._reader_thread: threading.Thread | None = None
        self._stderr_thread: threading.Thread | None = None
        self._closed = False
        self.approvals_denied = 0
        self.workspace_permissions_denied = 0

    def _command(self) -> list[str]:
        if self.launch_command:
            return list(self.launch_command)
        command = [resolve_codex_command(self.codex_command), "app-server", "--listen", "stdio://"]
        if self.mcp_server:
            command.extend(
                [
                    "-c",
                    f"mcp_servers.teamharness.command={_toml_string(sys.executable)}",
                    "-c",
                    "mcp_servers.teamharness.args=" + json.dumps([str(self.mcp_server)]),
                ]
            )
        return command

    def start(self) -> None:
        if self.process and self.process.poll() is None:
            return
        command = self._command()
        LOG.info("starting Codex app-server executable=%s", Path(command[0]).name)
        try:
            self.process = subprocess.Popen(
                command,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                encoding="utf-8",
                errors="replace",
                bufsize=1,
                env=dict(os.environ),
            )
        except OSError as exc:
            raise CodexError(f"failed to start Codex app-server: {exc}") from exc
        self._reader_thread = threading.Thread(target=self._read_stdout, daemon=True)
        self._stderr_thread = threading.Thread(target=self._read_stderr, daemon=True)
        self._reader_thread.start()
        self._stderr_thread.start()
        self.request(
            "initialize",
            {
                "clientInfo": {
                    "name": "agentteams-codex-worker",
                    "title": "AgentTeams Codex Worker",
                    "version": "0.1.0",
                },
                "capabilities": {"experimentalApi": False},
            },
            timeout=self.handshake_timeout,
        )
        self.notify("initialized", {})

    def _read_stdout(self) -> None:
        assert self.process and self.process.stdout
        for raw in self.process.stdout:
            line = raw.strip()
            if not line:
                continue
            try:
                message = json.loads(line)
            except json.JSONDecodeError:
                LOG.warning("ignored non-JSON app-server output")
                continue
            if not isinstance(message, dict):
                continue
            message_id = message.get("id")
            if message_id is not None and "method" in message:
                self._handle_server_request(message)
                continue
            if message_id is not None:
                with self._pending_lock:
                    waiter = self._pending.get(str(message_id))
                if waiter:
                    waiter.put(message)
                continue
            self._events.put(message)

    def _read_stderr(self) -> None:
        assert self.process and self.process.stderr
        for raw in self.process.stderr:
            safe = self.redactor.redact(raw.rstrip())
            if safe:
                self._stderr_tail.append(safe)
                del self._stderr_tail[:-40]
                LOG.debug("codex stderr: %s", safe)

    def _handle_server_request(self, message: dict[str, Any]) -> None:
        method = str(message.get("method") or "")
        if method in {
            "item/commandExecution/requestApproval",
            "execCommandApproval",
            "item/fileChange/requestApproval",
            "applyPatchApproval",
        }:
            result: dict[str, Any] = {"decision": "decline"}
            self.approvals_denied += 1
        elif method == "item/permissions/requestApproval":
            result = {"permissions": {}, "scope": "turn"}
            self.approvals_denied += 1
            self.workspace_permissions_denied += 1
        else:
            self._write(
                {
                    "id": message.get("id"),
                    "error": {"code": -32601, "message": f"unsupported client request: {method}"},
                }
            )
            return
        self._write({"id": message.get("id"), "result": result})

    def _write(self, message: dict[str, Any]) -> None:
        if not self.process or self.process.poll() is not None or not self.process.stdin:
            tail = " | ".join(self._stderr_tail[-3:])
            raise CodexError("Codex app-server is not running" + (f": {tail}" if tail else ""))
        payload = json.dumps(message, ensure_ascii=False, separators=(",", ":")) + "\n"
        with self._write_lock:
            self.process.stdin.write(payload)
            self.process.stdin.flush()

    def request(self, method: str, params: dict[str, Any], *, timeout: float | None = None) -> Any:
        timeout = self.handshake_timeout if timeout is None else timeout
        request_id = str(self._next_id)
        self._next_id += 1
        waiter: queue.Queue[dict[str, Any]] = queue.Queue(maxsize=1)
        with self._pending_lock:
            self._pending[request_id] = waiter
        try:
            self._write({"id": int(request_id), "method": method, "params": params})
            try:
                response = waiter.get(timeout=timeout)
            except queue.Empty as exc:
                raise CodexTimeout(f"Codex app-server {method} timed out after {timeout:g}s") from exc
        finally:
            with self._pending_lock:
                self._pending.pop(request_id, None)
        if "error" in response:
            error = response.get("error")
            detail = error.get("message") if isinstance(error, dict) else error
            raise CodexProtocolError(f"Codex app-server {method} failed: {self.redactor.redact(detail)}")
        return response.get("result")

    def notify(self, method: str, params: dict[str, Any]) -> None:
        self._write({"method": method, "params": params})

    def execute(
        self,
        *,
        prompt: str,
        workspace: Path,
        prior_thread_id: str = "",
        model: str = "",
        developer_instructions: str = "",
        on_thread_ready: Callable[[str], None] | None = None,
    ) -> ExecutionResult:
        self.start()
        self.workspace_permissions_denied = 0
        common: dict[str, Any] = {
            "cwd": str(workspace.resolve()),
            "approvalPolicy": "never",
            "sandbox": "workspace-write",
        }
        if model:
            common["model"] = model
        if developer_instructions:
            common["developerInstructions"] = developer_instructions

        thread_id = ""
        if prior_thread_id:
            try:
                result = self.request(
                    "thread/resume",
                    {"threadId": prior_thread_id, **common},
                    timeout=self.handshake_timeout,
                )
                thread_id = _object_id(result, "thread") or prior_thread_id
            except CodexProtocolError as exc:
                LOG.warning("unable to resume Codex thread; starting a new one: %s", exc)
        if not thread_id:
            result = self.request("thread/start", common, timeout=self.handshake_timeout)
            thread_id = _object_id(result, "thread")
        if not thread_id:
            raise CodexProtocolError("Codex app-server returned no thread id")
        if on_thread_ready:
            on_thread_ready(thread_id)

        turn_result = self.request(
            "turn/start",
            {
                "threadId": thread_id,
                "input": [{"type": "text", "text": prompt}],
            },
            timeout=self.handshake_timeout,
        )
        turn_id = _object_id(turn_result, "turn")
        if not turn_id:
            raise CodexProtocolError("Codex app-server returned no turn id")

        deadline = time.monotonic() + self.turn_timeout
        deltas: list[str] = []
        completed_text = ""
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                self.interrupt(thread_id, turn_id)
                raise CodexTimeout(f"Codex turn timed out after {self.turn_timeout:g}s")
            try:
                event = self._events.get(timeout=min(remaining, 1.0))
            except queue.Empty:
                if self.process and self.process.poll() is not None:
                    tail = " | ".join(self._stderr_tail[-3:])
                    raise CodexError("Codex app-server exited during turn" + (f": {tail}" if tail else ""))
                continue
            method = str(event.get("method") or "")
            params = event.get("params") if isinstance(event.get("params"), dict) else {}
            event_thread = str(params.get("threadId") or "")
            event_turn = str(params.get("turnId") or "")
            if event_thread and event_thread != thread_id:
                continue
            if event_turn and event_turn != turn_id:
                continue
            if method == "item/agentMessage/delta":
                deltas.append(str(params.get("delta") or ""))
            elif method == "item/completed":
                completed_text = _agent_message_text(params.get("item")) or completed_text
            elif method == "error":
                error = params.get("error")
                detail = error.get("message") if isinstance(error, dict) else error
                raise CodexError("Codex turn error: " + self.redactor.redact(detail or "unknown error"))
            elif method == "turn/completed":
                turn = params.get("turn") if isinstance(params.get("turn"), dict) else {}
                status = str(turn.get("status") or "completed")
                if self.workspace_permissions_denied:
                    raise CodexPermissionDenied(
                        "Codex requested permission outside the workspace-write sandbox"
                    )
                if status == "failed":
                    error = turn.get("error") if isinstance(turn.get("error"), dict) else {}
                    detail = self.redactor.redact(
                        error.get("message") or "unknown error"
                    )
                    raise CodexError("Codex turn failed: " + detail)
                output = "".join(deltas).strip() or completed_text.strip()
                return ExecutionResult(
                    thread_id=thread_id,
                    turn_id=turn_id,
                    status=status,
                    output=output,
                    approvals_denied=self.approvals_denied,
                )

    def interrupt(self, thread_id: str, turn_id: str) -> None:
        try:
            self.request(
                "turn/interrupt",
                {"threadId": thread_id, "turnId": turn_id},
                timeout=min(self.handshake_timeout, 10.0),
            )
        except CodexError:
            LOG.warning("failed to interrupt Codex turn")

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        process = self.process
        if not process or process.poll() is not None:
            return
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
        for stream in (process.stdin, process.stdout, process.stderr):
            if stream:
                stream.close()
        if self._reader_thread:
            self._reader_thread.join(timeout=1)
        if self._stderr_thread:
            self._stderr_thread.join(timeout=1)

    def __enter__(self) -> "CodexAppServer":
        self.start()
        return self

    def __exit__(self, *_: object) -> None:
        self.close()
