# Worker environment variables and gateway verification

Administrator and Manager callers can set `env` on Worker create/update requests:

```json
{"env":{"MY_SERVICE_URL":"https://example.com","MY_FEATURE_ENABLED":"true"}}
```

For updates, omit `env` to preserve it, send `{}` to clear user variables, or send a complete object to replace them. Values remain strings, including empty strings and whitespace. Names must match `[A-Za-z_][A-Za-z0-9_]*`; values cannot contain NUL. The API rejects collisions with the configured system environment. Existing direct CR users retain the reconciler's system-wins behavior.

Environment values are administrator-level configuration, not a secret vault. Worker reads return `envEditable` and only Admin/Manager callers receive `env`. Do not include credentials in screenshots or logs. Scoped users cannot write environment variables or read their values through Worker responses.

Environment changes reuse the existing container specification reconciliation: a managed Docker container is recreated and may interrupt current tasks. Persistent workspace data is retained. An accepted API update does not prove the replacement container is ready. Remote, unmanaged workers require their operator to update the process environment separately.

## Gateway probes

`POST /api/v1/workers/{name}/gateway-probe` accepts one of:

```json
{"kind":"model"}
```

```json
{"kind":"mcp","server":"github"}
```

Admin/Manager access and gateway-update authorization are required. The handler reloads the saved Worker spec and persisted Consumer credential. It never accepts a caller-supplied URL, credential, model override, prompt or tool call. It does not modify authorization.

- Model: sends one non-streaming `chat/completions` request using the saved explicit model, requesting at most 8 output tokens. This can incur a small inference charge. It checks for model choices without returning generated content.
- MCP: uses a saved Streamable HTTP endpoint on the configured gateway under `/mcp-servers/`, initializes the protocol and lists tools using the Worker credential. It never executes tools. Legacy SSE endpoints and arbitrary external MCP hosts are outside this probe's scope; upstream SSE services may be proxied behind a Streamable HTTP gateway endpoint.

Requests have a 20-second deadline, bounded response reads, and no redirects. Upstream error bodies and credentials are not returned. The result identifies the Worker Consumer and whether this gateway request passed, not whether a running Worker task or its local networking succeeded. Provider-specific gateway endpoints are reported as unsupported rather than incorrectly testing the deployment default gateway.

For a denied call, inspect the target route's Consumer allowlist and upstream configuration. Do not disable authentication or grant every Worker access merely to make a probe pass.
