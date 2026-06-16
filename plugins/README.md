# HiClaw Plugins

This directory contains HiClaw runtime plugin packages and the local fallback
CLI for installing them.

This package contract lets a plugin ship prompts, skills, MCP servers, runtime
adapters, and lifecycle scripts in one tarball. The package can be installed by
the HiClaw `agentteams` CLI, and it can also be consumed by local deployment
systems that unpack a tarball and run the same lifecycle script.

## Default Path: HiClaw CLI

The default installer is the HiClaw-owned `agentteams` CLI:

```bash
agentteams plugin install <name> --package dist/<name>.tar.gz
agentteams plugin list
agentteams plugin update <name> --package dist/<name>.tar.gz
agentteams plugin uninstall <name>
```

The CLI stores local state under `.agentteams/`. It does not manage cluster
worker lifecycle, and it does not hard-code runtime-specific install details.
It unpacks the plugin tarball, calls the package lifecycle script, and records
the installed manifest.

## Plugin Package Contract

Plugins are distributed as tarballs whose root is the plugin content itself:

```text
<name>.tar.gz
├── plugin.yaml
├── prompts/
├── skills/
├── mcp/
├── adapters/
│   ├── runtime-a/
│   └── runtime-b/
└── scripts/
    ├── install.sh
    └── uninstall.sh
```

The lifecycle entrypoints are the shared contract:

- `scripts/install.sh`: detects supported local runtimes and dispatches to the
  matching adapter.
- `scripts/uninstall.sh`: removes the runtime-specific installation through the
  matching adapter when available.

The HiClaw CLI calls these lifecycle scripts with `AGENTTEAMS_PLUGIN_NAME`,
`AGENTTEAMS_PLUGIN_DIR`, `AGENTTEAMS_PROJECT_DIR`, `PILOT_DATA_DIR`, and
`PILOT_LOG_DIR` set.

## Boundaries

- `desired.agentPackage` in the runtime config contract is a HiClaw AgentSpec
  package. It is not this plugin package.
- This directory defines the package, lifecycle, and CLI fallback contract.
  Plugin-specific business semantics live in each plugin package.

## Validation

```bash
python3 -m compileall -q plugins/cli/src
python3 plugins/tests/cli/test_agentteams_plugin_cli.py
ruby plugins/scripts/validate-plugin.rb path/to/plugin.yaml
git diff --check
```
