# openclaw.json merge golden fixtures

Shared-fixture contract: each case below is a `(remote.json, local.json, expected.json)`
triple. All three independent implementations of the openclaw.json merge —

  - `copaw/src/copaw_worker/sync.py` (`_merge_openclaw_config` / `_deep_merge`)
  - `hermes/src/hermes_worker/sync.py` (`_merge_openclaw_config` / `_deep_merge`)
  - `shared/lib/merge-openclaw-config.sh` (`merge_openclaw_config`, jq-based)

MUST produce output that is JSON-equal to `expected.json` when fed the same
`remote.json` + `local.json` pair. This directory is the single source of
truth for those inputs/outputs so the three implementations can't silently
drift apart again.

Consumers:
  - `shared/tests/test-merge-openclaw-config.sh` — exercises the shell impl
    directly via `jq` (skips if `jq` is not on PATH; use `bash -n` to
    lint-check without jq).
  - `shared/tests/test_merge_openclaw_config_parity.py` — exercises both
    Python impls (copaw_worker and hermes_worker) via pytest, importing each
    package's `sync` module with its own `PYTHONPATH=src` root.

Adding a case: drop a new `<case>/remote.json`, `<case>/local.json`, and
`<case>/expected.json` in this directory; both consumers auto-discover cases
by directory listing.
