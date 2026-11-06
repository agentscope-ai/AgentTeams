# Dedupe status

The `mcp/`, `prompts/`, and `skills/` trees in this bundle are **P0
compatibility copies** of the TeamHarness implementation that still lives
under `plugins/teamharness`. They exist so the portable bundle is
self-contained while the legacy plugin and this bundle coexist until the
switch-over.

## Identity

The bundle identity is `agentteams-teamharness` (bundle `plugin.json` id,
`plugin.yaml` metadata name, contract name). The QwenPaw plugin installed
from this bundle keeps the existing `teamharness` identity — plugin id,
install path, and state marker unchanged — so the switch-over does not
change the running plugin. "Millforge" is not a package or plugin name
anywhere: it appears only as the protocol namespace declared in the
contract metadata (`protocol.identifier: millforge/v0` — namespace plus
version; the identifier carries no harness, package or plugin name).

## Source

`plugins/teamharness/` — the live implementation, including the split
modules from issue #1452. Sync baseline commit: `70a21f39` (= main
`23315f8e` + this PR's content).

## Target / copied trees

- `mcp/` — 27 files (the #1452 split-module layout: the `server.py`
  dispatcher plus the `common/`, `lifecycle/`, `artifact/`, `project/`,
  `room/`, and `transport/` packages)
- `skills/` — full copy
- `prompts/` — currently byte-identical to the source
- `contracts/` — currently byte-identical to the source

Not copied: `loongsuite/` exists in the legacy plugin but is deliberately
left out of this bundle — the MCP code does not import it, and the QwenPaw
runtime loads it on its own from the legacy `plugin.yaml`.

## Update path

Re-sync is a whole-tree copy (`cp -a`) performed in the same commit as the
upstream change, followed by the drift test
(`plugins/tests/agentteams-teamharness/golden/test_compatibility_copy_drift.py`).
Single-file hand edits of the copies are not allowed: a copy that drifts
fails the drift test and must be re-synced whole.

## Removal condition

At `#1451` W2, when the split modules become the bundle's own code, the
copies are removed from this bundle. Until then the copies must not evolve
into a second implementation — every change lands in `plugins/teamharness/`
first and is re-synced whole in the same commit.

This duplication is **time-boxed**: at the switch-over, the split modules
from issue #1452 replace `mcp/` and the copied trees are removed from this
bundle. The switch-over PR must not leave a second copy of the
implementation behind.

## Follow-ups

- The DeepSeek Harness adapter ships its npm package under the name
  `agentteams-teamharness-dsh`
  (`plugins/teamharness/adapters/deepseek-harness/package.json`). That
  package name is frozen and does not move into this bundle.
