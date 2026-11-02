"""Golden projection tests for the agentteams-matrix bundle contract.

The bundle contract.yaml is the fixed sample contract. The QwenPaw
projection must reproduce the committed snapshot byte-for-byte.
"""

import subprocess
import sys
from pathlib import Path

GOLDEN_DIR = Path(__file__).resolve().parent
REPO_ROOT = GOLDEN_DIR.parents[3]
CONTRACT = REPO_ROOT / "plugins" / "agentteams-matrix" / "contract.yaml"
PROJECTOR = REPO_ROOT / "plugins" / "scripts" / "project-qwenpaw-contract.py"
SECRETS = GOLDEN_DIR / "secrets.json"
EXPECTED = GOLDEN_DIR / "expected" / "agentteams-matrix.json"


def test_projection_matches_committed_snapshot(tmp_path):
    out = tmp_path / "generated.json"
    result = subprocess.run(
        [
            sys.executable,
            str(PROJECTOR),
            "--contract",
            str(CONTRACT),
            "--secrets",
            str(SECRETS),
            "--out",
            str(out),
        ],
        cwd=REPO_ROOT,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    assert result.returncode == 0, (
        f"projector failed: {result.stdout}\n{result.stderr}"
    )
    generated = out.read_bytes()
    expected = EXPECTED.read_bytes()
    assert generated == expected, (
        "generated projection does not match the committed golden snapshot"
    )


def test_projection_is_deterministic_across_runs(tmp_path):
    outputs = []
    for index in range(2):
        out = tmp_path / f"generated-{index}.json"
        result = subprocess.run(
            [
                sys.executable,
                str(PROJECTOR),
                "--contract",
                str(CONTRACT),
                "--secrets",
                str(SECRETS),
                "--out",
                str(out),
            ],
            cwd=REPO_ROOT,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=True,
        )
        outputs.append(out.read_bytes())
    assert outputs[0] == outputs[1]
