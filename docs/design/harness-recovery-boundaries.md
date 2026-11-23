# Harness recovery boundaries for 1.2.8 (#1631)

Status: candidate implementation, incomplete acceptance. These changes are in
the 1.2.8 scope; this document does not declare that version ready for release.
Use the exact source and artifact evidence for each run. A successful
configuration projection, restart or reply retry proves only that operation.

## Authority and ownership

| Record | Owner and location | Harness responsibility |
| --- | --- | --- |
| Worker settings and asset references | Controller; `agents/<workerIdentity>/runtime/runtime.yaml` under the configured tenant storage root | Read the current version at every start and project it into native inputs. Do not publish private-directory configuration as a newer authority. |
| Credential bindings | Worker identity and scoped credential files/environment | Resolve the declared bindings. Never recover a superseded token from a native session. |
| Task identity, status and execution | Task system; namespace/team/stable task ID, with Project optional | Discover only assigned recoverable Tasks, confirm ownership and claim the actual instance/process generation before inference or tools. |
| Step checkpoint and artifacts | Task execution endpoint and shared object storage | Persist and verify artifact bytes before committing versioned references, completed/remaining steps, operation/evidence and saved delivery state. ACK/progress notes do not complete a step. |
| Native conversation | Each Harness's private session store, beneath the same Worker home | Treat it as a cache. Reuse only an existing, unexpired session consistent with the latest shared checkpoint; otherwise create a fresh native conversation from authoritative context. |
| Matrix consumption and delivery | Durable event IDs, receipts, pending replies and transaction IDs | Preserve backlog and prior cursors. A native read means input committed to a session; it does not prove execution. A cursor in another Harness's format is not a common handoff record. |

The Worker home and task ID survive replacement. Native session formats are
not standardized or migrated between Harnesses. Configuration generation,
execution generation and notification outbox lease are separate identities.
An old session or a delayed callback cannot override newer shared state.

Use the [common startup and consumption boundary](member-runtime-config-contract.md#common-startup-and-consumption-boundary-1631)
for all adapters. Matrix received/read evidence and private SDK cursors are
not yet one portable execution inbox. Completing that shared boundary takes
priority over adding source-to-target special cases.

Controller configuration publication covers all six Harnesses. Complete
standardization of every native setting, user configuration write path and
asset is still open. In particular, native UI writes must not remain the only
persisted copy of a Worker setting. Existing compatibility projections remain
while their fields are migrated; clearing them or their private directories is
not a migration strategy.

## Per-Harness implementation boundary

The source paths below belong to the independent Harness repositories unless
identified as Core. “Implemented” describes a code path, not full acceptance.

| Harness | Startup configuration | Native execution/session adapter | Message and acceptance boundary |
| --- | --- | --- | --- |
| Codex | Member `runtime.yaml` reader | `src/agentteams_codex/bridge.py` uses a checkpoint-bound Task thread cache in `runtime/codex-bridge-state.json` and rebuilds missing threads. | Preserve journal events and the cursor for unrelated rooms. |
| Hermes | `src/hermes_worker/sync.py` and `bridge.py` consume the Worker transport and model record. | `src/hermes_matrix/overlay_adapter.py` checks native SessionStore rows and expiry and clears warm Agent state on a checkpoint mismatch. | Verify that the packaged native MCP client discovers Task tools. |
| QwenPaw | The `qwenpaw_worker` member loader consumes Core's resolved fields. | Core Matrix channel `task_execution.py` checks native SafeJSONSession state and checkpoint digests and shares admission across SDK reloads. | Pass the current process boot identity through MCP stdio. Saved-reply retry checks whether a native stream is still active. |
| DeepSeek Harness | The member loader materializes the `.dsh` profile and workspace. | `scripts/matrix_bridge.py` handles checkpoint-bound session markers, missing-session fallback and saved replies. | Preserve first-sync pending input and scoped Task credentials across the SDK and MCP subprocesses. |
| OpenClaw | `scripts/configure-runtime.py` projects the Worker record, validates native configuration and publishes a derived cache. | The adapter uses the shared recovery RPC for Task continuation. | Compare the native transcript against its recorded digest before reuse. |
| OpenHuman | The entrypoint projects native TOML and retains Worker-owned shared storage. | The adapter uses shared RPC for startup recovery, native tool history, session validation and saved replies. | Native MCP cancellation covers the tool process group. |

All six must implement the same admission and checkpoint semantics. Differences
above explain native integration work, not permission to weaken the common
contract. A tool server that answers `tools/list` outside the Agent does not
prove its tools are registered in the Agent's native runtime.

## Outcome-specific recovery

- Saved result with pending delivery: verify the stored bytes and execution
  ownership, resend with the same durable transaction, acknowledge the Matrix
  event and do not enter inference or re-execute the completed operation.
- In-progress Task at a verified boundary: retain `in_progress`, restore the
  latest checkpoint and artifacts, and infer/execute only remaining steps.
- Started/unknown operation: first obtain external verification evidence or
  remain pending verification. Do not clear its ID, fabricate an artifact or
  replay the effect to make the Task appear recoverable.
- Cancelled, reassigned or replaced execution: reject stale native tools and
  replies. Connection failure alone is not proof that ownership was lost.
- Return to an older Harness: keep its old session for the regression, but
  reject reuse when its checkpoint is behind. Do not import the intermediate
  Harness's native conversation.

## Evidence and remaining release gates

The [recovery acceptance summary](../development/runtime-recovery-1631.md)
records the tested revisions, upgrade coverage and remaining gaps. Keep
configuration projection, restart continuation, directed switches and
saved-reply delivery as separate checks.
