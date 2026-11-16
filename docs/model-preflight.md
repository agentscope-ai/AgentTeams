# Manager model health

The Core Controller checks a Manager's deployed configuration after container
convergence and before the welcome-message readiness gates. Checks run in a
bounded background task, independent of model inference and the Harness process.
A failed probe never changes Manager lifecycle state or recreates its container.
No dedicated Manager image or Harness submodule change is required.

For managed Harnesses, Core reads `agents/manager/runtime/runtime.yaml` and uses
the same provisioning Gateway Key supplied to the container. The selected model
and gateway URL come from that deployed contract. Codex uses `/v1/responses`;
the other currently managed Harnesses use `/v1/chat/completions`. A URL already
ending in `/v1` retains that suffix exactly once. For the compatibility OpenClaw
projection, Core resolves the selected provider from `openclaw.json`, including
its URL, key and API protocol; it does not assume the provider ID.

These checks validate the Controller-to-Harness configuration contract from the
Controller network namespace. They do not inspect SDK-owned native configuration
or prove connectivity from inside the Harness container. A `native-config` model
without an explicit desired model cannot be probed and records a configuration
failure. Arbitrary native configuration and protocols other than Chat Completions
and Responses require a future Harness-side effective-config reporting contract.

Requests have a 20-second deadline, a 256-token output budget, no streaming and
no conversation/session creation. Responses requests also disable storage.
Reasoning-only output does not pass: a nonempty user-visible text generation is
required. Core rejects HTTP 200 business errors, empty or malformed bodies and
responses larger than 64 KiB. HTTP authentication, quota, model/endpoint, rate
limit, upstream failures, DNS, certificate, timeout and network failures have
fixed diagnostic categories. Redirects are not followed with gateway credentials.

Successful results are cached for five minutes; failures are probed at most once
per minute. Pending notification delivery can retry on each reconcile. This is
also periodic recovery detection, rather than a promise of continuous model
availability. Set `AGENTTEAMS_MODEL_PREFLIGHT_ENABLED=0` on the Controller to
turn off these background checks. The existing Helm preflight Job and
`agt llm-preflight` command retain their flags and strict/warning behavior;
the CLI now rejects business errors and empty generations too.

## Persistent state and notifications

The storage object `agents/manager/model-health.json` contains only fixed status
and reason strings, timestamps, a configuration fingerprint and Matrix delivery
state. It never contains credentials, model identifiers, URLs or upstream bodies.
No raw subprocess output or response files are created. A read or write failure
leaves existing state intact and prevents unsafe notification side effects; a
fixed diagnostic is logged. Invalid state requires operator repair rather than
silently resetting deduplication.

The fingerprint includes runtime, protocol, model, URL, credential, administrator
and room. A configuration/credential/admin/room change bypasses previous failure
deduplication. Recovery records `recovered_at` and resets the failure episode.
No recovery message is sent.

Failures send a fixed `m.notice` using the Manager Matrix token into the existing
Admin DM from Manager status. The configured admin must be joined to that room;
Core does not create a room or invite anyone. A random transaction ID is persisted
before delivery and reused after failed or uncertain acknowledgement. Delivery
is recorded only after a successful Matrix response containing `event_id`.
This allows retry after Controller restart or a failed post-delivery state write
without duplicating the Matrix event. Successful delivery suppresses additional
notices for the same failure episode across restarts. State availability and the
usual single active Controller reconciliation owner are required for this guarantee.

## Validation

Run from `agentteams-controller/`:

```sh
go test ./internal/modelhealth ./internal/service ./internal/controller ./internal/matrix ./cmd/agt
```

Tests use dummy credentials and local HTTP fixtures. They cover actual Gateway
request paths/authentication, Chat Completions and Responses, `/v1` normalization,
200 business errors, empty/reasoning-only output, safe error categories, TLS,
timeout, redirect protection, durable transaction retries, configuration and
admin changes, recovery, state failure, authorized DM membership and nonblocking
Controller lifecycle. `TestModelHealthGatewayMatrixHTTP` exercises both production
HTTP clients with a lost Matrix acknowledgement and stable transaction retry.
These fixtures do not establish real model/Higress/Tuwunel or tracing acceptance.
