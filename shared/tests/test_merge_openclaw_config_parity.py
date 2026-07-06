"""Golden/parity test for the openclaw.json merge, Python side.

Feeds the shared fixture pairs under
``shared/tests/fixtures/openclaw-merge/<case>/{remote,local,expected}.json``
through ``_merge_openclaw_config`` in BOTH Python implementations and asserts
the merged output is JSON-equal to ``expected.json``. The SAME fixtures are
also consumed by ``shared/tests/test-merge-openclaw-config.sh`` (shell impl,
jq-based) — see ``shared/tests/fixtures/openclaw-merge/README.md`` for the
shared-fixture contract.

Both packages' ``src`` roots are added to ``sys.path`` for the duration of
this module's import (rather than requiring ``pip install -e`` for both
simultaneously) — ``copaw_worker.sync`` needs its sibling
``copaw_worker.bridge`` importable, so isolated file-path loading isn't
enough. ``hermes_worker.sync`` has no such intra-package import but is
loaded the same way for consistency.

Run with: python -m pytest shared/tests/test_merge_openclaw_config_parity.py
(no external PYTHONPATH needed; both package roots are added here.)
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


def _load_package_module(src_root: Path, module_name: str) -> ModuleType:
    src_str = str(src_root)
    inserted = src_str not in sys.path
    if inserted:
        sys.path.insert(0, src_str)
    try:
        # Force a fresh import bound to this src_root even if a module of the
        # same name was already imported from a different path earlier.
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
def copaw_sync():
    return _load_package_module(COPAW_SRC, "copaw_worker.sync")


@pytest.fixture(scope="module")
def hermes_sync():
    return _load_package_module(HERMES_SRC, "hermes_worker.sync")


@pytest.mark.parametrize("case_dir", _fixture_cases(), ids=lambda p: p.name)
def test_copaw_merge_matches_golden(copaw_sync, case_dir: Path):
    remote_text = (case_dir / "remote.json").read_text(encoding="utf-8")
    local_text = (case_dir / "local.json").read_text(encoding="utf-8")
    expected = json.loads((case_dir / "expected.json").read_text(encoding="utf-8"))

    merged_text = copaw_sync._merge_openclaw_config(remote_text, local_text)

    assert json.loads(merged_text) == expected


@pytest.mark.parametrize("case_dir", _fixture_cases(), ids=lambda p: p.name)
def test_hermes_merge_matches_golden(hermes_sync, case_dir: Path):
    remote_text = (case_dir / "remote.json").read_text(encoding="utf-8")
    local_text = (case_dir / "local.json").read_text(encoding="utf-8")
    expected = json.loads((case_dir / "expected.json").read_text(encoding="utf-8"))

    merged_text = hermes_sync._merge_openclaw_config(remote_text, local_text)

    assert json.loads(merged_text) == expected


@pytest.mark.parametrize("case_dir", _fixture_cases(), ids=lambda p: p.name)
def test_copaw_and_hermes_agree(copaw_sync, hermes_sync, case_dir: Path):
    """Belt-and-braces: the two Python impls must also agree with each other."""
    remote_text = (case_dir / "remote.json").read_text(encoding="utf-8")
    local_text = (case_dir / "local.json").read_text(encoding="utf-8")

    copaw_merged = json.loads(copaw_sync._merge_openclaw_config(remote_text, local_text))
    hermes_merged = json.loads(hermes_sync._merge_openclaw_config(remote_text, local_text))

    assert copaw_merged == hermes_merged
