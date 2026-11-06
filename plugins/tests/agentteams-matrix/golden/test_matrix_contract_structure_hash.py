"""Structural drift guard for the agentteams-matrix bundle contract.

The pin below covers the *structure* of the bundle contract.yaml: the
document is parsed, re-serialised as canonical JSON (sorted keys,
compact separators, non-ASCII preserved) and hashed with sha256. The
hash is therefore stable across platforms and independent of YAML
formatting (indentation or key order in the file do not matter), but
any change to any contract field turns this test red until the pin is
deliberately updated.
"""

import hashlib
import json
from pathlib import Path

import yaml

GOLDEN_DIR = Path(__file__).resolve().parent
REPO_ROOT = GOLDEN_DIR.parents[3]
CONTRACT = REPO_ROOT / "plugins" / "agentteams-matrix" / "contract.yaml"

# Canonical structural hash of contract.yaml at the baseline commit.
# To update: change the contract, run this test, and replace the pin
# with the "computed (current)" hash from the failure message —
# deliberate contract changes only.
EXPECTED_CONTRACT_SHA256 = (
    "a67da42171d05048696c2eb785483b5571aaf91c68aa20d2b78d48b755b5ba43"
)


def canonical_sha256(obj) -> str:
    """Return the sha256 hex digest of the canonical JSON form.

    Canonical form: keys sorted recursively, compact separators,
    non-ASCII preserved. Identical structures always produce the same
    digest, on any platform.
    """
    canonical = json.dumps(
        obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def test_bundle_contract_structural_hash_is_pinned():
    assert CONTRACT.is_file(), f"missing bundle contract: {CONTRACT}"
    obj = yaml.safe_load(CONTRACT.read_text(encoding="utf-8"))
    assert isinstance(obj, dict), "contract.yaml must parse to a mapping"
    actual = canonical_sha256(obj)
    assert actual == EXPECTED_CONTRACT_SHA256, (
        "agentteams-matrix contract.yaml structural hash drifted.\n"
        f"expected (pinned): {EXPECTED_CONTRACT_SHA256}\n"
        f"computed (current): {actual}\n"
        "If the change is intentional, update the pin in this test."
    )
