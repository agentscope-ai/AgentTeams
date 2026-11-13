"""Exact request delivery and runtime receipts against QwenPaw's approval service."""
import asyncio
import importlib.util
import json
from pathlib import Path
import sys
import time
from unittest.mock import AsyncMock, patch

import pytest

pytest.importorskip("nio")
pytest.importorskip("qwenpaw")
from qwenpaw.app.approvals.service import ApprovalService, PendingApproval
from qwenpaw.security.tool_guard.approval import ApprovalDecision

path = Path(__file__).resolve().parents[3] / "plugins/agentteams-matrix-channel/agentteams_matrix/channel.py"
spec = importlib.util.spec_from_file_location("approval_test_channel", path)
module = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = module
spec.loader.exec_module(module)


@pytest.mark.parametrize("decision,status", [
    (ApprovalDecision.APPROVED, "approved"),
    (ApprovalDecision.DENIED, "rejected"),
    (ApprovalDecision.TIMEOUT, "expired"),
])
def test_approval_request_and_service_receipt(decision, status):
    async def run():
        service = ApprovalService()
        loop = asyncio.get_running_loop()
        pending = PendingApproval(
            request_id="req-1", session_id="matrix:!room:test", root_session_id="matrix:!room:test",
            owner_agent_id="default", user_id="@human:test", channel="matrix", agent_id="default",
            tool_name="Bash", created_at=time.time(), future=loop.create_future(),
            result_summary="test rule", extra={"tool_call": {"input": {"command": "echo " + "x" * 1000}}},
        )
        service._pending[pending.request_id] = pending
        channel = module.AgentTeamsMatrixChannel.__new__(module.AgentTeamsMatrixChannel)
        channel.send = AsyncMock()
        with patch("qwenpaw.app.approvals.get_approval_service", return_value=service):
            await channel.send_approval_notification(
                session_id=pending.session_id, user_id=pending.user_id, request_id="req-1",
                tool_name="Bash", severity="HIGH", result_summary="Approval Required",
                channel_meta={"room_id": "!room:test"},
            )
        first = channel.send.await_args.args
        contract = first[2]["org.agentteams.approval"]
        assert contract["request_id"] == "req-1"
        assert len(json.loads(contract["parameters"])["command"]) > 1000
        assert contract["expires_at"] == int((pending.created_at + pending.timeout_seconds) * 1000)
        # Test the final Matrix serialization, not just the notification hook.
        serialized = channel._matrix_text_content("!room:test", first[1], first[2], "m.text")
        assert serialized["org.agentteams.approval"] == contract
        await service.resolve_request("req-1", decision)
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        assert channel.send.await_count == 2
        assert channel.send.await_args.args[2]["org.agentteams.approval"]["status"] == status
    asyncio.run(run())


def test_missing_approval_is_not_sent():
    async def run():
        channel = module.AgentTeamsMatrixChannel.__new__(module.AgentTeamsMatrixChannel)
        channel.send = AsyncMock()
        with patch("qwenpaw.app.approvals.get_approval_service", return_value=ApprovalService()):
            await channel.send_approval_notification(session_id="matrix:!room:test", user_id="@human:test", request_id="missing", tool_name="Bash", severity="HIGH", result_summary="old")
        channel.send.assert_not_awaited()
    asyncio.run(run())
