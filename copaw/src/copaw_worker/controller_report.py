"""Report CoPaw worker readiness and heartbeat metrics to the controller."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path

from copaw_worker.llm_usage import get_llm_usage_tracker

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class ControllerReadyReporter:
    """POST /api/v1/workers/{name}/ready with optional LLM usage metrics."""

    worker_name: str
    controller_url: str = ""

    @classmethod
    def from_env(cls, worker_name: str) -> "ControllerReadyReporter":
        return cls(
            worker_name=worker_name,
            controller_url=os.getenv("AGENTTEAMS_CONTROLLER_URL", "").rstrip("/"),
        )

    def enabled(self) -> bool:
        return bool(self.controller_url)

    def report_ready(self, llm_calls_since_last_heartbeat: int | None = None) -> bool:
        body: dict[str, int] = {}
        if llm_calls_since_last_heartbeat is not None:
            body["llmCallsSinceLastHeartbeat"] = llm_calls_since_last_heartbeat
        return self._post(body or None)

    def _post(self, body: dict[str, int] | None) -> bool:
        if not self.enabled():
            return False

        path = f"/api/v1/workers/{self.worker_name}/ready"
        payload = None
        headers: dict[str, str] = {}
        if body is not None:
            payload = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"

        token = _discover_auth_token()
        if token:
            headers["Authorization"] = f"Bearer {token}"

        try:
            request = urllib.request.Request(
                self.controller_url + path,
                data=payload,
                headers=headers,
                method="POST",
            )
            with urllib.request.urlopen(request, timeout=10) as response:
                if response.status < 200 or response.status >= 300:
                    logger.warning(
                        "controller ready report returned HTTP %s for worker %s",
                        response.status,
                        self.worker_name,
                    )
                    return False
            return True
        except Exception as exc:
            logger.warning(
                "controller ready report failed for worker %s: %s",
                self.worker_name,
                exc,
            )
            return False


async def run_controller_ready_loop(
    *,
    worker_name: str,
    report_interval: float | None = None,
    reporter: ControllerReadyReporter | None = None,
) -> None:
    """Report ready once, then periodic heartbeats with LLM call counts."""

    reporter = reporter or ControllerReadyReporter.from_env(worker_name)
    if not reporter.enabled():
        logger.info("controller ready reporter disabled (AGENTTEAMS_CONTROLLER_URL unset)")
        return

    interval = report_interval
    if interval is None:
        interval = float(os.getenv("AGENTTEAMS_WORKER_HEARTBEAT_INTERVAL", "60"))

    tracker = get_llm_usage_tracker()
    next_report_at = 0.0
    ready_reported = False

    logger.info(
        "controller ready loop started worker=%s interval_seconds=%s",
        worker_name,
        interval,
    )

    try:
        while True:
            now = time.time()
            if not ready_reported or now >= next_report_at:
                pending = tracker.take_for_report()
                reported = await asyncio.to_thread(reporter.report_ready, pending)
                if reported:
                    if not ready_reported:
                        logger.info(
                            "controller ready report accepted worker=%s llm_calls=%s",
                            worker_name,
                            pending,
                        )
                        ready_reported = True
                    else:
                        logger.debug(
                            "controller heartbeat report accepted worker=%s llm_calls=%s",
                            worker_name,
                            pending,
                        )
                    next_report_at = time.time() + interval
                else:
                    tracker.restore_after_failed_report(pending)

            await asyncio.sleep(min(max(interval / 4, 5.0), 15.0))
    except asyncio.CancelledError:
        logger.info("controller ready loop stopped worker=%s", worker_name)
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
