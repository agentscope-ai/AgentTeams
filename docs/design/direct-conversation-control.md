# Direct conversation execution controls

Refs #1620.

## Scope and behavior

All six Harnesses support stopping and redirecting an execution in a
two-member direct conversation. The Dashboard exposes controls only after
the known Agent advertises support. Group conversations and cancellation of
delegated Worker tasks are outside this change.

- **Stop** cancels the captured active execution and waits for the native
  stream to close. It preserves conversation history and completed actions.
- **Supplement and continue** performs the same cancellation, then enqueues
  the supplementary input in the existing conversation. Original and
  interrupted supplementary inputs are carried forward, including when
  interruption happens before the first native context checkpoint.
- A confirmed stop marks the corresponding Agent message **已停止** while
  preserving its partial content. If no response exists, the acknowledgement
  appears as its own Agent message. A stop request alone never marks a message.
- Tool cancellation follows each native runtime. Completed external
  operations are not rolled back. Remote work may require its own cancel API.

## Native execution paths and validation

| Harness | Cancellation and continuation | Focused validation |
| --- | --- | --- |
| OpenClaw | Public reply `abortSignal`; wait for dispatch/draft cleanup; reuse native conversation | Native exec cancellation for stop and steer; fresh membership rejection; 61 Matrix handler regressions |
| QwenPaw | Captured TaskTracker producer cancellation; native stream cleanup; original and supplementary inputs retained | 11 direct-control tests with the native TaskTracker and long shell tool |
| Hermes | Native `hard_interrupt`, including its tool-worker thread signal; wait for processing owner; retain session | Native local shell cancellation for stop and steer; owner, membership and stale-run checks |
| DeepSeek Harness | Keep receiving controls while DSH runs; dispose native profile and reap the owned process group; resume room session | Packaged DSH CLI, native bash, durable supplementary input and native session resume for stop and steer |
| OpenHuman | Run-scoped cancellation around the native agent bus; finish typing/draft cleanup; reap native shell process group | Pinned v0.54.0 compile; Matrix SDK lifecycle/continuation integration and real native tool cancellation |
| Codex | Native App Server `turn/interrupt`; await terminal receipt and process cleanup; continue the same native thread | Packaged App Server, real native exec, stop and steer, original/supplementary input and thread persistence |

The local model endpoints in the DeepSeek and Codex tests are deterministic
fixtures. Native tools, subprocess cancellation and session persistence are
real; these tests do not measure a live model's reasoning quality.

## Matrix contract

The Agent sends `m.room.message` notices with `com.agentteams.execution`:

```json
{"run_id":"$original-input-event","status":"running","capabilities":["stop","steer"]}
```

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
ACL and fetches current joined membership before cancelling. The Agent,
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
