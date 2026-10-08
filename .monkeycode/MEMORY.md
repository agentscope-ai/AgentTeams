# User Instruction Memory

This file records user instructions, preferences, and teachings for reference in future interactions.

## Entries

[Project Knowledge Summary]
- Date: 2026-10-08
- Context: Discovered by Agent while running tests/test-build-workflows.py locally
- Category: Troubleshooting & Debugging
- Instructions:
  - tests/test-build-workflows.py needs both PyYAML and jq; local sandboxes often lack jq. `apt-get install -y jq` fails until `apt-get update` is run first (stale package lists).
  - jq 1.6 `jq -e` exits 0 on empty stdin, so `cmd | jq -e '...'` alone cannot detect empty output. Pair it with an explicit `[[ -z "$var" ]]` guard (as now done in build-image.yml / release.yml / build-deepseek-harness.yml).
  - The same test runs in CI via .github/workflows/helm-lint.yml, which installs PyYAML==6.0.3 and relies on the runner's preinstalled jq.
