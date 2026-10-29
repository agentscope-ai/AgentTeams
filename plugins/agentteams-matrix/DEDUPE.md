# Dedupe status

The QwenPaw adapter in this bundle is a **P0 compatibility copy** of the
Matrix channel implementation that still lives under
`plugins/agentteams-matrix-channel`. It exists so the portable bundle is
self-contained while the legacy plugin and this bundle coexist until the
switch-over.

## Identity

The bundle identity is `agentteams-matrix` (bundle `plugin.json` id, contract
name). The QwenPaw plugin installed from this bundle keeps the existing
`agentteams-matrix-channel` identity — plugin id, install path
(`~/.qwenpaw/plugins/agentteams-matrix-channel`), and state marker unchanged —
so the switch-over does not change the running plugin.

## Source

`plugins/agentteams-matrix-channel/` — the live implementation: the
`agentteams_matrix/` package plus the `plugin.py` entry point. Sync baseline
commit: `fc25f272` (= main `23315f8e` + this PR's content).

## Target / copied files

- `adapters/qwenpaw/agentteams_matrix/__init__.py`
- `adapters/qwenpaw/plugin.py` — the plugin entry point

Not byte-copied: `adapters/qwenpaw/agentteams_matrix/channel.py` has been
bundle-owned code since P9/PR1 M3 (#1468) — it delegates mention parsing,
the per-room history buffer and session-id derivation to the bundle's
`core/` package instead of carrying its own copy. The legacy tree's
`channel.py` remains the standalone reference implementation; the drift
test still checks the file's presence on both sides and exempts only the
byte-compare.

Not copied: the QwenPaw manifest (`adapters/qwenpaw/plugin.json`) and the
adapter scaffolding (`README.md`, `install.sh`, `uninstall.sh`) are bundle
manifest/install files, not part of the synced implementation copy.

## Update path

Re-sync is a byte-for-byte copy performed in the same commit as the upstream
change, followed by the drift test
(`plugins/tests/agentteams-teamharness/golden/test_compatibility_copy_drift.py`).
Single-file hand edits of the copies are not allowed: a copy that drifts
fails the drift test and must be re-synced whole. Since P9/PR1 M3 (#1468)
the drift test byte-exempts the bundle's `channel.py` (bundle-owned, see
above); every other synced file remains byte-checked.

## Removal condition

At `#1451` W2, when the split modules become the bundle's own code, the
copies are removed from this bundle. Until then the copies must not evolve
into a second implementation — every change lands in
`plugins/agentteams-matrix-channel/` first and is re-synced whole in the
same commit.
