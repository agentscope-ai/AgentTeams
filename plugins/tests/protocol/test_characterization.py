"""Protocol characterization tests (Phase 0/5 — S0.2 / P5.3).

CoPaw domain: fixtures/cases/*/snapshots/copaw-domain/
TeamHarness MCP: fixtures/cases/*/snapshots/teamharness-mcp/
"""
from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
PROTOCOL_DIR = Path(__file__).resolve().parent
KNOWN_DIFFS = PROTOCOL_DIR / "known-engine-diffs.json"


def test_known_engine_diffs_documents_mcp_comparison() -> None:
    """Ensure the diff catalog references both engines and comparison tests."""
    data = json.loads(KNOWN_DIFFS.read_text(encoding="utf-8"))
    assert "copaw_domain" in data["engines"]
    assert "teamharness_mcp" in data["engines"]
    assert data["diffs"], "expected at least one documented engine diff"
    tests = data["comparison_tests"]
    assert "plugins/tests/protocol/test_characterization.py" in tests[
        "copaw_domain_characterization"
    ]
    assert tests["teamharness_mcp_integration"]


def test_fixture_shapes_exist() -> None:
    shapes = PROTOCOL_DIR / "fixtures" / "shapes"
    required = [
        "task-meta.sample.json",
        "project-meta.sample.json",
        "plan-dag.sample.md",
        "spec.sample.md",
        "result.sample.md",
    ]
    for name in required:
        assert (shapes / name).is_file(), f"missing shape sample: {name}"


def test_copaw_domain_characterization_matches_golden() -> None:
    runner = PROTOCOL_DIR / "run_characterization.py"
    proc = subprocess.run(
        [sys.executable, str(runner)],
        cwd=str(REPO_ROOT),
        capture_output=True,
        text=True,
        check=False,
    )
    assert proc.returncode == 0, proc.stderr or proc.stdout
