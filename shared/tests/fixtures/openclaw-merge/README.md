# openclaw.json merge golden fixtures

Shared-fixture contract: each case below is a `(remote.json, local.json, expected.json)`
triple. All implementations of the openclaw.json merge —

  - `shared/python/agentteams_openclaw_merge` (canonical Python package)
  - `copaw/src/copaw_worker/sync.py` (delegates to the shared package)
  - `hermes/src/hermes_worker/sync.py` (delegates to the shared package)
  - `shared/lib/merge-openclaw-config.sh` (`python3 -m agentteams_openclaw_merge`)

MUST produce output that is JSON-equal to `expected.json` when fed the same
`remote.json` + `local.json` pair. This directory is the single source of
truth for those inputs/outputs so the implementations can't silently
drift apart again.

Consumers:
  - `shared/tests/test-merge-openclaw-config.sh` — exercises the shell wrapper
    (skips if `python3` or the merge package is unavailable).
  - `shared/tests/test_merge_openclaw_config_parity.py` — exercises the shared
    package and both worker sync shims via pytest.

Adding a case: drop a new `<case>/remote.json`, `<case>/local.json`, and
`<case>/expected.json` in this directory; both consumers auto-discover cases
by directory listing.
