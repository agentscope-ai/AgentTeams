#!/usr/bin/env python3
"""Minimal Matrix channel loop for an AgentTeams-managed qwen-code worker.

Same room loop, outbox contract, and delivery state as the deepseek-harness
reference bridge; the execution layer is the qwen-code serve HTTP API
(session create/load, prompt, status, permission vote, SSE event stream)
instead of a subprocess per task.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import signal
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from bridge_state import BridgeState
from matrix_channel import (
    MatrixClient,
    matrix_events,
    matrix_transaction_id,
    text_events,
    visible_matrix_user_ids,
)

STOP = threading.Event()


def required_env(name: str) -> str:
    value = os.getenv(name, "").strip()
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


def runtime_section(path: Path, section: str) -> dict[str, str]:
    """Read scalar values from one top-level controller-generated YAML section."""
    values: dict[str, str] = {}
    active = False
    for raw in path.read_text(encoding="utf-8").splitlines():
        if raw == f"{section}:":
            active = True
            continue
        if active and raw and not raw.startswith(" "):
            break
        if not active or not raw.startswith("  ") or raw.startswith("    "):
            continue
        key, separator, value = raw.strip().partition(":")
        if separator:
            values[key] = value.strip().strip("'\"")
    return values


def runtime_matrix_context(runtime_path: Path, worker_name: str, matrix_domain: str) -> tuple[str, dict[str, str]]:
    member = runtime_section(runtime_path, "member")
    team = runtime_section(runtime_path, "team")
    own_user_id = member.get("matrixUserId") or f"@{worker_name}:{matrix_domain}"
    room_types: dict[str, str] = {}
    if member.get("personalRoomId"):
        room_types[member["personalRoomId"]] = "personal"
    if team.get("teamRoomId"):
        room_types[team["teamRoomId"]] = "team"
    return own_user_id, room_types


def runtime_agent_user_ids(runtime_path: Path) -> set[str]:
    """Read agent Matrix IDs from the controller-generated team.members list."""
    agent_roles = {"team_leader", "team-leader", "teamleader", "leader", "worker", "remote", "remote-member"}
    members: list[dict[str, str]] = []
    current: dict[str, str] | None = None
    in_team = False
    members_indent: int | None = None
    item_indent: int | None = None

    for raw in runtime_path.read_text(encoding="utf-8").splitlines():
        stripped = raw.strip()
        indent = len(raw) - len(raw.lstrip(" "))
        if raw == "team:":
            in_team = True
            continue
        if in_team and stripped and indent == 0:
            break
        if not in_team:
            continue
        if members_indent is None:
            if stripped == "members:":
                members_indent = indent
            continue

        if stripped.startswith("- ") and indent >= members_indent:
            if current is not None:
                members.append(current)
            current = {}
            item_indent = indent
            field = stripped[2:]
        elif current is not None and item_indent is not None and indent > item_indent:
            field = stripped
        else:
            if current is not None:
                members.append(current)
            break

        key, separator, value = field.partition(":")
        if separator:
            current[key.strip()] = value.strip().strip("'\"")
    else:
        if current is not None:
            members.append(current)

    return {
        member["matrixUserId"]
        for member in members
        if member.get("role", "").strip().lower().replace("_", "-") in agent_roles
        and member.get("matrixUserId")
    }


def source_agent_reply_user_id(event: dict[str, object], agent_user_ids: set[str], own_user_id: str) -> str:
    sender = str(event.get("sender") or "").strip()
    if sender != own_user_id and sender in agent_user_ids and event.get("auto_source_mention") is not True:
        return sender
    return ""


def reply_mention_user_ids(
    event: dict[str, object],
    text: str,
    agent_user_ids: set[str],
    own_user_id: str,
) -> list[str]:
    """Target the source Agent, plus any known Agent visibly named in the reply."""
    allowed = agent_user_ids - {own_user_id}
    mentioned = set(visible_matrix_user_ids(text, allowed))
    source_agent = source_agent_reply_user_id(event, agent_user_ids, own_user_id)
    if source_agent:
        mentioned.add(source_agent)
    return sorted(mentioned)


def safe_filename(value: str) -> str:
    leaf = value.replace("\\", "/").rsplit("/", 1)[-1].strip()
    leaf = re.sub(r"[\x00-\x1f<>:\"|?*]", "_", leaf)
    if leaf in ("", ".", ".."):
        leaf = "attachment.bin"
    return leaf[:180]


def workspace_subdirectory(workspace: Path, name: str, *, create: bool = False) -> tuple[Path, Path]:
    workspace_root = workspace.resolve()
    directory_path = workspace_root / name
    if create:
        directory_path.mkdir(parents=True, exist_ok=True)
    directory = directory_path.resolve()
    if not directory.is_relative_to(workspace_root):
        raise RuntimeError(f"Workspace {name} escapes the Workspace through a symbolic link")
    return workspace_root, directory


def materialize_attachment(
    event: dict[str, str],
    client: MatrixClient,
    workspace: Path,
    max_bytes: int,
) -> tuple[str, Path]:
    data, downloaded_type = client.download_media(event["mxc_url"], max_bytes)
    room_key = hashlib.sha256(event["room_id"].encode("utf-8")).hexdigest()[:16]
    event_key = hashlib.sha256(event["event_id"].encode("utf-8")).hexdigest()[:16]
    workspace_root, inbox = workspace_subdirectory(workspace, "inbox", create=True)
    destination = inbox / room_key / event_key / safe_filename(event.get("filename") or event["body"])
    destination.parent.mkdir(parents=True, exist_ok=True)
    resolved = destination.resolve()
    if not resolved.is_relative_to(inbox) or not resolved.is_relative_to(workspace_root):
        raise RuntimeError("Matrix attachment path escapes the Workspace inbox")
    resolved.write_bytes(data)
    relative = resolved.relative_to(workspace_root).as_posix()
    mimetype = event.get("mimetype") or downloaded_type
    label = "图片" if event["kind"] == "image" else "文件"
    prompt = (
        f"Matrix 用户 {event['sender']} 发来一个{label}。"
        f"它已保存到 Workspace 相对路径 `{relative}`（类型 {mimetype}）。"
        "请读取这个文件并完成用户请求；需要回传的文件请写入 Workspace 的 `outbox/` 目录。"
    )
    return prompt, resolved


def snapshot_outbox(workspace: Path) -> dict[str, str]:
    outbox_path = workspace.resolve() / "outbox"
    if not outbox_path.exists():
        return {}
    _workspace_root, outbox = workspace_subdirectory(workspace, "outbox")
    snapshot: dict[str, str] = {}
    for path in sorted(outbox.rglob("*")):
        if not path.is_file() or path.is_symlink():
            continue
        relative = path.relative_to(outbox).as_posix()
        digest = hashlib.sha256()
        with path.open("rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
        snapshot[relative] = digest.hexdigest()
    return snapshot


def changed_outbox_files(workspace: Path, before: dict[str, str]) -> list[str]:
    after = snapshot_outbox(workspace)
    return sorted(relative for relative, digest in after.items() if before.get(relative) != digest)


def workspace_output_path(workspace: Path, relative: str) -> Path:
    workspace_root, outbox = workspace_subdirectory(workspace, "outbox")
    path = (outbox / Path(relative)).resolve()
    if not path.is_relative_to(outbox) or not path.is_relative_to(workspace_root):
        raise RuntimeError(f"Workspace output escapes outbox: {relative}")
    return path


def output_remote_path(worker_name: str, relative: str) -> str:
    prefix = required_env("AGENTTEAMS_STORAGE_PREFIX").rstrip("/")
    normalized = Path(relative).as_posix().lstrip("/")
    return f"{prefix}/agents/{worker_name}/workspace/outbox/{normalized}"


def sync_output_paths(worker_name: str, workspace: Path, relative_paths: list[str]) -> None:
    for relative in relative_paths:
        path = workspace_output_path(workspace, relative)
        if not path.is_file() or path.is_symlink():
            raise RuntimeError(f"Workspace output is unavailable: {relative}")
        mc("cp", str(path), output_remote_path(worker_name, relative))


def restore_output_paths(worker_name: str, workspace: Path, relative_paths: list[str]) -> None:
    for relative in relative_paths:
        path = workspace_output_path(workspace, relative)
        if path.is_file() and not path.is_symlink():
            continue
        path.parent.mkdir(parents=True, exist_ok=True)
        mc("cp", output_remote_path(worker_name, relative), str(path))
        if not path.is_file() or path.is_symlink():
            raise RuntimeError(f"Persisted Workspace output could not be restored: {relative}")


def send_output_paths(
    client: MatrixClient,
    room_id: str,
    event_id: str,
    workspace: Path,
    relative_paths: list[str],
    mentioned_user_ids: list[str] | None = None,
    auto_source_mention: bool = False,
) -> list[Path]:
    sent: list[Path] = []
    for relative in sorted(relative_paths):
        path = workspace_output_path(workspace, relative)
        if not path.is_file() or path.is_symlink():
            raise RuntimeError(f"Workspace output is unavailable: {relative}")
        client.send_file(
            room_id,
            path,
            event_id,
            f"{event_id}:file:{relative}",
            mentioned_user_ids,
            auto_source_mention,
        )
        sent.append(path)
    return sent


def send_workspace_outputs(
    client: MatrixClient,
    room_id: str,
    event_id: str,
    workspace: Path,
    before: dict[str, str],
    mentioned_user_ids: list[str] | None = None,
    auto_source_mention: bool = False,
) -> list[Path]:
    return send_output_paths(
        client,
        room_id,
        event_id,
        workspace,
        changed_outbox_files(workspace, before),
        mentioned_user_ids,
        auto_source_mention,
    )


def mc(*args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["mc", *args], check=check, text=True, capture_output=True)


def push_runtime_state(worker_name: str, state_path: Path) -> None:
    prefix = required_env("AGENTTEAMS_STORAGE_PREFIX").rstrip("/")
    mc("cp", str(state_path), f"{prefix}/agents/{worker_name}/runtime/{state_path.name}")


def refresh_runtime_config(worker_name: str, runtime_path: Path) -> bool:
    prefix = required_env("AGENTTEAMS_STORAGE_PREFIX").rstrip("/")
    remote = f"{prefix}/agents/{worker_name}/runtime/{runtime_path.name}"
    temporary = runtime_path.with_suffix(runtime_path.suffix + ".remote")
    temporary.unlink(missing_ok=True)
    mc("cp", remote, str(temporary))
    if not temporary.exists() or temporary.stat().st_size == 0:
        temporary.unlink(missing_ok=True)
        raise RuntimeError(f"controller runtime config is empty: {remote}")
    if runtime_path.exists() and temporary.read_bytes() == runtime_path.read_bytes():
        temporary.unlink()
        return False
    temporary.replace(runtime_path)
    return True


def sync_sessions(worker_name: str) -> None:
    qwen_home = Path(os.getenv("QWEN_HOME") or str(Path(required_env("HOME")) / ".qwen"))
    sessions = qwen_home / "sessions"
    if not sessions.exists():
        return
    prefix = required_env("AGENTTEAMS_STORAGE_PREFIX").rstrip("/")
    mc("mirror", f"{sessions}/", f"{prefix}/agents/{worker_name}/.qc/sessions/", "--overwrite")


class ServeClient:
    """Minimal client for the qwen-code serve HTTP API (loopback, token auth)."""

    def __init__(self, base: str, token: str) -> None:
        self.base = base.rstrip("/")
        self.token = token

    def _request(self, method: str, path: str, body: object | None = None, timeout: int = 60) -> tuple[int, bytes]:
        data = json.dumps(body).encode("utf-8") if body is not None else None
        request = urllib.request.Request(self.base + path, data=data, method=method)
        request.add_header("Authorization", f"Bearer {self.token}")
        request.add_header("X-Qwen-Client-Id", "agentteams-worker")
        if data is not None:
            request.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def health(self) -> bool:
        status, _ = self._request("GET", "/health", timeout=5)
        return status == 200

    def status(self, session_id: str) -> dict:
        status, body = self._request("GET", f"/session/{session_id}/status", timeout=15)
        if status != 200:
            return {}
        decoded = json.loads(body)
        return decoded if isinstance(decoded, dict) else {}

    def create_or_load(self, session_id: str, cwd: str) -> None:
        # Explicit UUID: creates a fresh session if unknown; if the daemon
        # already holds it (restart recovery), load the persisted state.
        status, _ = self._request(
            "POST",
            "/session",
            {"sessionId": session_id, "cwd": cwd},
            timeout=60,
        )
        if status in (200, 201):
            return
        status, _ = self._request("POST", f"/session/{session_id}/load", None, timeout=120)
        if status not in (200, 201, 204):
            raise RuntimeError(f"serve session {session_id[:8]} create/load failed (http {status})")

    def pending_permissions(self, session_id: str) -> list[dict]:
        summary = self.status(session_id)
        pending = summary.get("pendingInteractions")
        if not isinstance(pending, list):
            return []
        return [item for item in pending if isinstance(item, dict)]

    def vote(self, session_id: str, request_id: str, option_id: str) -> bool:
        status, _ = self._request(
            "POST",
            f"/session/{session_id}/permission/{request_id}",
            {"outcome": {"outcome": "selected", "optionId": option_id}},
            timeout=15,
        )
        return status == 200  # 404 = already settled by another voter

    def prompt(self, session_id: str, text: str) -> None:
        status, _ = self._request(
            "POST",
            f"/session/{session_id}/prompt",
            {"prompt": [{"type": "text", "text": text}]},
            timeout=60,
        )
        if status not in (200, 201, 202):
            raise RuntimeError(f"serve prompt rejected (http {status})")

    def collect_answer(
        self,
        session_id: str,
        workspace: Path,
        timeout_seconds: int,
        *,
        stop: threading.Event,
    ) -> str:
        """Follow one turn: attach the SSE event stream, stream the final
        assistant message, and resolve when the session is no longer active.

        Permission prompts encountered mid-turn are answered with the first
        offered option (the worker's delegation contract is to proceed;
        human veto happens at the Matrix room level via the leader).
        """
        deadline = time.time() + timeout_seconds
        chunks: list[str] = []
        last_error: str = ""
        while not stop.is_set():
            if time.time() > deadline:
                raise TimeoutError(f"qwen-code turn exceeded {timeout_seconds}s")
            summary = self.status(session_id)
            for item in self.pending_permissions(session_id):
                request_id = str(item.get("requestId") or item.get("id") or "")
                options = item.get("options")
                if not isinstance(options, list) or not options:
                    options = []
                first = next(
                    (str(opt.get("optionId") or opt.get("id") or "") for opt in options if isinstance(opt, dict)),
                    "",
                )
                if request_id and first:
                    self.vote(session_id, request_id, first)
                    break
            if bool(summary.get("hasTurnError")):
                last_error = str(summary.get("turnError") or "turn error")
                break
            if not bool(summary.get("hasActivePrompt")):
                break
            # Drain the event stream while the turn is in flight; reconnect
            # transparently (SSE drops mid-turn are expected on long tasks).
            self._drain_events(session_id, chunks, budget_seconds=5)
            stop.wait(1)
        answer = "".join(chunks).strip()
        if not answer:
            if last_error:
                raise RuntimeError(f"qwen-code turn failed: {last_error}")
            raise RuntimeError("qwen-code returned an empty answer")
        return answer

    def _drain_events(self, session_id: str, chunks: list[str], budget_seconds: int) -> None:
        request = urllib.request.Request(self.base + f"/session/{session_id}/events")
        request.add_header("Authorization", f"Bearer {self.token}")
        request.add_header("Accept", "text/event-stream")
        deadline = time.time() + budget_seconds
        try:
            with urllib.request.urlopen(request, timeout=max(2, budget_seconds)) as stream:
                for raw in stream:
                    if time.time() >= deadline:
                        return
                    line = raw.decode("utf-8", errors="replace").strip()
                    if not line.startswith("data:"):
                        continue
                    try:
                        decoded = json.loads(line[len("data:"):].strip())
                    except json.JSONDecodeError:
                        continue
                    data = decoded.get("data") if isinstance(decoded, dict) else None
                    update = data.get("update") if isinstance(data, dict) else None
                    if not isinstance(update, dict) or update.get("sessionUpdate") != "agent_message_chunk":
                        continue
                    content = update.get("content")
                    if isinstance(content, dict) and isinstance(content.get("text"), str):
                        chunks.append(content["text"])
        except (urllib.error.URLError, TimeoutError, OSError):
            return  # reconnect on next drain; completion is decided by status


def run_serve(
    task: str,
    workspace: Path,
    timeout_seconds: int,
    *,
    session_id: str,
    resume: bool,
    client: ServeClient,
    stop: threading.Event,
) -> str:
    client.create_or_load(session_id, str(workspace))
    client.prompt(session_id, task)
    return client.collect_answer(session_id, workspace, timeout_seconds, stop=stop)


class HealthHandler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        if self.path != "/healthz":
            self.send_response(404)
            self.end_headers()
            return
        body = b'{"ok":true,"runtime":"qwen-code"}\n'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format: str, *_args: object) -> None:
        return


def serve_health() -> None:
    # qwen serve itself owns the worker console port (8088); the bridge
    # health probe lives on a separate port.
    port = int(os.getenv("AGENTTEAMS_BRIDGE_HEALTH_PORT", "8089"))
    server = ThreadingHTTPServer(("0.0.0.0", port), HealthHandler)
    server.timeout = 1
    while not STOP.is_set():
        server.handle_request()
    server.server_close()


def main() -> int:
    worker_name = required_env("AGENTTEAMS_WORKER_NAME")
    homeserver = required_env("AGENTTEAMS_MATRIX_URL")
    token = required_env("AGENTTEAMS_WORKER_MATRIX_TOKEN")
    runtime_path = Path(required_env("TEAMHARNESS_RUNTIME_CONFIG"))
    workspace = Path(required_env("TEAMHARNESS_WORKSPACE"))
    workspace.mkdir(parents=True, exist_ok=True)
    matrix_domain = required_env("AGENTTEAMS_MATRIX_DOMAIN")
    own_user_id, room_types = runtime_matrix_context(runtime_path, worker_name, matrix_domain)
    agent_user_ids = runtime_agent_user_ids(runtime_path)
    if not room_types:
        raise RuntimeError("runtime.yaml contains no personalRoomId or teamRoomId")

    state_path = runtime_path.parent / "matrix-bridge-state.json"
    state = BridgeState.load(state_path)
    legacy_cursor_path = runtime_path.parent / "matrix-next-batch"
    if state.next_batch is None and legacy_cursor_path.exists():
        state.next_batch = legacy_cursor_path.read_text(encoding="utf-8").strip()
    since = state.next_batch
    client = MatrixClient(homeserver, token)
    threading.Thread(target=serve_health, name="health", daemon=True).start()

    if not since:
        initial = client.sync(None, 0)
        since = str(initial.get("next_batch") or "")
        if not since:
            raise RuntimeError("initial Matrix sync returned no next_batch")
        state.next_batch = since
        state.save()
        push_runtime_state(worker_name, state_path)

    timeout_seconds = int(os.getenv("AGENTTEAMS_QWEN_TASK_TIMEOUT_SECONDS", "3600"))
    serve_base = os.getenv("AGENTTEAMS_QWEN_SERVE_BASE", "http://127.0.0.1:8088")
    serve_token = os.getenv("AGENTTEAMS_QWEN_SERVE_TOKEN", "agentteams-worker")
    serve = ServeClient(serve_base, serve_token)
    attachment_max_bytes = int(os.getenv("AGENTTEAMS_MATRIX_ATTACHMENT_MAX_BYTES", str(25 * 1024 * 1024)))
    max_attempts = max(1, int(os.getenv("AGENTTEAMS_MATRIX_EVENT_MAX_ATTEMPTS", "3")))
    retry_base_seconds = max(1, int(os.getenv("AGENTTEAMS_MATRIX_RETRY_BASE_SECONDS", "2")))
    print(f"[agentteams-dsh-worker] ready worker={worker_name} rooms={len(room_types)}", flush=True)
    while not STOP.is_set():
        try:
            if refresh_runtime_config(worker_name, runtime_path):
                own_user_id, room_types = runtime_matrix_context(runtime_path, worker_name, matrix_domain)
                agent_user_ids = runtime_agent_user_ids(runtime_path)
                if not room_types:
                    raise RuntimeError("updated runtime.yaml contains no personalRoomId or teamRoomId")
                print(
                    f"[agentteams-dsh-worker] runtime config refreshed rooms={len(room_types)}",
                    flush=True,
                )
            payload = client.sync(since, 30_000)
            batch_complete = True
            for event in matrix_events(payload, room_types, own_user_id, agent_user_ids):
                if state.is_completed(event["event_id"]):
                    continue
                session_id, resume = state.session_for(event["room_id"])
                attempts = state.begin_event(event["event_id"], event["room_id"])
                state.save()
                push_runtime_state(worker_name, state_path)
                try:
                    cached_answer = state.answer_for(event["event_id"])
                    if cached_answer is None:
                        task = event["body"]
                        if event["kind"] != "text":
                            task, _saved_path = materialize_attachment(event, client, workspace, attachment_max_bytes)
                        outbox_before = state.outbox_before_for(event["event_id"])
                        if outbox_before is None:
                            outbox_before = snapshot_outbox(workspace)
                            state.mark_outbox_before(event["event_id"], outbox_before)
                            state.save()
                            push_runtime_state(worker_name, state_path)
                        answer = run_serve(
                            task,
                            workspace,
                            timeout_seconds,
                            session_id=session_id,
                            resume=resume,
                            client=serve,
                            stop=STOP,
                        )
                        sync_sessions(worker_name)
                        state.mark_session_ready(event["room_id"])
                        output_paths = changed_outbox_files(workspace, outbox_before)
                        sync_output_paths(worker_name, workspace, output_paths)
                        state.mark_answer(event["event_id"], answer, output_paths)
                        state.save()
                        push_runtime_state(worker_name, state_path)
                    else:
                        answer, output_paths = cached_answer

                    restore_output_paths(worker_name, workspace, output_paths)
                    source_agent = source_agent_reply_user_id(event, agent_user_ids, own_user_id)
                    reply_mentions = reply_mention_user_ids(event, answer, agent_user_ids, own_user_id)
                    reply_event_id = client.send_text(
                        event["room_id"],
                        answer,
                        event["event_id"],
                        f"{event['event_id']}:reply",
                        reply_mentions,
                        bool(source_agent),
                    )
                    send_output_paths(
                        client,
                        event["room_id"],
                        event["event_id"],
                        workspace,
                        output_paths,
                        [source_agent] if source_agent else [],
                        bool(source_agent),
                    )
                    state.mark_completed(event["event_id"], reply_event_id)
                    state.save()
                    push_runtime_state(worker_name, state_path)
                except Exception as error:
                    state.mark_failure(event["event_id"], str(error))
                    state.save()
                    push_runtime_state(worker_name, state_path)
                    if state.should_retry(event["event_id"], max_attempts):
                        delay = min(30, retry_base_seconds * (2 ** (attempts - 1)))
                        print(
                            f"[agentteams-dsh-worker] event retry event={event['event_id']} "
                            f"attempt={attempts}/{max_attempts} delay={delay}s error={error}",
                            flush=True,
                        )
                        batch_complete = False
                        STOP.wait(delay)
                        break
                    failure = f"QwenCode 任务执行失败（已重试 {attempts} 次）：{error}"
                    source_agent = source_agent_reply_user_id(event, agent_user_ids, own_user_id)
                    failure_event_id = client.send_text(
                        event["room_id"],
                        failure,
                        event["event_id"],
                        f"{event['event_id']}:failure",
                        reply_mention_user_ids(event, failure, agent_user_ids, own_user_id),
                        bool(source_agent),
                    )
                    state.mark_completed(event["event_id"], failure_event_id)
                    state.save()
                    push_runtime_state(worker_name, state_path)
            if not batch_complete:
                continue
            next_batch = str(payload.get("next_batch") or "")
            if next_batch and next_batch != since:
                since = next_batch
                state.next_batch = since
                state.save()
                push_runtime_state(worker_name, state_path)
        except (
            urllib.error.URLError,
            TimeoutError,
            OSError,
            RuntimeError,
            json.JSONDecodeError,
            subprocess.SubprocessError,
        ) as error:
            print(f"[agentteams-dsh-worker] channel error: {error}", flush=True)
            STOP.wait(2)
    return 0


def stop(_signum: int, _frame: object) -> None:
    STOP.set()


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    raise SystemExit(main())
