# TeamHarness QwenPaw Adapter

This adapter turns the runtime-neutral TeamHarness package into a QwenPaw-native
plugin package. The Dockerfile and worker image do not need to understand the
TeamHarness source tree.

Build the QwenPaw package:

```bash
plugins/teamharness/adapters/qwenpaw/scripts/build-qwenpaw-plugin.rb plugins/teamharness/plugin.yaml
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
plugins/teamharness/adapters/qwenpaw/install.sh
```

`install.sh` builds the package, unpacks it, and runs:

```bash
qwenpaw plugin install <generated-package-dir> --force
```

QwenPaw writes the installed plugin under `${QWENPAW_WORKING_DIR}/plugins`.
Set `QWENPAW_WORKING_DIR` before installation when running inside a managed
worker.
