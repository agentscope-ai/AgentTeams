# TeamHarness QwenPaw Adapter

This adapter turns the runtime-neutral TeamHarness package into a QwenPaw-native
plugin package. The Dockerfile and worker image do not need to understand the
TeamHarness source tree.

Build the QwenPaw package:

```bash
plugins/agentteams-teamharness/adapters/qwenpaw/scripts/build-qwenpaw-plugin.rb plugins/agentteams-teamharness/plugin.yaml
```

The generated zip contains:

```text
teamharness-qwenpaw-{version}/
├── plugin.json
├── plugin.py
└── teamharness/
    ├── plugin.yaml
    ├── prompts/
    ├── skills/
    └── mcp/
```

Install the adapter into the current QwenPaw working directory:

```bash
plugins/agentteams-teamharness/scripts/install.sh
```

`install.sh` builds the package, unpacks it, and runs:

```bash
qwenpaw plugin install <generated-package-dir> --force
```

QwenPaw writes the installed plugin under `${QWENPAW_WORKING_DIR}/plugins`.
Set `QWENPAW_WORKING_DIR` before installation when running inside a managed
worker.

## Idempotency

`install.sh` hashes the package source (manifest, prompts, skills, mcp, and
the adapter entrypoints) and records the hash in:

```text
${QWENPAW_WORKING_DIR}/.agentteams/agentteams-teamharness/installed.json
```

When the recorded hash matches the current bundle content, the install exits
without rebuilding or re-running the QwenPaw CLI. `uninstall.sh` removes the
QwenPaw plugin through the CLI and deletes the state marker, so re-running
either script is safe and leaves no residue.
