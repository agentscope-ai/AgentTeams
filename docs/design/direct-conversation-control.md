# Direct conversation execution controls

Refs #1620.

## Scope and behavior

The QwenPaw Matrix channel supports stopping and redirecting an execution in
a two-member direct conversation. The Dashboard exposes controls only after
the known Agent advertises support. Group conversations and cancellation of
delegated Worker tasks are outside this change.

- **Stop** cancels the captured active execution and waits for the native
  stream to close. It preserves conversation history and completed actions.
- **Supplement and continue** performs the same cancellation, then enqueues
  the supplementary input in the existing conversation. Original and
  interrupted supplementary inputs are carried forward, including when
  interruption happens before the first native context checkpoint.
- Tool cancellation follows QwenPaw's native runtime. Completed external
  operations are not rolled back. Remote work may require its own cancel API.

## Matrix contract

The Agent sends `m.room.message` notices with `com.agentteams.execution`:

```json
{"run_id":"$original-input-event","status":"running","capabilities":["stop","steer"]}
```

Statuses are `running`, `stopping`, `stopped`, `completed`, `failed`, and
`unavailable` (the requested producer no longer exists).
`stopped` is sent after the native stream's cleanup. `completed` means the
execution ended; the conversation still contains its business result/errors.

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

The channel captures the actual native TaskTracker producer task rather than
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
