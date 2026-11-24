#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# Keep the existing entrypoint while sharing the public Task observer fixtures.
python3 "$ROOT/tests/test_teamharness_notification_wait.py"
