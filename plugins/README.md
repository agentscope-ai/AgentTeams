# AgentTeams Plugins

This directory contains every AgentTeams runtime plugin package, the local
fallback CLI for installing bundles, and the shared manifest schemas/tests.

Two distribution paths exist:

* **Image / CLI** — bundles are packed into tarballs and installed by the
  `agentteams` CLI (fallback) or a runtime adapter; built-in plugins ship inside
  the controller/worker images.
* **Module channels (Millforge)** — plugins can also be installed and upgraded on
  a running deployment from the Millforge release mirror. See
  [docs/usage/module-update.md](../docs/usage/module-update.md) for the index and
  install contract.

## Inventory

| Directory | Plugin id | Form | Runtime / entry | How it is installed |
|:--|:--|:--|:--|:--|
| `teamharness` | `teamharness` | Shared prompts, skills and MCP tools | Harness-independent | Bundled with Core |
| `agentteams-teamharness` | `agentteams-teamharness` | Bundle referencing the shared TeamHarness source | OpenClaw / Hermes | Bundle lifecycle scripts |
| `agentteams-matrix` | `agentteams-matrix` | Shared Matrix core and portable runtime adapters | OpenClaw / Hermes | Bundle lifecycle scripts |
| `agentteams-matrix-openclaw` | `agentteams-matrix-openclaw` | OpenClaw channel plugin (JS plugin + Python sidecar) | OpenClaw | OpenClaw adapter path (`openclaw.plugin.json`, `install.sh`) |
| `qwen-code-bridge` | `qwen-code-bridge` | QwenPaw plugin (`type: general`) with `entry.backend: plugin.py` + `entry.frontend: frontend/index.js` | QwenPaw 2.2.x | `qwenpaw plugin install` / `POST /api/plugins/upload` / Millforge module `qwenpaw-qwencode-bridge` |
| `agentteams-manager-tools` | `agentteams-manager-tools` | QwenPaw tool plugin (`type: tool`) | QwenPaw (manager) | shipped in the controller/manager image (`plugins/` → `/opt/agentteams/plugins/`) |
| `agentteams-upgrade` | `agentteams-upgrade` | QwenPaw general plugin (`type: general`) | QwenPaw | shipped in the controller/manager image |
| `workerflow` | `workerflow` | `AgentTeamPlugin` (prompts only) | QwenPaw worker | built into the worker image by the Makefile adapter targets |
| `cli` | `agentteams` | Python CLI (PyPI package) | — | `pip install -e plugins/cli` (or the PyPI release) |
| `schemas` | — | `plugin.schema.json`, `bundle.schema.json` | — | referenced by validators and tests |
| `scripts` | — | `validate-plugin.rb`, `package-plugin.rb`, `install-artifact.sh`, `verify-artifact.sh`, `resolve-plugin-release.sh`, `status.sh` | — | build/release tooling |
| `tests` | — | Shared plugin test suites (manifest, bundle lifecycle, parity, release resolver) | — | `pytest -q plugins/tests` |

## Versioning and release discipline

* Every manifest carries the plugin version (`plugin.yaml` → `metadata.version`,
  or `plugin.json` → `version`); the CHANGELOG’s top heading must match it — the
  release gate (`plugins/tests/plugins/test_plugin_manifests.py`) enforces both.
* Adapter manifests (`adapters/*/plugin.json`, `openclaw.plugin.json`,
  `adapters/*/plugin.yaml`) carry the **same** version as their bundle when they
  declare one.
* Release tags are `<module>-v<semver>`; the tag, the manifest and the Millforge
  index entry must agree. Pre-releases use `-alpha.N` / `-beta.N` suffixes.
* `plugins/teamharness/` owns the shared Task protocol. The
  `plugins/agentteams-teamharness/` bundle references the shared source and
  materializes it in portable releases. Controller and Embedded images ship
  the canonical `plugins/teamharness/` tree.

## QwenPaw Source and Upgrade

The [QwenPaw Harness](https://github.com/agentteams-group/agentteams-harness-qwenpaw)
owns the native Task tools, receipts, and Matrix channel. The managed QwenPaw
image packages those adapters with the shared TeamHarness and Matrix assets
from Core. Upgrade Core and the QwenPaw Harness as one release.

## OpenClaw and Hermes Bundles

The lifecycle entrypoints are the shared contract:

- `scripts/install.sh`: detects the supported local runtimes — `openclaw` on PATH or via `$OPENCLAW_CONFIG` /
  `~/.openclaw/openclaw.json`; `hermes` on PATH or via `$HERMES_HOME` /
  `~/.hermes/config.yaml` — and dispatches to the matching adapter for every
  detected runtime, so coexisting runtimes are all installed.
  `--runtime <name>` (repeatable) selects runtimes explicitly and bypasses
  detection; `--dry-run` previews through every adapter without writing.
- `scripts/uninstall.sh`: removes the runtime-specific installation through
  the matching adapter. Detection is artifact-driven (config files and staged
  plugin roots, not just the runtime binary), so residue is removed even
  after the runtime binary is gone; the adapters are idempotent, so an
  uninstall with nothing installed is a no-op. `--runtime` and `--dry-run`
  behave as in install.

Interpreter contract: the lifecycle scripts (and every per-runtime adapter
script) run on the system shell — **bash 3.2 or later, macOS system bash
included**. They deliberately avoid bash-4-only constructs (associative
arrays, `mapfile`/`readarray`, case-modifying expansions such as `${var^^}`,
`[[ -v ]]`, `wait -n`, `&>>`, `|&`, `coproc`, nameref declarations); the
per-runtime status state is plain variables plus `set_status`/`get_status`
helpers. `plugins/tests/teamharness/test_shell_portability.py` enforces the
contract with a static denylist scan over every lifecycle script and a
real-interpreter dry-run when a bash 3.2 is resolvable: set
`TEAMHARNESS_BASH32` to a bash 3.2 path (or provide `bash3.2` / `bash-3.2`
on PATH) and the whole lifecycle suite runs under it. On macOS the system
`/bin/bash` is 3.2.57, so macOS CI exercises this automatically.

## Boundaries

- `desired.agentPackage` in the runtime config contract is an AgentTeams AgentSpec
  package. It is not this TeamHarness plugin package.
- Remote-managed Claude Code workers use
  `teamharness/remote/claude-code/` and produce a separate
  `agentteams-claude-code-local-runtime-0.0.1.tar.gz` bundle.

## Validation

```bash
plugins/tests/run-integration-tests.sh
python3 -m compileall -q plugins/cli/src
ruby plugins/scripts/validate-plugin.rb plugins/teamharness/plugin.yaml
bash -n plugins/teamharness/scripts/install.sh
bash -n plugins/teamharness/scripts/uninstall.sh
git diff --check
```
