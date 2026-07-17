"""Golden/parity test for the openclaw.json merge implementations.

**Phase 4 acceptance suite (remediation S0.3 / M4.1):** Do not change merge
rules without updating fixtures and explicit Phase 4 review. The canonical
implementation is ``agentteams_openclaw_merge``; ``copaw_worker.sync`` and
``hermes_worker.sync`` delegate to it.

Feeds the shared fixture pairs under
``shared/tests/fixtures/openclaw-merge/<case>/{remote,local,expected}.json``
through the merge function and asserts the merged output is JSON-equal to
``expected.json``. The SAME fixtures are consumed by
``shared/tests/test-merge-openclaw-config.sh`` (shell wrapper calling
``python3 -m agentteams_openclaw_merge``) — see
``shared/tests/fixtures/openclaw-merge/README.md`` for the shared-fixture
contract.

Run with: python -m pytest shared/tests/test_merge_openclaw_config_parity.py
"""
from __future__ import annotations

import importlib
import json
import sys
from pathlib import Path
from types import ModuleType

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures" / "openclaw-merge"

COPAW_SRC = REPO_ROOT / "copaw" / "src"
HERMES_SRC = REPO_ROOT / "hermes" / "src"
MERGE_SRC = REPO_ROOT / "shared" / "python" / "agentteams_openclaw_merge" / "src"


def _load_package_module(src_root: Path, module_name: str) -> ModuleType:
    src_str = str(src_root)
    inserted = src_str not in sys.path
    if inserted:
        sys.path.insert(0, src_str)
    try:
        sys.modules.pop(module_name, None)
        top_level = module_name.split(".", 1)[0]
        sys.modules.pop(top_level, None)
        return importlib.import_module(module_name)
    finally:
        if inserted:
            sys.path.remove(src_str)


def _fixture_cases() -> list[Path]:
    return sorted(
        p for p in FIXTURES_DIR.iterdir()
        if p.is_dir() and (p / "expected.json").is_file()
    )


@pytest.fixture(scope="module")
def merge_module():
    return _load_package_module(MERGE_SRC, "agentteams_openclaw_merge.merge")


@pytest.fixture(scope="module")
def copaw_sync():
    return _load_package_module(COPAW_SRC, "copaw_worker.sync")


@pytest.fixture(scope="module")
def hermes_sync():
    return _load_package_module(HERMES_SRC, "hermes_worker.sync")


def _merge_case(merge_fn, case_dir: Path) -> None:
    remote_text = (case_dir / "remote.json").read_text(encoding="utf-8")
    local_text = (case_dir / "local.json").read_text(encoding="utf-8")
    expected = json.loads((case_dir / "expected.json").read_text(encoding="utf-8"))
    merged_text = merge_fn(remote_text, local_text)
    assert json.loads(merged_text) == expected


@pytest.mark.parametrize("case_dir", _fixture_cases(), ids=lambda p: p.name)
def test_shared_merge_matches_golden(merge_module, case_dir: Path):
    _merge_case(merge_module.merge_openclaw_config, case_dir)


@pytest.mark.parametrize("case_dir", _fixture_cases(), ids=lambda p: p.name)
def test_copaw_merge_matches_golden(copaw_sync, case_dir: Path):
    _merge_case(copaw_sync._merge_openclaw_config, case_dir)


@pytest.mark.parametrize("case_dir", _fixture_cases(), ids=lambda p: p.name)
def test_hermes_merge_matches_golden(hermes_sync, case_dir: Path):
    _merge_case(hermes_sync._merge_openclaw_config, case_dir)


@pytest.mark.parametrize("case_dir", _fixture_cases(), ids=lambda p: p.name)
def test_copaw_and_hermes_agree(copaw_sync, hermes_sync, case_dir: Path):
    remote_text = (case_dir / "remote.json").read_text(encoding="utf-8")
    local_text = (case_dir / "local.json").read_text(encoding="utf-8")

    copaw_merged = json.loads(copaw_sync._merge_openclaw_config(remote_text, local_text))
    hermes_merged = json.loads(hermes_sync._merge_openclaw_config(remote_text, local_text))

    assert copaw_merged == hermes_merged
