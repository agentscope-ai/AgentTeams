# TeamHarness Codex CLI Adapter

This adapter installs the runtime-neutral TeamHarness assets for a host-local
Codex CLI remote member. Codex remains an execution backend; the adapter does
not add a controller-managed `runtime: codex` value or copy Codex credentials.

The remote runtime launches `codex app-server --listen stdio://`, injects the
TeamHarness remote-member instructions through the app-server protocol, and
registers the packaged TeamHarness MCP server through non-persistent CLI config
overrides.

Install the TeamHarness package in the target project with the AgentTeams
plugin CLI. The lifecycle script writes only a non-secret marker below
`.agentteams/codex-cli/`:

```bash
agentteams plugin install teamharness --package dist/teamharness.tar.gz
```

Run the separately packaged bridge with:

```powershell
python runtime/run.py doctor --runtime-config runtime.yaml --workspace C:\src\project
python runtime/run.py run --runtime-config runtime.yaml --workspace C:\src\project
```

`AGENTTEAMS_MATRIX_URL` and `AGENTTEAMS_WORKER_MATRIX_TOKEN` are required at
runtime and are never written by this adapter.
