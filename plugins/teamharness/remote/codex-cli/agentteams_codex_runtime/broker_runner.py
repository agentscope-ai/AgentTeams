"""Host-local Codex runner for Manager execution requests."""

from __future__ import annotations

import argparse
import json
import os
import signal
import sys
import time
from collections.abc import Sequence
from pathlib import Path
from typing import Any
from urllib import error, parse, request

from .app_server import CodexAppServer, CodexError
from .journal import SessionJournal
from .security import environment_secret_values

MANAGER_INSTRUCTIONS = """You are the AgentTeams Manager. Coordinate the team
rather than editing product source. Use TeamHarness projectflow, taskflow,
message, filesync, and artifact capabilities when available. Keep planning and
team state authoritative in AgentTeams. Never request workspace writes or
credentials. Return a concise current-room answer or BLOCKED report.
"""


class BrokerClient:
    def __init__(self, base_url: str, token: str, timeout: float = 30.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def _request(
        self, method: str, path: str, payload: dict[str, Any] | None = None
    ) -> Any:
        body = None if payload is None else json.dumps(payload).encode("utf-8")
        call = request.Request(
            self.base_url + path,
            data=body,
            method=method,
            headers={
                "Authorization": f"Bearer {self.token}",
                "Content-Type": "application/json",
            },
        )
        with request.urlopen(call, timeout=self.timeout) as response:
            return json.loads(response.read().decode("utf-8"))

    def lease(self) -> dict[str, Any] | None:
        payload = self._request("GET", "/teamharness/codex/executions/lease")
        if not payload.get("ok"):
            raise RuntimeError(str(payload.get("error") or "broker rejected lease"))
        execution = payload.get("execution")
        return execution if isinstance(execution, dict) else None

    def complete(
        self, execution_id: str, *, output: str = "", error_text: str = ""
    ) -> None:
        path = (
            "/teamharness/codex/executions/"
            + parse.quote(execution_id, safe="")
            + "/complete"
        )
        payload = self._request(
            "POST",
            path,
            {"output": output, "error": error_text},
        )
        if not payload.get("ok"):
            raise RuntimeError("broker rejected completion")


class ManagerRunner:
    def __init__(
        self,
        *,
        broker: BrokerClient,
        codex: CodexAppServer,
        workspace: Path,
        journal: SessionJournal,
    ) -> None:
        self.broker = broker
        self.codex = codex
        self.workspace = workspace.resolve()
        self.journal = journal
        self.stopped = False

    def run_once(self) -> bool:
        execution = self.broker.lease()
        if execution is None:
            return False
        execution_id = str(execution.get("executionId") or "")
        session_key = str(execution.get("sessionKey") or execution_id)
        prompt = str(execution.get("prompt") or "")
        try:
            result = self.codex.execute(
                prompt=prompt,
                workspace=self.workspace,
                prior_thread_id=self.journal.thread_for(session_key),
                developer_instructions=MANAGER_INSTRUCTIONS,
                on_thread_ready=lambda thread_id: self.journal.set_thread(
                    session_key,
                    thread_id,
                ),
                approval_policy="never",
                sandbox="read-only",
            )
            self.journal.set_thread(session_key, result.thread_id)
            self.broker.complete(execution_id, output=result.output)
        except (CodexError, OSError, RuntimeError) as exc:
            self.broker.complete(execution_id, error_text=str(exc)[:1000])
        return True

    def run_forever(self, poll_interval: float) -> None:
        while not self.stopped:
            try:
                handled = self.run_once()
            except (OSError, RuntimeError, error.URLError):
                handled = False
            if not handled:
                time.sleep(poll_interval)

    def stop(self, *_: object) -> None:
        self.stopped = True
        self.codex.close()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="agentteams-codex-manager-runner")
    parser.add_argument("--broker-url", required=True)
    parser.add_argument("--workspace", required=True, type=Path)
    parser.add_argument("--state-dir", type=Path)
    parser.add_argument("--codex-command", default="codex")
    parser.add_argument("--mcp-server", type=Path)
    parser.add_argument("--token-env", default="AGENTTEAMS_CODEX_BROKER_TOKEN")
    parser.add_argument("--handshake-timeout", type=float, default=90.0)
    parser.add_argument("--turn-timeout", type=float, default=1800.0)
    parser.add_argument("--poll-interval", type=float, default=1.0)
    parser.add_argument("--once", action="store_true")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    token = os.getenv(args.token_env, "").strip()
    if not token:
        print(f"ERROR: {args.token_env} is required", file=sys.stderr)
        return 1
    workspace = args.workspace.resolve()
    if not workspace.is_dir():
        print(f"ERROR: workspace is not a directory: {workspace}", file=sys.stderr)
        return 1
    state_dir = (
        args.state_dir or Path.home() / ".agentteams" / "codex-manager"
    ).resolve()
    runner = ManagerRunner(
        broker=BrokerClient(args.broker_url, token),
        codex=CodexAppServer(
            codex_command=args.codex_command,
            mcp_server=args.mcp_server.resolve() if args.mcp_server else None,
            enabled_mcp_tools=(
                "health",
                "message",
                "roomflow",
                "filesync",
                "artifact",
                "projectflow",
                "taskflow",
            ),
            handshake_timeout=args.handshake_timeout,
            turn_timeout=args.turn_timeout,
            secret_values=environment_secret_values(),
        ),
        workspace=workspace,
        journal=SessionJournal(state_dir),
    )
    signal.signal(signal.SIGINT, runner.stop)
    if hasattr(signal, "SIGTERM"):
        signal.signal(signal.SIGTERM, runner.stop)
    try:
        runner.run_once() if args.once else runner.run_forever(args.poll_interval)
    except (OSError, RuntimeError, error.URLError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    finally:
        runner.stop()
    return 0
