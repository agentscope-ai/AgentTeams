"""TeamHarness remote-member bridge around Codex app-server."""

from __future__ import annotations

import hashlib
import json
import logging
import os
import tempfile
import threading
from pathlib import Path
from typing import Any

from agentteams_codex_runtime.journal import SessionJournal

from .codex_client import CodexAppServer, CodexError, ExecutionResult
from .config import RuntimeConfig
from .matrix import AssignedTask, MatrixClient, MatrixError
from .security import Redactor

LOG = logging.getLogger(__name__)


class StateStore:
    """Non-secret local cursor and task-to-thread mapping."""

    def __init__(self, directory: Path) -> None:
        self.directory = directory
        self.path = directory / "state.json"
        self.directory.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._state = self._load()
        self.sessions = SessionJournal(self.directory)
        legacy_threads = self._state.pop("threads", {})
        if isinstance(legacy_threads, dict):
            for task_id, thread_id in legacy_threads.items():
                if str(task_id) and str(thread_id) and not self.sessions.thread_for(str(task_id)):
                    self.sessions.set_thread(str(task_id), str(thread_id))
            if legacy_threads:
                self._save()

    def _load(self) -> dict[str, Any]:
        if not self.path.exists():
            return {"since": "", "threads": {}, "seenEvents": []}
        try:
            value = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return {"since": "", "threads": {}, "seenEvents": []}
        return value if isinstance(value, dict) else {"since": "", "threads": {}, "seenEvents": []}

    def _save(self) -> None:
        payload = json.dumps(self._state, indent=2, sort_keys=True) + "\n"
        with tempfile.NamedTemporaryFile(
            "w",
            encoding="utf-8",
            dir=self.directory,
            prefix="state-",
            suffix=".tmp",
            delete=False,
        ) as handle:
            handle.write(payload)
            temporary = Path(handle.name)
        os.replace(temporary, self.path)

    @property
    def since(self) -> str:
        return str(self._state.get("since") or "")

    def set_since(self, value: str) -> None:
        with self._lock:
            self._state["since"] = value
            self._save()

    def has_seen(self, event_id: str) -> bool:
        return event_id in self._state.get("seenEvents", [])

    def mark_seen(self, event_id: str) -> None:
        with self._lock:
            events = [str(item) for item in self._state.get("seenEvents", [])]
            if event_id not in events:
                events.append(event_id)
            self._state["seenEvents"] = events[-2048:]
            self._save()

    def thread_for(self, task_id: str) -> str:
        return self.sessions.thread_for(task_id)

    def set_thread(self, task_id: str, thread_id: str) -> None:
        self.sessions.set_thread(task_id, thread_id)


def load_developer_instructions(plugin_root: Path) -> str:
    paths = (
        plugin_root / "prompts" / "agent" / "remote-member.md",
        plugin_root / "prompts" / "team" / "TEAMS.md",
        plugin_root / "skills" / "team" / "task-execution" / "SKILL.md",
    )
    sections: list[str] = []
    for path in paths:
        if path.is_file():
            sections.append(path.read_text(encoding="utf-8"))
    if not sections:
        raise FileNotFoundError(f"TeamHarness prompt assets not found under {plugin_root}")
    return "\n\n---\n\n".join(sections)


class CodexWorkerBridge:
    def __init__(
        self,
        *,
        config: RuntimeConfig,
        workspace: Path,
        plugin_root: Path,
        matrix: MatrixClient,
        codex: CodexAppServer,
        state: StateStore,
        redactor: Redactor,
    ) -> None:
        self.config = config
        self.workspace = workspace.resolve()
        self.plugin_root = plugin_root.resolve()
        self.matrix = matrix
        self.codex = codex
        self.state = state
        self.redactor = redactor
        self.stop_event = threading.Event()
        self.developer_instructions = load_developer_instructions(self.plugin_root)

    def _prompt(self, task: AssignedTask) -> str:
        return (
            f"You received this current Matrix task event from {task.sender}:\n\n"
            f"{task.body}\n\n"
            f"The authoritative task id is {task.task_id}. Work only inside {self.workspace}. "
            "Follow the TeamHarness remote-member and task-execution contracts in your developer "
            "instructions. Acknowledge and submit through the TeamHarness MCP taskflow tools when "
            "the task exists. Your final answer must be a concise current-room completion or "
            "BLOCKED report and must not expose credentials, hidden reasoning, or raw tool output."
        )

    def _blocked_message(self, task: AssignedTask, error: object) -> str:
        leader = self.config.leader_matrix_user_id
        prefix = f"{leader} " if leader else ""
        detail = self.redactor.redact(error).replace("\n", " ")[:500]
        return f"{prefix}BLOCKED: {task.task_id} - Codex Worker: {detail}"

    def process_task(self, task: AssignedTask) -> ExecutionResult | None:
        if self.state.has_seen(task.event_id):
            return None
        prior_thread = self.state.thread_for(task.task_id)
        try:
            result = self.codex.execute(
                prompt=self._prompt(task),
                workspace=self.workspace,
                prior_thread_id=prior_thread,
                model=self.config.model,
                developer_instructions=self.developer_instructions,
                on_thread_ready=lambda thread_id: self.state.set_thread(
                    task.task_id,
                    thread_id,
                ),
            )
            self.state.set_thread(task.task_id, result.thread_id)
            message = result.output.strip()
            if not message:
                leader = self.config.leader_matrix_user_id
                prefix = f"{leader} " if leader else ""
                message = f"{prefix}TASK_COMPLETED: {task.task_id} - Codex turn completed."
            self.matrix.send_text(
                task.room_id,
                message[:20000],
                transaction_id=self._transaction_id(task.event_id, "result"),
            )
            self.state.mark_seen(task.event_id)
            return result
        except (CodexError, MatrixError, OSError) as exc:
            LOG.error("task %s failed: %s", task.task_id, self.redactor.redact(exc))
            try:
                self.matrix.send_text(
                    task.room_id,
                    self._blocked_message(task, exc),
                    transaction_id=self._transaction_id(task.event_id, "blocked"),
                )
                self.state.mark_seen(task.event_id)
            except MatrixError as send_error:
                LOG.error("unable to report blocker: %s", self.redactor.redact(send_error))
            return None

    @staticmethod
    def _transaction_id(event_id: str, suffix: str) -> str:
        digest = hashlib.sha256(event_id.encode("utf-8")).hexdigest()[:24]
        return f"codex-worker-{digest}-{suffix}"

    def sync_once(self, *, timeout_ms: int = 30000) -> int:
        response = self.matrix.sync(self.state.since, timeout_ms=timeout_ms)
        processed = 0
        for task in self.matrix.assigned_tasks(response):
            if self.state.has_seen(task.event_id):
                continue
            self.process_task(task)
            processed += 1
        next_batch = str(response.get("next_batch") or "")
        if next_batch:
            self.state.set_since(next_batch)
        return processed

    def run_forever(self, *, sync_timeout_ms: int = 30000) -> None:
        while not self.stop_event.is_set():
            try:
                self.sync_once(timeout_ms=sync_timeout_ms)
            except MatrixError as exc:
                LOG.warning("Matrix sync failed: %s", self.redactor.redact(exc))
                self.stop_event.wait(2.0)

    def stop(self) -> None:
        self.stop_event.set()
        self.codex.close()
