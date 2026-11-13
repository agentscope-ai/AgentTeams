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

| Harness | Execution boundary | Reply transport |
| --- | --- | --- |
| OpenClaw 2026.10.9 | Native exec approval manager and Matrix approval adapter | Native `/approve ID allow-once` or `/approve ID deny` |
| QwenPaw 2.2.2.post1 | Native ApprovalService future and command handler | Native `/approval approve ID` or `/approval deny ID` |
| Hermes v0.23.0 | Native gateway approval queue and settle observer | Request-bound `/agentteams-approval` control |
| DeepSeek 0.1.1-rc.2 | Native ApprovalService answerer and committed audit event | Request-bound `/agentteams-approval` control |
| OpenHuman v0.54.0 | Native tool-call loop, task-local Matrix approval context and one-shot waiter | Request-bound `/agentteams-approval` control |
| Codex 0.143.0 | Native App Server command/file approval RPC | Native `/approve ID CONTEXT` or `/reject ID CONTEXT` |

Transport commands are sent by card buttons and rendered as compact activity,
not exposed as instructions the user must type. The requesting user or the
configured operator admin can answer; each adapter retains its channel's access
checks. Unknown, stale or mismatched controls cannot grant another request.

OpenClaw and OpenHuman patches are applied against the pinned upstream sources
during image builds. Revalidate those patches and native tests when changing the
pins. OpenHuman's channel dispatch previously passed no ApprovalManager and its
non-CLI loop auto-approved configured requests; Matrix turns now use the actual
autonomy configuration and wait before tool execution. Cancellation drops the
waiter and sends a cancellation receipt. Existing permission defaults are not
changed by these adapters.

## Questions

DeepSeek and Codex native questions emit `org.agentteams.run` confirmation blocks
with question IDs and options. The Dashboard returns `com.agentteams.answer`
with `confirmation_id`, readable `answer`, and lossless
`responses: [{id, selected, custom?}]`. Runtime adapters validate the IDs and
options before resuming the native request. Natural-language heuristics alone
do not establish that a runtime waiter resumed.

## Verification

Test native approval, rejection, timeout/cancellation and stale/wrong-user
responses, then verify that only approval permits tool execution. Dashboard
tests cover all six contracts, history-batch receipts, sender/context binding,
failed sends, and preservation of the original card. Hermes and DeepSeek native
service checks run in their independent repositories' `make smoke` targets.
The OpenClaw patch includes native transport tests; OpenHuman includes native
tool-loop, one-shot authorization and HTTP request/receipt tests.
