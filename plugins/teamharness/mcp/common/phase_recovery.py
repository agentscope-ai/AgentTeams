"""Common framework lifecycle RPC; never starts model reasoning on RPC retries."""
from __future__ import annotations

import asyncio
import json
import os
from pathlib import Path
import urllib.error
import urllib.parse
import urllib.request


def retryable_error(error: BaseException | None) -> bool:
    """Recognize transport failures even when a native callback wraps them."""
    while error is not None:
        if isinstance(error, urllib.error.HTTPError):
            return error.code == 429 or 500 <= error.code <= 599
        if isinstance(error, (urllib.error.URLError, TimeoutError, ConnectionError)):
            return True
        error = error.__cause__
    return False


async def notify(event_id: str, native_event_id: str, action: str, *, request=None, sleep=None) -> dict:
    """Repeat the same idempotent lifecycle fact until committed or superseded."""
    return await until_available(request or _request, event_id, native_event_id, action, sleep=sleep)


async def until_available(request, *args, sleep=None):
    """Wait for transport recovery without consuming a phase or invoking a model."""
    sleep = sleep or asyncio.sleep
    delay = 1
    while True:
        try:
            return await asyncio.to_thread(request, *args)
        except urllib.error.HTTPError as error:
            if error.code != 429 and not 500 <= error.code <= 599:
                raise
        except (urllib.error.URLError, TimeoutError, ConnectionError):
            pass
        await sleep(delay)
        delay = min(delay * 2, 30)


def _request(event_id: str, native_event_id: str, action: str) -> dict:
    if action not in {"begin", "retry"}:
        raise ValueError("unknown coordination lifecycle action")
    process = os.environ["AGENTTEAMS_PROCESS_ID"]
    token = Path(os.getenv("AGENTTEAMS_AUTH_TOKEN_FILE") or "/var/run/agentteams/cli-token").read_text().strip()
    url = os.environ["AGENTTEAMS_CONTROLLER_URL"].rstrip("/") + "/api/v1/coordination/events/" + urllib.parse.quote(event_id, safe="") + "/phase"
    body = {"action": action, "native_event_id": native_event_id, "process_id": process}
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST",
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json",
                 "X-AgentTeams-Process-ID": process})
    with urllib.request.urlopen(req, timeout=10) as response:
        result = json.load(response)
    if not isinstance(result, dict):
        raise ValueError("invalid coordination lifecycle response")
    return result
