# TeamHarness Claude Code Adapter

This adapter builds the local Claude Code delivery bundle for TeamHarness.

The bundle is for local remote-worker deployments. HiClaw owns the assets and
the worker code; LoongSuite/Pilot or polite owns local installation and process
management.

```text
agentteams-claude-code-local-runtime-0.0.2.tar.gz
├── teamharness-claude-plugin/
├── claude-code-worker/
├── scripts/
│   ├── install.sh
│   ├── uninstall.sh
│   └── worker-entrypoint.sh
└── worker.manifest.json
```

Boundaries:

- `teamharness-claude-plugin/` is the Claude Code plugin package. It contains
  TeamHarness prompts, skills, MCP config, runtime-neutral MCP assets, and
  Claude-local hooks. It does not contain the worker loop.
- `claude-code-worker/` is the local worker loop. It pulls
  `shared/runtime/members/{memberName}/runtime.yaml`, listens to the Matrix Team
  Room, invokes `claude --plugin-dir <teamharness-claude-plugin>
  --output-format stream-json`, maps optional `llm.model` to `--model` and
  `llm.baseUrl` / `llm.apiKey` to Claude process environment variables, streams
  Claude progress back to Matrix, records Claude session ids by Matrix room and
  desired AgentSpec package state, and keeps state under
  `.agentteams/runtime/claude-code/`.
- `scripts/install.sh` installs or refreshes Claude plugin assets only. It does
  not start the worker.
- `worker.manifest.json` is the process contract for LoongSuite/Pilot or polite.
  Pilot starts `scripts/worker-entrypoint.sh --member <memberName>` and owns
  pid, log, status, restart, stop, and bundle update ordering.

Build the local bundle with:

```bash
ruby plugins/teamharness/remote/claude-code/scripts/build-claude-local-bundle.rb
```

Run the worker directly for local demo/debug:

```bash
./scripts/worker-entrypoint.sh --member claude-dev
```
