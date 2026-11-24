# OpenCode Harness candidate

The native adapter lives in
[agentteams-harness-opencode](https://github.com/agentteams-group/agentteams-harness-opencode).
The first candidate supports Worker and Leader roles with OpenCode 2.4.1.
Manager, encrypted Matrix rooms, Code Mode and nested subagents are not supported.

Use `spec.runtime: opencode` and an explicitly built `spec.image`. Alternatively,
configure `AGENTTEAMS_OPENCODE_WORKER_IMAGE` on the Controller. There is deliberately
no default release image until the candidate passes the
[Harness admission requirements](../design/harness-integration-guide.md).
Helm exposes the optional `worker.defaultImage.opencode.repository` and `.tag`.
The shared publication workflow accepts explicit `runtime=opencode` with a fixed
Harness SHA; `runtime=all` retains the existing release set until admission passes.
Use `make build-opencode-worker OPENCODE_HARNESS_DIR=/path/to/checkout` for a local build.

The adapter consumes the existing member runtime record and public Task schema;
Core owns authorization, coordination, execution generations, files and delivery.
Harness-native tools register against the public recovery journal. Unknown
effects remain pending verification. Startup discovers assigned Tasks without
requiring a new Matrix wake-up. Native sessions are optional caches and are not
converted when switching Harnesses.

Local SDK tests use a deterministic model fixture. They do not establish the
required 10 simple and 10 complex real-model runs, bidirectional switching,
return-switch behavior, or Collector-visible Trace acceptance. Keep these gates
open until evidence has been recorded for the frozen Core/Harness candidate.
