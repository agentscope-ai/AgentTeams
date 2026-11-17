---
name: gateway-management
description: Configure model and MCP assets through Controller and the gateway before assigning them to Workers.
---

# Gateway management

You use the same Controller asset APIs as Dashboard. Your Manager identity is
already configured in `agt`; never read tokens or use Higress Console passwords.
Do not use legacy MCP shell scripts: they can authorize every Worker.

## Priority and fallback

1. Inspect the requested Worker once with `agt get workers NAME -o json`.
2. Inspect existing gateway assets. Reuse a matching asset rather than create
   a duplicate. Changing a shared asset can affect other Workers; apply only
   the configuration the user requested.
3. Create or update the gateway asset, verify the service, and bind only the
   requested Worker. Preserve its other models, MCP servers and settings.
4. Verify the saved Worker identity through the gateway before reporting it
   usable. If configuration is saved but verification fails or runtime sync is
   pending, report those stages separately. Do not claim completion from a save.

Use direct Agent CR configuration with `agt apply -f FILE` only when the gateway
capability returns HTTP 501 (unsupported/unconfigured), or the deployment is
explicitly known not to integrate the requested resource (for example a local
stdio-only MCP). Explain that reason and preserve unrelated CR fields. Use only
schema-supported model/MCP fields; never invent credential fields or edit native
Harness config. If required direct-endpoint configuration is unavailable in the
CR schema, report that limitation rather than inventing it.

HTTP 401/403, a 409 conflict, invalid input, failed upstream credentials, and
temporary connection errors are not unsupported capabilities. Do not fall back
to CR edits, disable gateway auth, or broaden Consumer permissions for those
errors. Retry a transient activation failure once when appropriate; otherwise
report the current stage. Do not run shell polling loops.

## Models

```bash
agt gateway models capabilities
agt gateway models list
agt gateway models get connection-name
agt gateway models save -f model.yaml
agt gateway models verify connection-name --model team-chat
agt gateway models bind --worker worker-name --provider connection-name --model team-chat
agt gateway models verify --worker worker-name
```

`model.yaml` describes a gateway Provider, not an Agent CR:

```yaml
name: connection-name
type: openai
protocol: openai/v1
tokens: [USER_SUPPLIED_API_KEY]
rawConfigs:
  openaiCustomUrl: https://models.example/v1
  modelMapping:
    team-chat: actual-upstream-model-id
```

Use an existing secret file or user-supplied credential; do not echo the key in
commands, messages or reports. For changes to an existing connection use
`agt gateway models save --update -f model.yaml`. Empty/omitted tokens preserve
the stored credential on update. The Controller saves the Provider and creates
or synchronizes its managed route, preserving existing route permissions.
If route synchronization fails after Provider save, retry with `--update`.
An existing manually managed route remains authoritative.

`verify connection-name --model ALIAS` checks the upstream connection and may
make one short generation request. `verify --worker NAME` checks the saved
model with that Worker's gateway credential. These are different proofs.
Model selection uses the alias, not necessarily the upstream model ID.

## MCP servers

```bash
agt gateway mcps capabilities
agt gateway mcps list
agt gateway mcps get docs
agt gateway mcps save -f mcp.yaml
agt gateway mcps verify docs
agt gateway mcps bind docs --worker worker-name
agt gateway mcps verify docs --worker worker-name
agt gateway mcps unbind docs --worker worker-name
```

For an existing HTTP MCP, use this input:

```yaml
name: docs
description: Documentation tools
connection:
  kind: proxy
  backendUrl: https://tools.example/mcp
  transport: streaminghttp
  auth: {type: none}
  headers: []
  query: []
  timeout: 15000
  tools: []
```

For upstream bearer auth use `auth: {type: bearer, secret: USER_SUPPLIED_KEY}`.
The upstream key stays in the gateway asset; the Worker uses its own gateway
Consumer credential. A saved MCP is listed in Asset Center automatically.
`save` updates an existing managed MCP and preserves stored blank secrets.
After an update, verify again before adding Workers.

For HTTP APIs use `connection.kind: rest` and define `tools` with `name`,
`description`, `method`, `path`, and `parameters` (each parameter has `name`,
`type`, `position` and `required`). Alternatively import an OpenAPI file with
`agt gateway mcps import -f openapi.yaml` and put its returned `rawConfig` in
the connection. For databases use `kind: database` and `database` with `type`
(`MYSQL`, `POSTGRESQL` or `CLICKHOUSE`), `host`, `port`, `username`, `password`
and `dbname`. These gateway capabilities must exist in the deployment.

`verify` initializes MCP and lists tools; it never runs a business tool.
After binding, tell the Worker to use its runtime's native MCP tool when present
(QwenPaw exposes assigned servers natively). An empty mcporter CLI config does
not prove a native server is unavailable. Distinguish gateway identity checks
from the Worker's actual runtime tool discovery and authorized invocation.
Only when the user's task authorizes an actual tool call, use
`agt gateway mcps call docs --tool TOOL --arguments '{"key":"value"}'`.
Use per-Worker `bind`/`unbind`; `authorize -f workers.json` replaces the whole
authorization list and is only appropriate when the user requests that scope.
Remove bindings before `agt gateway mcps delete docs`. Deleting an asset is
different from removing it from one Worker.
