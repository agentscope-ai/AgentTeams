# L1 Human authentication regression

Validated locally on 2026-11-09 using the Controller branch binary, a production
Dashboard build, and an isolated Docker stack with real Tuwunel, Higress and
MinIO. Existing user containers, networks and data were not used. Model calls
used no real provider credential; inference quality is outside this regression.

The local run completed 120 successful request/assertion checks across the
following areas. This is control-plane/API and runtime-lifecycle coverage, not
a claim that every UI interaction or every deployment backend was tested.

| Area | Exercised behavior |
| --- | --- |
| Login | Real Matrix L1/L2/L3 login; `/me`; built Dashboard login with only each Human's own username/password |
| Human | Create/read/update/delete; change permission level, assigned workers and capabilities; deny L2/L3 administrative reads |
| Team | Create/read/update/delete; administrator and worker membership; cross-team denial |
| Worker | Create/read/update/delete; persisted model/state configuration; actual QwenPaw container startup; skills endpoint |
| Runtime lifecycle | Worker restart/sleep/wake; confirm Docker stopped/running state; actual default Manager startup and restart; additional Manager CRUD |
| Models | Provider and AI-route CRUD through the built Dashboard; provider credential masking; L2 refusal; L1 unrestricted model-access scope |
| Consumers | Create/bind/delete and repeated deletion. Direct Higress deletion while referenced returns 500; Controller unbinds before deleting |
| Human files | Cross-user write/read, soft delete, trash listing, restore, purge and quota change against real object storage; scoped-user denial |
| Worker knowledge base | Read/write a `memory/` file through the running Worker's HTTP API |
| Projects/audit | Cross-team project create/read/pause/resume/replan/complete; global audit access; scoped-user refusal |
| Skills | Cross-team publish, real runtime scan (`pass`), catalog and ZIP download; scoped-user denial |
| Credential boundaries | Real Worker SA refresh obtains a Matrix token that passes homeserver `whoami`; L2/L3 denial; real AppService token rotation |
| Revocation | An existing L1 token loses administrative access after the Human is downgraded |

## Findings fixed

1. Higress returns provider credentials both in `tokens` and in
   `rawConfigs.apiTokens`. Both Controller and the legacy Dashboard response
   masker now remove the raw duplicate. Tests preserve non-secret editable
   provider configuration and token counts.
2. A Consumer referenced by AI routes cannot be deleted directly by Higress.
   The Controller delete endpoint revokes those route bindings first and does
   not delete the Consumer if revocation fails. A regression test checks order
   and failure handling; the real stack confirms deletion and idempotent retry.

## Explicit limits

- The local MinIO deployment has no cloud credential-provider sidecar. STS
  requests return 503 for both the original administrator and L1 Human. Real
  cloud STS issuance was not exercised.
- Human accounts have no persisted runtime credential entry. Calling the
  machine Matrix-refresh endpoint as a Human fails at that business boundary;
  it is not a supported Human workflow. Actual Worker credential refresh was
  exercised successfully.
- No real LLM inference, external identity provider or production deployment
  was exercised.

## Repeatable checks

Run `go test ./...` from `agentteams-controller/`. The
`TestL1AdminRegression` route-level suite covers resource CRUD, cross-team
authorization, files/trash, projects/audit, skills and credential boundaries.
`TestProviderMaskRemovesHigressRawAPITokens` and
`TestDeleteConsumerRevokesRouteBindingsFirst` preserve the real-stack findings.

The Dashboard counterpart passed 405 test files / 3756 tests, lint and production
build. Its existing wall-clock candidate-speed test initially fluctuated under
concurrent build/container load; it passed alone and in a full run with two
workers. No unrelated timing-test behavior was changed.
