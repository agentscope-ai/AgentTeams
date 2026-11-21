# Direct conversation execution controls

Refs #1620 and #1685.

## Scope and behavior

All six Harnesses support stopping and redirecting an execution in a
two-member direct conversation. The Dashboard exposes controls only after
the known Agent advertises support. Group conversations and cancellation of
delegated Worker tasks are outside this change.

- **Stop** cancels the captured active execution and waits for the native
  stream to close. It preserves conversation history and completed actions.
- **Supplement and continue** uses native steering when the Harness exposes
  it. The current execution and its tools remain active; the native runtime
  delivers the supplementary input at its supported boundary. This does not
  emit `stopping` or `stopped`. Stop remains a separate cancellation action.
- QwenPaw retains cancellation followed by continuation because its pinned
  integration has no native mid-turn injection path. Original and interrupted
  supplementary inputs are carried forward, including before the first
  native context checkpoint. Text/media support follows each native API;
  Codex image supplements and Hermes media supplements retain this fallback.
- A confirmed stop marks the corresponding Agent message **已停止** while
  preserving its partial content. If no response exists, the acknowledgement
  appears as its own Agent message. A stop request alone never marks a message.
- Tool cancellation follows each native runtime. Completed external
  operations are not rolled back. Remote work may require its own cancel API.

## Native execution paths and validation

| Harness | Cancellation and continuation | Focused validation |
| --- | --- | --- |
| OpenClaw | Native `/steer` prepared-reply queue for supplementary text; public reply `abortSignal` for Stop | Consecutive supplements leave the native exec alive; Stop waits for process cleanup; fresh membership rejection |
| QwenPaw | Captured TaskTracker producer cancellation; native stream cleanup; original and supplementary inputs retained | 24 direct-control tests with the native TaskTracker and long shell tool, including overlapping controls during cleanup |
| Hermes | Native `/steer` command and steering buffer for text; `hard_interrupt` for Stop | Ordered native steering delivery, retained source/ID, native shell cancellation and recovery regressions |
| DeepSeek Harness | Private per-process socket delivers input to native `agent.steer`; flush the session before acknowledging acceptance; dispose/reap only for Stop | Packaged DSH CLI with real bash preservation during steer, next-inference input, deduplication and same-session Stop continuation |
| OpenHuman | Upgrade to v0.57.18 and feed its native `RunQueue::Steer` at the engine boundary; retain run-scoped Stop | Matrix source receipts, native queue delivery, completion-boundary handoff and process-group cleanup |
| Codex | App Server `turn/steer` with `expectedTurnId`; `turn/interrupt` for Stop | Real App Server and shell preservation during steer; ordered controls, completion-race retention and native Stop |

The local model endpoints in the DeepSeek and Codex tests are deterministic
fixtures. Native tools, subprocess cancellation and session persistence are
real; these tests do not measure a live model's reasoning quality.

## Matrix contract

The Agent sends `m.room.message` notices with `com.agentteams.execution`:

```json
{"run_id":"$original-input-event","status":"running","capabilities":["stop","steer"]}
```

The notice also includes `steering_mode`: `native` or `restart`. This is
advisory runtime metadata; existing clients can keep using the same control
action. `native` describes supported text steering, not universal attachment
injection or immediate interruption of an in-flight tool.

Statuses are `running`, `stopping`, `stopped`, `completed`, `failed`, and
`unavailable` (the requested producer no longer exists).
`stopped` is sent after the native stream's cleanup. `completed` means the
execution ended; the conversation still contains its business result/errors.
An optional `message_id` on `stopped` identifies the affected Agent response.
Without a matching loaded response, the Dashboard renders a standalone
acknowledgement rather than marking an unrelated message.

Dashboard messages carry `com.agentteams.control` alongside the normal body:

```json
{"action":"steer","run_id":"$original-input-event","agent_id":"@agent:server"}
```

The sender comes from the authenticated Matrix event, never this metadata.
Controls bypass the normal execution queue. The channel checks the existing
ACL and fetches current joined membership before steering or cancelling. The Agent,
room, requester and execution must match. A stale Stop cannot cancel a newer
run; stale supplementary text remains an ordinary queued input.
Unmarked messages retain their existing queue/command behavior, including
approval replies and messages from other Matrix clients.

In QwenPaw, the channel captures the actual native TaskTracker producer task rather than
cancelling by a reusable chat id. Overlapping SDK channel generations share
this registry through their existing agent-scoped TaskTracker. Continuations
use the existing delivery locks and native queue; they do not create parallel
writes to the same conversation. No synthetic `/stop` command is used to
clear the normal queue.

## Verification

`plugins/tests/agentteams-matrix-channel/test_direct_control.py` covers ACLs,
fresh membership, stale targets, room isolation, consecutive supplements,
SDK channel registry sharing, and the real QwenPaw TaskTracker and shell
tool cancelling a long subprocess before the continuation starts.

Dashboard tests cover capability/member gating, late events from old runs,
input/Stop actions, and control metadata passing through the actual Matrix
client and server proxy. Screenshot fixtures must identify simulated model
results separately from real process cancellation.
