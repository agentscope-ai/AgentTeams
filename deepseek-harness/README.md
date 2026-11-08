# DeepSeek Harness Worker runtime

This directory packages DeepSeek Harness as an AgentTeams-managed Worker. The Controller still owns the Worker lifecycle and projects `runtime.yaml`; the image translates that contract, including `desired.model.model` and its gateway route, into a DSH profile, listens for Matrix messages, resumes one DSH session per room, and posts text and file results back to the same room.

The image is pinned to `@deepseek-ai/dsh@0.1.1-rc.2` and uses the TeamHarness adapter in `plugins/teamharness/adapters/deepseek-harness`. This runtime is experimental until the upstream DSH dependency reaches a stable release and passes the same compatibility suite.

`package.json` also fixes the Cordis service dependencies to the versions in the tested DSH source snapshot `b150a551b8d465e31e418e1b2eaf5e79bbb7d28e`. The CLI's compatible dependency ranges otherwise install newer service APIs that rc.2 cannot boot. When changing `DSH_VERSION`, revalidate this dependency set with the packaged headless smoke test as well as the live checks below.

The Dockerfile applies one guarded rc.2 compatibility fix: empty tool-call ID continuation fragments must not replace the initial non-empty ID. Without it, a completed tool turn can save an invalid session and fail on the next message. The packaged smoke covers a real read tool, this SSE fragment pattern, and a second process restoring that tool history. A changed upstream implementation fails the build rather than silently skipping the fix.

The current bridge does not support Matrix E2EE. It fails fast with a clear
error when `AGENTTEAMS_MATRIX_E2EE=1`; use unencrypted Matrix rooms with this
experimental runtime.

The adapter image has its own version (`v0.1.0` initially) instead of inheriting
the AgentTeams core release tag. Build the image from the repository root:

```bash
make build-deepseek-harness-worker
make push-deepseek-harness-worker DEEPSEEK_HARNESS_WORKER_VERSION=v0.1.0
```

Production multi-architecture images are published by
`.github/workflows/build-deepseek-harness.yml`, either manually or from a
`deepseek-harness-vX.Y.Z` tag. Core `vX.Y.Z` and RC workflows do not rebuild or
retag this runtime; installers and Helm pin a tested runtime version explicitly.

Create a Worker with the runtime explicitly selected:

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: dsh-worker
spec:
  runtime: deepseek-harness
  model: deepseek-v4-flash
```

The test coverage is split by boundary:

- `docker run --rm --entrypoint python3 -v "$PWD/deepseek-harness/tests/headless_smoke.py:/tmp/headless_smoke.py:ro" <worker-image> /tmp/headless_smoke.py` boots the packaged CLI and adapter against a local model endpoint, without real credentials. CI runs it after building the image to catch incompatible transitive dependencies that a config dump does not exercise.
- `python -m unittest discover -s deepseek-harness/tests -p "test_*.py"` checks room-scoped session state, text/image/file handling, safe Workspace paths, outbox delivery, event deduplication, retry state, useful error reporting, and gateway URL normalization.
- `node --test deepseek-harness/tests/*.test.mjs` checks that each execution retry receives a distinct, restart-stable DSH message ID.
- `plugins/teamharness/adapters/deepseek-harness/smoke.ps1` checks package install/remove, role-specific prompts and Skills, and TeamHarness MCP registration against the pinned DSH checkout.
- `model-smoke.ps1` makes a real model call and verifies that DSH sees the identity injected from `runtime.yaml`.
- `live-services-smoke.ps1 -WorkerName <existing-worker>` sends and reads back a real Matrix event and pushes, stats, reads back, and removes a real MinIO object.
- `deepseek-harness/tests/k8s_lifecycle_smoke.ps1 -WorkerName <dsh-worker>` verifies a Controller-created Worker, a Matrix-to-model-to-Matrix turn, persisted bridge state, Pod replacement, and a second turn without replay.
- `deepseek-harness/tests/team_e2e_smoke.ps1 -Image <image>` creates a real DSH Leader/Worker Team and verifies Team role context, Team-room delivery, cross-message continuation after Leader Pod replacement, duplicate suppression, and a real Matrix file → Workspace inbox → DSH outbox → Matrix file round trip.

The bridge processes events sequentially per Worker. `runtime/matrix-bridge-state.json` stores the Matrix cursor, room-to-session mapping, bounded completed-event history, retry count, and any answer waiting for delivery. DSH JSONL sessions and bridge state are mirrored to object storage before an event is acknowledged. Each failed execution attempt gets a distinct deterministic DSH message ID, so the next attempt runs a new turn while a restart within that attempt remains identifiable. Matrix transaction IDs are derived from the source event, so retrying a reply is idempotent. Replies to another Agent automatically carry an `m.mentions` target even when the model omits its Matrix ID; the bridge marks that automatic hop so two Agents do not bounce replies forever. Incoming files are limited to 25 MiB by default and are written under `Workspace/inbox/`; only files created or changed under `Workspace/outbox/` during the turn are returned.
