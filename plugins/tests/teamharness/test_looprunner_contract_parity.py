"""R4: dual-tree mirror of the four LoopRunner contract schemas.

R1-R3 landed the four LoopRunner schemas in the millforge repo
(``core/schemas/``), the single source of truth. This test pins the
TeamHarness mirror: the same four ``*.schema.json`` files under
``plugins/teamharness/contracts/`` (legacy source) and
``plugins/agentteams-teamharness/contracts/`` (bundle copy) must stay
byte-for-byte identical to each other and to the accepted millforge
bytes (sha256 pinned per file below). They follow the same whole-tree
copy mechanism as ``task-transitions.json`` / ``signal-mapping.json``
(drift guard:
``plugins/tests/agentteams-teamharness/golden/test_compatibility_copy_drift.py``).

Per file:
- both trees exist and are byte-for-byte identical
- the bytes equal the millforge source sha256 (mirror invariant)
- the JSON parses to an object
- the schema is a valid schema under the draft it declares in
  ``$schema`` (draft-07 today; the validator class is derived from the
  declared draft, never hard-coded to a single draft version)
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

PLUGINS_ROOT = Path(__file__).resolve().parents[2]
LEGACY_CONTRACTS = PLUGINS_ROOT / "teamharness" / "contracts"
BUNDLE_CONTRACTS = PLUGINS_ROOT / "agentteams-teamharness" / "contracts"

# File name -> sha256 of the accepted millforge source file (R1-R3).
MILLFORGE_SHA256 = {
    "occurrence.schema.json": "7ddc72f693aece04e444ad6a2be029e01e8e9b9e228d9a2fc6126acc2771d929",
    "iteration.schema.json": "a3139adf82dfbe68990f71a27301c011f651b5dd0f4967a96773b835f33bb669",
    "kick.schema.json": "a99d6ab032483e0496b4e329d5204326757e4937f47aa038c765b3637397604a",
    "looprunner-manifest.schema.json": "821f7f968744bb7196438a3081921913f2871f156ea707fcdef23bd4bdb85424",
}


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def test_mirror_is_byte_identical_across_trees_and_pinned() -> None:
    for name, expected in MILLFORGE_SHA256.items():
        legacy = LEGACY_CONTRACTS / name
        bundle = BUNDLE_CONTRACTS / name
        assert legacy.is_file(), f"missing legacy mirror: {legacy}"
        assert bundle.is_file(), f"missing bundle mirror: {bundle}"
        legacy_bytes = legacy.read_bytes()
        bundle_bytes = bundle.read_bytes()
        assert legacy_bytes == bundle_bytes, (
            f"{name}: bundle copy is not a byte-for-byte mirror of the legacy tree"
        )
        assert _sha256(legacy) == expected, (
            f"{name}: bytes drifted from the millforge source "
            f"(got {_sha256(legacy)})"
        )
        data = json.loads(legacy_bytes.decode("utf-8"))
        assert isinstance(data, dict), f"{name} must contain a JSON object"


def _validator_class(jsonschema, declared_draft: str):
    """Map the draft declared in ``$schema`` to its validator class."""
    if "draft-07" in declared_draft:
        return jsonschema.Draft7Validator
    if "draft/2019-09" in declared_draft:
        return jsonschema.Draft201909Validator
    if "draft/2020-12" in declared_draft:
        return jsonschema.Draft202012Validator
    raise AssertionError(f"unsupported declared draft: {declared_draft!r}")


def test_schemas_are_valid_schemas() -> None:
    jsonschema = pytest.importorskip("jsonschema")
    for name in MILLFORGE_SHA256:
        schema = json.loads(
            (LEGACY_CONTRACTS / name).read_text(encoding="utf-8")
        )
        draft = str(schema.get("$schema", ""))
        assert draft, f"{name} must declare a $schema"
        _validator_class(jsonschema, draft).check_schema(schema)
