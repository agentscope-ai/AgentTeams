"""Worker heartbeat snapshot for the managed QwenPaw process."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from datetime import datetime, timezone
import json
import logging
import os
from pathlib import Path
import time
from typing import Any, Dict, Tuple
import urllib.error
import urllib.request

logger = logging.getLogger(__name__)


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _rfc3339_utc(value: datetime) -> str:
    return value.astimezone(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _parse_rfc3339(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip()
    if text.endswith("Z"):
        text = f"{text[:-1]}+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


@dataclass
class WorkerHeartbeat:
    path: Path
    status: str = "not_ready"
    message: str = "qwenpaw app not checked"
    details: Dict[str, Any] = field(default_factory=dict)

    def update(
        self,
        status: str,
        message: str = "",
        details: Dict[str, Any] | None = None,
    ) -> None:
        if status not in ("ready", "not_ready"):
            raise ValueError(f"invalid worker heartbeat status: {status}")
        self.status = status
        self.message = message
        self.details = details or {}
        self.persist()

    def snapshot(self) -> Dict[str, Any]:
        return {
            "status": self.status,
            "message": self.message,
            "details": self.details,
            "updatedAt": _now(),
        }

    def is_ready(self) -> bool:
        return self.status == "ready"

    def persist(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.path.write_text(json.dumps(self.snapshot(), indent=2, ensure_ascii=False), encoding="utf-8")


def check_qwenpaw_heartbeat(port: int) -> Tuple[str, str, Dict[str, Any]]:
    url = f"http://127.0.0.1:{port}/api/version"
    try:
        with urllib.request.urlopen(url, timeout=5) as response:
            body = response.read().decode("utf-8")
        return "ready", "qwenpaw API reachable", {"url": url, "body": body[:300]}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return "not_ready", str(exc), {"url": url}


def get_qwenpaw_agent_status(port: int, agent_id: str = "default") -> Dict[str, Any] | None:
    """Read QwenPaw's agent-status endpoint and return the raw payload.

    The payload carries the runtime's task-level truth (status,
    running_task_count, last_run_at, last_finish_at) — unlike Matrix
    typing presence it has no time cap, so a long-running task stays
    "running" for its whole duration.
    """

    url = f"http://127.0.0.1:{port}/api/agents/{agent_id}/agent-status"
    try:
        with urllib.request.urlopen(url, timeout=5) as response:
            payload = json.loads(response.read().decode("utf-8"))
    except Exception as exc:
        logger.debug("qwenpaw agent-status request failed component=heartbeat error_type=%s", type(exc).__name__)
        return None

    return payload if isinstance(payload, dict) else None


def derive_last_active_at(payload: Dict[str, Any] | None) -> str | None:
    """Map an agent-status payload to the controller lastActiveAt field."""
    if payload is None:
        return None
    if payload.get("status") == "running":
        return _rfc3339_utc(datetime.now(timezone.utc))

    candidates = [
        _parse_rfc3339(payload.get("last_finish_at")),
        _parse_rfc3339(payload.get("last_run_at")),
    ]
    latest = max((item for item in candidates if item is not None), default=None)
    return _rfc3339_utc(latest) if latest is not None else None


def get_qwenpaw_last_active_at(port: int, agent_id: str = "default") -> str | None:
    """Read QwenPaw's agent-status endpoint and map it to controller lastActiveAt."""
    return derive_last_active_at(get_qwenpaw_agent_status(port, agent_id))


def _agent_status_report_fields(payload: Dict[str, Any] | None) -> Dict[str, Any]:
    """Project the agent-status payload onto the heartbeat report body."""
    if not payload:
        return {}
    fields: Dict[str, Any] = {}
    for source, target in (
        ("status", "agentStatus"),
        ("running_task_count", "runningTaskCount"),
        ("last_run_at", "lastRunAt"),
        ("last_finish_at", "lastFinishAt"),
    ):
        value = payload.get(source)
        if value is not None:
            fields[target] = value
    return fields


