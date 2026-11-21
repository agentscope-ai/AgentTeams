"""Lifecycle delivery retries never invoke the model or alter event identity."""
import asyncio
import importlib.util
from pathlib import Path
import urllib.error

import pytest

SOURCE = Path(__file__).resolve().parents[3] / "plugins/teamharness/mcp/common/phase_recovery.py"
spec = importlib.util.spec_from_file_location("phase_recovery_test", SOURCE)
recovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(recovery)


def test_transient_failures_repeat_exact_callback_without_model():
    calls, delays = [], []
    failures = [urllib.error.HTTPError("http://controller", 503, "down", {}, None),
                urllib.error.URLError("connection reset"),
                urllib.error.HTTPError("http://controller", 429, "busy", {}, None)]
    def request(*args):
        calls.append(args)
        if failures:
            raise failures.pop(0)
        return {"active": False, "retry_queued": True}
    async def sleep(delay):
        delays.append(delay)
    result = asyncio.run(recovery.notify("event", "$native", "retry", request=request, sleep=sleep))
    assert result["retry_queued"] is True
    assert calls == [("event", "$native", "retry")] * 4
    assert delays == [1, 2, 4]


@pytest.mark.parametrize("code", [400, 401, 403, 404, 409, 412])
def test_permanent_or_superseded_callback_never_retries(code):
    calls = []
    def request(*args):
        calls.append(args)
        raise urllib.error.HTTPError("http://controller", code, "rejected", {}, None)
    async def sleep(delay):
        pytest.fail("terminal callback response retried")
    with pytest.raises(urllib.error.HTTPError):
        asyncio.run(recovery.notify("event", "$native", "retry", request=request, sleep=sleep))
    assert len(calls) == 1


def test_cancellation_stops_only_the_rpc_wait():
    calls = []
    def request(*args):
        calls.append(args)
        raise urllib.error.URLError("down")
    async def sleep(delay):
        raise asyncio.CancelledError()
    with pytest.raises(asyncio.CancelledError):
        asyncio.run(recovery.notify("event", "$native", "begin", request=request, sleep=sleep))
    assert calls == [("event", "$native", "begin")]
