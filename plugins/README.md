# AgentTeams Plugins

This directory contains AgentTeams runtime plugin packages and the local fallback
CLI for installing them.

Stage 2 establishes the TeamHarness plugin package contract. The package is
installed by the AgentTeams `agentteams` CLI by default, and it is also compatible
with the LoongSuite/Pilot `plugin-probe` convention for local QwenPaw runtime
deployment.

Remote-managed local workers use runtime-specific packages under
`teamharness/remote/`. Claude Code local management assets, worker code, and
LoongSuite runtime templates live there so they do not mix with the
runtime-neutral TeamHarness base package.

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

- `scripts/install.sh`: detects supported local runtimes and dispatches to the
  matching TeamHarness adapter.
- `scripts/uninstall.sh`: removes the runtime-specific installation through the
  matching adapter when available.

Both the AgentTeams CLI and LoongSuite `plugin-probe` path call the same lifecycle
scripts. Runtime-specific details stay inside TeamHarness adapters.

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
