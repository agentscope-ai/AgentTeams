"""Structural hash skeleton for the TeamHarness loop contracts.

Three loop protocol contracts — occurrence, iteration, and kick — will
land in ``plugins/teamharness/contracts/`` from the held branch #1510
(TaskForge T1-T2). This skeleton pre-wires the drift guard so the
contracts are protected the moment the files appear, without adopting
any #1510 schema copy into this tree (that would duplicate management
with the held branch).

Per-contract behaviour:

* file missing  -> ``pytest.skip`` (the held branch owns their arrival);
* file present, pin registered -> the canonical structural hash
  (sorted keys / canonical JSON / sha256, identical scheme to the
  matrix bundle guard) must match the pin — any drift turns red;
* file present, no pin registered -> the test fails and prints the
  computed hash, so the first arrival is pinned deliberately before
  drift protection can silently go stale.

The canonical form is independent of key order, whitespace, and
platform, so the hashes are stable everywhere.
"""

import hashlib
import json
from pathlib import Path

import pytest

GOLDEN_DIR = Path(__file__).resolve().parent
REPO_ROOT = GOLDEN_DIR.parents[3]
CONTRACTS_DIR = REPO_ROOT / "plugins" / "teamharness" / "contracts"

LOOP_CONTRACTS = {
    "occurrence": CONTRACTS_DIR / "loop-occurrence.json",
    "iteration": CONTRACTS_DIR / "loop-iteration.json",
    "kick": CONTRACTS_DIR / "loop-kick.json",
}

# Canonical structural hashes, registered when the files first land
# from #1510 (TaskForge T1-T2). Empty at the baseline commit on
# purpose: the skeleton must not copy the held branch's schemas here.
EXPECTED_CONTRACT_SHA256 = {
    # "occurrence": "<sha256>",
    # "iteration": "<sha256>",
    # "kick": "<sha256>",
}


def canonical_sha256(obj) -> str:
    """Return the sha256 hex digest of the canonical JSON form.

    Canonical form: keys sorted recursively, compact separators,
    non-ASCII preserved. Identical structures always produce the same
    digest, on any platform. Keep in sync with the matrix bundle
    guard (plugins/tests/agentteams-matrix/golden/).
    """
    canonical = json.dumps(
        obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


@pytest.mark.parametrize("name", sorted(LOOP_CONTRACTS))
def test_loop_contract_structural_hash(name):
    path = LOOP_CONTRACTS[name]
    if not path.is_file():
        pytest.skip(
            f"loop contract '{path.name}' is not present in this tree yet; "
            "it arrives with held branch #1510 (TaskForge T1-T2). This "
            "test activates automatically once the file lands."
        )

    try:
        obj = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        pytest.fail(
            f"loop contract '{path.name}' is not valid JSON: {exc}"
        )
    assert isinstance(obj, dict), (
        f"loop contract '{path.name}' must parse to a JSON object"
    )

    actual = canonical_sha256(obj)
    expected = EXPECTED_CONTRACT_SHA256.get(name)
    if expected is None:
        pytest.fail(
            f"loop contract '{path.name}' has landed but no canonical "
            "hash pin is registered yet. Register the hash below in "
            "EXPECTED_CONTRACT_SHA256 to activate drift protection:\n"
            f"computed (current): {actual}"
        )
    assert actual == expected, (
        f"loop contract '{path.name}' structural hash drifted.\n"
        f"expected (pinned): {expected}\n"
        f"computed (current): {actual}\n"
        "If the change is intentional, update the pin in this test."
    )