@dataclass(frozen=True)
class ControllerHeartbeatReporter:
    """Report QwenPaw worker readiness and heartbeat to the AgentTeams controller."""

    worker_name: str
    controller_url: str = ""
    token: str = ""

    @classmethod
    def from_env(cls, worker_name: str) -> "ControllerHeartbeatReporter":
        return cls(
            worker_name=worker_name,
            controller_url=os.getenv("AGENTTEAMS_CONTROLLER_URL", "").rstrip("/"),
        )

    def enabled(self) -> bool:
        return bool(self.controller_url)

    def report_ready(
        self,
        last_active_at: str | None = None,
        agent_status: Dict[str, Any] | None = None,
    ) -> bool:
        return self._post("ready", last_active_at, agent_status)

    def report_heartbeat(
        self,
        last_active_at: str | None = None,
        agent_status: Dict[str, Any] | None = None,
    ) -> bool:
        # The controller uses repeated ready reports as worker heartbeats.
        return self._post("ready", last_active_at, agent_status)

    def _post(
        self,
        action: str,
        last_active_at: str | None,
        agent_status: Dict[str, Any] | None = None,
    ) -> bool:
        if not self.enabled():
            return False
        path = f"/api/v1/workers/{self.worker_name}/{action}"
        body = None
        headers = {}
        if last_active_at or agent_status:
            data: Dict[str, Any] = {}
            if last_active_at:
                data["lastActiveAt"] = last_active_at
            data.update(_agent_status_report_fields(agent_status))
            body = json.dumps(data).encode("utf-8")
            headers["Content-Type"] = "application/json"
        token = self.token or _discover_auth_token()
        if token:
            headers["Authorization"] = f"Bearer {token}"
        try:
            request = urllib.request.Request(
                self.controller_url + path,
                data=body,
                headers=headers,
                method="POST",
            )
            with urllib.request.urlopen(request, timeout=10) as response:
                if response.status < 200 or response.status >= 300:
                    logger.warning(
                        "controller report returned HTTP status component=heartbeat action=%s worker=%s http_status=%s",
                        action,
                        self.worker_name,
                        response.status,
                    )
                    return False
            return True
        except Exception as exc:
            logger.warning(
                "controller report failed component=heartbeat action=%s worker=%s error_type=%s",
                action,
                self.worker_name,
                type(exc).__name__,
            )
            return False


async def run_worker_heartbeat_loop(
    heartbeat: WorkerHeartbeat,
    *,
    worker_name: str,
    port: int,
    local_interval: float = 5,
    report_interval: float | None = None,
    reporter: ControllerHeartbeatReporter | None = None,
) -> None:
    """Probe QwenPaw locally and report worker status to the controller."""

    reporter = reporter or ControllerHeartbeatReporter.from_env(worker_name)
    report_every = report_interval if report_interval is not None else float(
        os.getenv("AGENTTEAMS_WORKER_HEARTBEAT_INTERVAL", "60")
    )
    ready_reported = False
    next_report_at = 0.0
    last_status: tuple[str, str] | None = None
    last_agent_status_key: tuple[str, int | None] | None = None

    logger.info(
        "qwenpaw heartbeat loop started component=heartbeat worker=%s port=%s local_interval_seconds=%s "
        "report_interval_seconds=%s reporter_enabled=%s",
        worker_name,
        port,
        local_interval,
        report_every,
        reporter.enabled(),
    )

    try:
        while True:
            status, message, details = await asyncio.to_thread(check_qwenpaw_heartbeat, port)
            heartbeat.update(status, message, details)
            status_key = (status, message)
            if status_key != last_status:
                logger.info(
                    "qwenpaw heartbeat status changed component=heartbeat worker=%s status=%s message=%s",
                    worker_name,
                    status,
                    message,
                )
                last_status = status_key

            if status == "ready" and reporter.enabled():
                agent_status = await asyncio.to_thread(get_qwenpaw_agent_status, port)
                last_active_at = derive_last_active_at(agent_status)
                # Change-driven report: the status light must flip within
                # one local poll (5s), not wait for the 60s heartbeat.
                agent_status_key = (
                    (agent_status or {}).get("status"),
                    (agent_status or {}).get("running_task_count"),
                )
                if agent_status_key != last_agent_status_key:
                    last_agent_status_key = agent_status_key
                    logger.info(
                        "qwenpaw agent status changed component=heartbeat worker=%s agent_status=%s running_task_count=%s",
                        worker_name, agent_status_key[0], agent_status_key[1],
                    )
                    # The ready report (below, first tick) already carries
                    # the full payload — skip the redundant change report.
                    if ready_reported:
                        await asyncio.to_thread(reporter.report_heartbeat, last_active_at, agent_status)
                if not ready_reported:
                    ready_reported = await asyncio.to_thread(reporter.report_ready, last_active_at, agent_status)
                    if ready_reported:
                        logger.info("controller ready report accepted component=heartbeat worker=%s", worker_name)
                now = time.time()
                if now >= next_report_at:
                    reported = await asyncio.to_thread(reporter.report_heartbeat, last_active_at, agent_status)
                    if reported:
                        logger.debug("controller heartbeat report accepted component=heartbeat worker=%s", worker_name)
                    next_report_at = now + report_every

            await asyncio.sleep(local_interval)
    except asyncio.CancelledError:
        logger.info("qwenpaw heartbeat loop stopped component=heartbeat worker=%s", worker_name)
        raise


def _discover_auth_token() -> str:
    token = os.getenv("AGENTTEAMS_AUTH_TOKEN", "")
    if token:
        return token
    token_file = os.getenv("AGENTTEAMS_AUTH_TOKEN_FILE", "")
    if token_file:
        try:
            return Path(token_file).read_text(encoding="utf-8").strip()
        except OSError:
            return ""
    return ""
