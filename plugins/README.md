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
| `teamharness` | `teamharness` | `AgentTeamPlugin` bundle (surface: prompts / skills / mcp / adapters) | QwenPaw, OpenClaw, Hermes, Claude Code, DeepSeek Harness via `adapters/` | `agentteams plugin install` tarball, bundle `scripts/install.sh`, or Millforge module `teamharness` |
| `agentteams-teamharness` | `teamharness` | Bundle compatibility copy (byte-identical to `teamharness/`, drift-tested) | same as above | not installed directly — exists so bundle consumers and parity tests can compare both trees |
| `agentteams-matrix` | `agentteams-matrix` | Bundle (`plugin.json` `type: bundle`) with `adapters/{qwenpaw,openclaw,hermes}` | QwenPaw / OpenClaw / Hermes | bundle `scripts/install.sh --adapter <rt>` or Millforge module `agentteams-matrix` (installs plugin id `agentteams-matrix-channel` on QwenPaw) |
| `agentteams-matrix-channel` | `agentteams-matrix-channel` | QwenPaw channel plugin (`type: channel`, `entry.backend: plugin.py`) | QwenPaw | written into `<working-dir>/plugins/` by the MX bundle installer (or the Millforge module install) |
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
* `plugins/agentteams-teamharness/` must stay byte-identical to
  `plugins/teamharness/` (parity gates: drift tests + compatibility-copy tests).

## Default Path: AgentTeams CLI

The default installer is the AgentTeams-owned `agentteams` CLI:

```bash
agentteams plugin install teamharness --package dist/teamharness.tar.gz
agentteams plugin list
agentteams plugin update teamharness --package dist/teamharness.tar.gz
agentteams plugin uninstall teamharness
```

The CLI stores local state under `.agentteams/`. It does not manage cluster
worker lifecycle, and it does not hard-code QwenPaw or Claude Code install
details. It unpacks the TeamHarness tarball, calls the package lifecycle script,
and records the installed manifest.

## LoongSuite Compatibility

LoongSuite/Pilot is currently a local deployment integration path. The base
TeamHarness package keeps only the generic QwenPaw-compatible plugin-probe
definition. Remote-managed Claude Code runtime templates are packaged from
`teamharness/remote/claude-code/`.

TeamHarness is compatible with LoongSuite by providing:

```text
loongsuite-pilot/
├── agents.d/teamharness.json
└── plugins/teamharness.tar.gz
```

`agents.d/teamharness.json` uses LoongSuite's `plugin-probe` shape:

```json
{
  "id": "teamharness",
  "displayName": "TeamHarness",
  "deployMode": "plugin-probe",
  "detection": {
    "paths": ["~/.qwenpaw"],
    "commands": ["qwenpaw"]
  },
  "pluginProbe": {
    "source": {
      "type": "tar",
      "tarball": "$PILOT_DIR/plugins/teamharness.tar.gz",
      "destDir": "$PILOT_DATA/plugins/teamharness"
    },
    "mountType": "wrapper"
  }
}
```

LoongSuite only discovers the local runtime, unpacks the tarball, and runs the
standard lifecycle script. It does not parse `plugin.yaml`, prompts, skills, MCP
tools, hooks, or runtime adapter internals.

## TeamHarness Package Contract

TeamHarness is distributed as a tarball whose root is the plugin content itself:

```text
teamharness.tar.gz
├── plugin.yaml
├── prompts/
├── skills/
├── mcp/
├── hooks/
├── adapters/
│   └── qwenpaw/
└── scripts/
    ├── install.sh
    └── uninstall.sh
```

The lifecycle entrypoints are the shared contract:

- `scripts/install.sh`: detects the supported local runtimes — `qwenpaw` on
  PATH; `openclaw` on PATH or via `$OPENCLAW_CONFIG` /
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

Both the AgentTeams CLI and LoongSuite `plugin-probe` path call the same lifecycle
scripts. Runtime-specific details stay inside TeamHarness adapters.

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
- Cluster QwenPaw workers do not use LoongSuite. They may later reuse the same
  TeamHarness tarball or bundled assets from the worker image.
- Remote-managed Claude Code workers use
  `teamharness/remote/claude-code/` and produce a separate
  `agentteams-claude-code-local-runtime-0.0.1.tar.gz` bundle.
- Stage 2 only defines package, lifecycle, CLI fallback, and LoongSuite
  compatibility contracts. TeamHarness business semantics and real QwenPaw /
  remote-managed Claude Code runtime integration are covered in later phases.

## Validation

```bash
plugins/tests/run-integration-tests.sh
python3 -m compileall -q plugins/cli/src
ruby plugins/scripts/validate-plugin.rb plugins/teamharness/plugin.yaml
bash -n plugins/teamharness/scripts/install.sh
bash -n plugins/teamharness/scripts/uninstall.sh
git diff --check
```
