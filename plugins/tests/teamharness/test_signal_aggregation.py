# -*- coding: utf-8 -*-
"""TH-23 signal-mapping source_event extension and Manager by-code aggregation.

The contract's source_event vocabulary grows from 15 to 20 codes by
piggybacking five planned codes (task_delegated, task_acknowledged,
project_created, loop_planned, submission_conflict) onto existing rows
with #future: emitter labels — no new row, no new identity input, no
separate REASONS list. The Manager aggregates signal events BY CODE
only: free text is never aggregated, and a code outside the contract
vocabulary is rejected (reported, never silently accepted).
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest


MCP_DIR = Path(__file__).resolve().parents[2] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


CONTRACT = (
    Path(__file__).resolve().parents[2] / "teamharness" / "contracts" / "signal-mapping.json"
)


def _contract_codes() -> list[str]:
    contract = json.loads(CONTRACT.read_text(encoding="utf-8"))
    codes: list[str] = []
    for row in contract["events"]:
        for code in row["source_events"]:
            codes.append(code)
    return codes


class TestContractVocabulary:
    def test_twenty_codes_queryable(self) -> None:
        codes = server.signal_events._contract_source_codes()
        assert len(codes) == 20
        # The five TH-23 codes are in the vocabulary ...
        for code in (
            "task_delegated",
            "task_acknowledged",
            "project_created",
            "loop_planned",
            "submission_conflict",
        ):
            assert code in codes, code
        # ... on top of the 15 pre-existing codes (none lost):
        for code in (
            "task_submitted",
            "task_accepted",
            "project_completed",
            "project_paused",
            "project_resumed",
            "project_replanned",
            "task_cancelled",
            "loop_iteration_finished",
            "loop_review_verdict",
            "attention_raised",
            "attention_resolved",
            "attention_overdue",
            "mismatch_recorded",
            "contract_snapshot",
            "artifact_delivered",
        ):
            assert code in codes, code

    def test_row_count_and_identity_unchanged(self) -> None:
        contract = json.loads(CONTRACT.read_text(encoding="utf-8"))
        assert len(contract["events"]) == 14
        assert contract["version"] == "1.2"
        # The five extended codes live on existing rows, each exactly once:
        codes = _contract_codes()
        assert len(codes) == 20
        assert len(set(codes)) == 20

    def test_new_codes_carry_future_emitter_labels(self) -> None:
        contract = json.loads(CONTRACT.read_text(encoding="utf-8"))
        new_codes = (
            "task_delegated",
            "task_acknowledged",
            "project_created",
            "loop_planned",
            "submission_conflict",
        )
        for row in contract["events"]:
            for code in new_codes:
                if code in row["source_events"]:
                    # Not wired yet: the planned emit point is labelled
                    # #future: (same convention as contract_snapshot /
                    # artifact_delivered), so no new _SIGNAL_EMITTERS entry.
                    assert f"#future:{code}" in row["emitters"], (code, row["emitters"])


class TestAggregation:
    def test_groups_by_code_with_count_level_latest_ts(self) -> None:
        result = server.signal_events.aggregate_signal_events(
            [
                "task_submitted",
                {"code": "task_submitted", "ts": "2026-11-03T10:00:00Z"},
                {"code": "task_submitted", "ts": "2026-11-03T09:00:00Z", "level": "warn"},
                {"code": "loop_planned", "ts": "2026-11-03T11:00:00Z"},
            ]
        )
        assert result["ok"] is True
        assert result["unknown"] == []
        group = result["by_code"]["task_submitted"]
        assert group["count"] == 3
        assert group["level"] == "warn"
        assert group["latest_ts"] == "2026-11-03T10:00:00Z"
        assert result["by_code"]["loop_planned"]["count"] == 1
        assert result["by_code"]["loop_planned"]["level"] == "info"

    def test_unknown_code_rejected_not_aggregated(self) -> None:
        result = server.signal_events.aggregate_signal_events(
            [
                "task_submitted",
                "NOT_A_CONTRACT_CODE",
                {"code": "totally_new_code"},
            ]
        )
        assert "task_submitted" in result["by_code"]
        assert "NOT_A_CONTRACT_CODE" not in result["by_code"]
        assert "totally_new_code" not in result["by_code"]
        # Rejected and reported — never silently dropped or accepted:
        assert sorted(result["unknown"]) == ["NOT_A_CONTRACT_CODE", "totally_new_code"]

    def test_free_text_is_never_aggregated(self) -> None:
        result = server.signal_events.aggregate_signal_events(
            [
                {"code": "task_submitted", "ts": "2026-11-03T10:00:00Z"},
                # A carrier with free text but no code: nothing to aggregate.
                {"note": "the worker said it would be done by lunch"},
                # A bare string outside the vocabulary: reported as unknown.
                "free text that is not a code",
            ]
        )
        assert list(result["by_code"]) == ["task_submitted"]
        assert result["by_code"]["task_submitted"]["count"] == 1
        assert result["unknown"] == ["free text that is not a code"]

    def test_vocabulary_exposed_for_queries(self) -> None:
        result = server.signal_events.aggregate_signal_events([])
        assert result["ok"] is True
        assert result["by_code"] == {}
        assert len(result["codes"]) == 20
