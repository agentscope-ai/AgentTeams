# Harness health contract v1

The Harness owns its native readiness and model probe. The Controller never
selects a model API protocol, decodes a native response, or receives a model key
for health checking. This contract applies to OpenClaw, QwenPaw, Hermes, DeepSeek,
OpenHuman, Codex, OpenCode, Qoder and Claude Code, for Manager and Worker roles
where that Harness supports the role.

## Separate evidence

- Container liveness remains a backend lifecycle fact.
- `harness-health --readiness` reports native runtime readiness. A local API
  response does not prove Matrix sync or inference. The image's adapter owns
  its interpretation of the native readiness signal.
- `harness-health --json` performs a bounded, tool-free model data-plane request
  using the Harness's selected binding and protocol. It does not create an Agent
  conversation or Task. It does not certify tools, context limits, native SDK
  initialization or full Harness admission.
- `harness-health --configuration` returns only an opaque SHA-256 of the
  selected model binding. No credential, endpoint or model output is returned.

Each image installs its adapter at `/opt/agentteams/harness-health`. Shared
supervision consumes versioned readiness results, without native CLI/API
branches. Readiness returns version 1, status `ready`, `live` or `starting`,
and a safe `evidence` category. `matrix_verified` is true only with actual
native Matrix evidence; liveness-only adapters leave it false.

## Model request and result transport

The existing private AgentFS member scope works across Docker, Kubernetes,
Sandbox and cloud storage, without container exec or an additional inbound
port. A Harness launch starts the common transport after credentials and local
configuration are initialized. Its protocol bridge stays in the Harness repo.

The Controller writes `agents/<runtimeName>/runtime/health-request.json`:

```json
{"version":1,"id":"0123456789abcdef0123456789abcdef","expires_at":"2026-11-25T12:00:00Z"}
```

The Harness polls at 30-second intervals. It ignores unsupported versions,
malformed identities and expired requests, and acknowledges the consumed ID in
`agents/<runtimeName>/runtime/health.json`:

```json
{
  "version":1,
  "runtime":"example",
  "member":"alice",
  "configuration":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "status":"ok",
  "reason":"ok",
  "checked_at":"2026-11-25T11:59:30Z",
  "request_id":"0123456789abcdef0123456789abcdef"
}
```

The result describes **model health at checked_at**, not overall member health.
Status is `ok`, `failed` or `unknown`. Reasons are a closed set of safe categories;
raw errors, credentials, prompts and responses must not be included. Account
authentication modes without a qualified probe return `unknown/unsupported`.

Successful automatic probes run at most every five minutes, failed or
unconfigured probes every minute. A changed configuration fingerprint triggers
a new check on the next poll. Manual requests trigger a new check. The native
request is bounded to 20 seconds and adapter execution to 25 seconds. Reporting
stops when the native launch exits; an in-flight probe cannot publish after its
parent exits.

Results older than ten minutes, future-dated results, unknown schema versions,
foreign runtime/member results and malformed values are `unknown`. Images that
predate this interface also remain `unknown`; the Controller does not fall back
to direct model calls. Upgrade the Harness image with the matched Core assets
to enable reporting. A model failure is diagnostic and never restarts a member.

## Controller API

- `GET /api/v1/workers/{name}/health`
- `GET /api/v1/managers/{name}/health`
- `POST /api/v1/workers/{name}/health/probe`
- `POST /api/v1/managers/{name}/health/probe`

Read permissions follow the corresponding member resource. Probe requests
require existing administrator/Manager gateway-management permission. The
Controller resolves the storage identity from the saved resource; callers cannot
supply a URL, credential, model or executable.

POST returns **202 pending**, a request ID, its two-minute expiry and `queued: true`. This is not probe
success. Clients read GET until a result acknowledges that ID, or the request
expires. A later request can supersede a pending request for the same member.
The existing `gateway-probe` route delegates `kind=model` to this same contract;
clients must handle 202 as pending. Its gateway-managed MCP diagnostic is separate.

Manager failure notices consume only fresh Harness failure results. They retain
durable transaction identity through lost acknowledgements and restarts. Unknown
results do not masquerade as successful probes or generate model-failure notices.

Installer credential checks before a Harness exists are separate from runtime
health; they cannot certify an installed Harness.

## Validation

Every Harness runs the same contract cases against local HTTP fixtures: effective
binding and route prefix, native protocol, authentication/quota/unavailability,
redirect refusal, credential-free results, unknown configuration and independent
readiness. Core tests cover request scoping, stale/foreign/malformed reports,
unknown handling, process-exit publication, configuration changes, and durable
notice retries. These checks do not replace frozen-image or live admission tests.
