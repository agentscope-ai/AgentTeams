# Runtime approval cards

The Dashboard consumes `content["org.agentteams.approval"]` from the owning
runtime's Matrix account. Approval controls remain inside the chat message.
The original command and risk details remain visible after the runtime reports
approval, rejection, cancellation or expiry.

## Contract

```json
{
  "version": 1,
  "runtime": "hermes",
  "request_id": "native-request-id",
  "context_id": "opaque-request-context",
  "reply_format": "com.agentteams.answer",
  "status": "pending",
  "tool_name": "terminal",
  "parameters": "{\"command\":\"printf probe\"}",
  "risk_details": "A tool needs your approval",
  "expires_at": 1790000000000
}
```

`runtime` is `openclaw`, `qwenpaw`, `hermes`, `deepseek-harness`, `openhuman` or
`codex`. Terminal statuses are `approved`, `rejected`, `cancelled` and `expired`.
Hermes, DeepSeek, OpenHuman and Codex require a context identifier. Receipts must
match the request's room, runtime sender, request ID and context. A successful
Matrix send only means the decision was submitted; it does not mean the runtime
accepted it or that the tool completed.

## Native adapters

New requests advertise `reply_format: "com.agentteams.answer"` and provide a
nonempty `context_id`. Dashboard and Mobile Client send the same content for
every Harness:

```json
{"com.agentteams.answer": {
  "kind": "tool_approval",
  "confirmation_id": "native-request-id",
  "context_id": "opaque-request-context",
  "agent_id": "@requesting-agent:server",
  "answer": "approved"
}}
```

`answer` is exactly `approved` or `rejected`; it grants at most the native
one-shot decision. Use the real Matrix sender as the decider. `actor` is display
provenance and never authority. Validate the owning account, room, pending
request, context, expiry and existing native sender policy before settling the
waiter. A malformed structured control must not fall back to executing its
body as a command or model instruction. Native translation belongs in the
Adapter. Requests without `reply_format` retain the legacy client transport
below for rolling upgrades; native commands remain supported for existing IM
clients. No approval policy or persistent permission default changes.

| Harness | Execution boundary | Reply transport |
| --- | --- | --- |
| OpenClaw 2026.10.9 | Native exec approval manager and Matrix approval adapter | Native `/approve ID allow-once` or `/approve ID deny` |
| QwenPaw 2.2.2.post1 | Native ApprovalService future and command handler | Native `/approval approve ID` or `/approval deny ID` |
| Hermes v0.23.0 | Native gateway approval queue and settle observer | Request-bound `/agentteams-approval` control |
| DeepSeek 0.1.1-rc.2 | Native ApprovalService answerer and committed audit event | Request-bound `/agentteams-approval` control |
| OpenHuman v0.57.18 | Native ApprovalGate, turn-scoped Matrix event surface and one-shot waiter | Request-bound `/agentteams-approval` control |
| Codex 0.143.0 | Native App Server command/file approval RPC | Native `/approve ID CONTEXT` or `/reject ID CONTEXT` |

Transport commands are sent by card buttons and rendered as compact activity,
not exposed as instructions the user must type. The requesting user or the
configured operator admin can answer; each adapter retains its channel's access
checks. Unknown, stale or mismatched controls cannot grant another request.

OpenClaw and OpenHuman patches are applied against the pinned upstream sources
during image builds. Revalidate those patches and native tests when changing the
pins. OpenHuman's Matrix turns register a turn-scoped approval surface with the
native ApprovalGate, use the actual autonomy configuration and wait before tool
execution. Cancellation drops the
waiter and sends a cancellation receipt. Existing permission defaults are not
changed by these adapters.

## Questions

DeepSeek and Codex native questions emit `org.agentteams.run` confirmation blocks
with question IDs and options. The Dashboard returns `com.agentteams.answer`
with `confirmation_id`, readable `answer`, and lossless
`responses: [{id, selected, custom?}]`. Runtime adapters validate the IDs and
options before resuming the native request. Natural-language heuristics alone
do not establish that a runtime waiter resumed.

TeamHarness `request_attention` emits the same confirmation card, retaining
its human-readable `ATTENTION_*` body for older clients. Its stable question ID,
recipient roster, options, deadline and accepted answer are stored with the
Task attention record. The shared Matrix intake commits the decision with an
ETag before admitting a continuation input. The first valid answer wins;
replay of the same Matrix event is idempotent, while a different late answer
cannot replace it. An answer to a business `kind=approval` record is recorded
without granting the existing human-only approval or unfreezing the Task.

New question cards advertise `reply_format: "com.agentteams.answer"` and
`status: "pending"`. Answers carry `kind: "ask_user"`, `agent_id`, the exact
`confirmation_id` and `responses: [{id, selected, custom?}]`. The owner emits a
confirmation receipt with the same ID and `status: answered | cancelled |
expired`. A send is only submission; clients retain pending state until an
owner receipt or the request deadline. Receipts from another account or room
cannot settle the request, including when history arrives after the receipt.

## Turn and session lifecycle

Both HITL flows expose `waiting_user`, separately from running, finished and
idle. A live process or typing heartbeat does not override a pending human
request. Dashboard derives this state from the shared inbox; fallback Task
records expose `session_state`. An ordinary question or its retry must not
escalate a new Task into blocked/frozen. Historical text requests and business
approval authority retain their previous policy.

Native Ask User parks the original Turn at the question; native Tool Approval
parks it before tool execution. Neither waiting path continuously calls the
model. A valid answer resumes that Turn when the native SDK supports it.
Fallback questions may finish their native Turn: the next admitted Turn reads
the persisted decision and continues the same work. Restart must restore a
compatible native waiter or invalidate the old request and recover from the
durable checkpoint. Never apply an old approval to a new tool invocation or
replay a tool whose side effect is unknown.

## Verification

Test native approval, rejection, timeout/cancellation and stale/wrong-user
responses, then verify that only approval permits tool execution. Dashboard
tests cover all six contracts, history-batch receipts, sender/context binding,
failed sends, and preservation of the original card. Hermes and DeepSeek native
service checks run in their independent repositories' `make smoke` targets.
The OpenClaw patch includes native transport tests; OpenHuman includes native
tool-loop, one-shot authorization and HTTP request/receipt tests.
