#!/usr/bin/env python3
"""Verify lazy setup from an unmodified disposable Worker environment."""
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import subprocess
import time

ROOT = Path(__file__).resolve().parent
RUNNER = ROOT / "browser-automation/scripts/headless.sh"
CLI = Path("/opt/agentteams/optional/browser-automation/node_modules/.bin/agent-browser")
assert not CLI.exists(), "Run this test only in a fresh disposable Worker container."
assert not Path("/tmp/chromium").exists()

def invoke():
    started = time.monotonic()
    result = subprocess.run(["bash", str(RUNNER), "--version"], capture_output=True,
                            text=True, timeout=600)
    return {"code": result.returncode, "stdout": result.stdout, "stderr": result.stderr,
            "seconds": round(time.monotonic()-started, 3)}

with ThreadPoolExecutor(max_workers=2) as pool:
    results = list(pool.map(lambda _: invoke(), range(2)))
print(json.dumps(results), flush=True)
assert all(r["code"] == 0 for r in results), results
assert all(r["stdout"].strip() == "agent-browser 0.41.2" for r in results), results
assert sum("Installing browser dependencies" in r["stderr"] for r in results) == 1, results
assert CLI.exists() and Path("/tmp/chromium").exists()

# A ready call must not enter setup again or pollute command stdout.
warm = invoke()
assert warm["code"] == 0 and warm["stdout"].strip() == "agent-browser 0.41.2", warm
assert "Installing browser dependencies" not in warm["stderr"], warm
print(json.dumps({"passed": True, "firstUse": results, "warm": warm}), flush=True)
