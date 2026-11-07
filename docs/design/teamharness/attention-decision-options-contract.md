# Attention Decision-Options Contract And Upgrade Path

Pinned by PR #1506 (2026-11-06). Machine-readable twin:
`plugins/teamharness/contracts/attention.json` (`decision_options` section,
v1.1.0) — this document is the human-facing statement of the same contract;
when they disagree the JSON contract (read by tests) wins and this document
must be fixed.

## 1. The switch and its deployment default

The no-options guard is controlled by the protocol switch:

| | |
|---|---|
| Config key | `TEAMHARNESS_RUNTIME_CONFIG` (JSON or YAML), `protocol.enforce` |
| Env override | `TEAMHARNESS_PROTOCOL_ENFORCE` (`1/true/yes/on`) — wins when set |
| **Deployment default** | **`false` (off)** — an unset key is pre-W2 behaviour |

With the switch **off**, `request_attention(kind=decision)` may be raised
without `options[]` (pre-W2 behaviour, byte-for-byte backward compatible) and
the answer arrives as free text. With the switch **on**, every **new**
`kind=decision` raise must carry `options[]` (at least one `{id,label}`
entry, at most 10); the rejection happens at raise time, before any write,
notification or signal event, so no half record is ever persisted.

Flipping the switch on enforces new raises only: already-open records keep
their shape and stay answerable.

## 2. Behaviour matrix

| switch | raise (new record) | resolve (explicit close) |
|---|---|---|
| **off** | decision without `options[]`: **ok** (backward compat); with `options[]`: ok | exempt — any open record closes; free-text or option-id answer; no protocol fields written |
| **on** | decision without `options[]`: **rejected** (`request_attention kind=decision requires options[]`); with `options[]`: ok | exempt — any open record closes, including legacy no-options records |

The `resolved=true` close is exempt in **both** switch states: it answers the
record it closes, and a record's options (when present) were fixed at its own
raise time. `kind=approval` is unaffected by the options guard in both
states; its close additionally requires the trusted human runtime role
(`approval_requires_human`) while the switch is on.

## 3. Legacy records: answer in place, no migration

A **legacy record** is an unresolved `kind=decision` record without
`options` — raised with the switch off, or before this guard existed.

Strategy: **answer in place, no migration.**

- The human answers in free text through the normal explicit-close path
  (the record has no options to echo).
- The record is **never** rewritten, force-migrated, options-backfilled, or
  marked legacy. No new `resolvedText` field and no legacy marker are
  introduced.
- The free-text answer is recorded as **resolve provenance** on the record
  itself at close time: `resolved` / `resolvedVia` / `resolvedBy` /
  `resolveEvidence` / `resolvedAt` (plus the server-written
  `resolvedActor` marker while the switch is on).

A deployment that flips the switch on mid-flight therefore needs no data
migration: legacy records simply keep their shape until a human answers
them, and the answer path is identical for legacy and option-carrying
records (only the echoed content differs: free text vs option id).

## 4. The two resolve surfaces

The runtime's single choke point is
`taskflow.request_attention(resolved=true, resolvedVia=...)`; both surfaces
converge on it with a surface-specific `resolvedVia`.

### 4.1 Matrix (task room)

The human answers in the task room. The room reply or command closes the
record with:

- `resolvedVia = im_reply` (a reply to the raised message) or `im_command`
  (an explicit command),
- `resolvedBy` (the answering human),
- `resolveEvidence` citing the room message (message id / text); the answer
  text itself stays in the room.

While the switch is on, closing a `kind=approval` record additionally
requires the trusted human runtime role; closing a `kind=decision` record
(legacy or option-carrying) needs no trusted role — the provenance is
recorded, authority rules only bind the approval channel.

### 4.2 Dashboard (TH-4)

`POST /api/v1/projects/{id}/attention/{taskId}/resolve`
(landing in PR #1515; contract in
`方案与设计/AgentTeams/下版本1.2.6-1.2.7/11-团协线-TeamHarness-千锻-Matrix/06-前端契约-20261103.md` §4):

```
Body: {"resolution": "<option id 或自由文本>", "comment"?: "≤500", "evidence"?: {…} ≤4KB}
```

- `resolution` is an **option id** or **free text**. A record that carries
  `options` rejects a text that matches no option id
  (`resolution_not_an_option`, 400); a **legacy record can only be free
  text** (it has no option ids).
- Free text requires `evidence: {"freeform": true}`; the close is recorded
  with `resolvedVia = "dashboard"`.
- Errors: `attention_not_pending`(409) / `not_a_human_member`(403) /
  `resolution_not_an_option`(400) / `attention_already_resolved`(409) /
  `task_changed_during_resolve`(409).

Until TH-4 lands, the dashboard close is not exercisable end-to-end; the
shared runtime choke point (this document's §4 Matrix/`resolvedVia`
semantics) is what the tests in `test_protocol_guard.py` cover for both
surfaces.

## 5. Event identity (attention_raised, #1506)

`SIG-ATTENTION` (emitter `py:attention_raised`) is Python-only — the Go
controller emits no attention event. Its event id fingerprints the raised
record's **stable identity** — the creation-time fields
`{kind, attempt, question, options, suggested, timeoutAt, expiresAt}` as
canonical JSON (sorted keys, compact separators, raw unicode) — **not** the
post-write meta bytes: the S4a notification-outcome write
(notification_log, plus `eventId` once a send succeeds) rewrites the record's
meta file after the emit, so post-write bytes hash differently on a retry of
the pending record and would drop a duplicate event object for one logical
raise. The identity is fixed at record creation and never rewritten, so:

- a same-payload retry of the pending record (first send failed, no
  `eventId` yet) re-lands on the **same** event id (one object, consumer
  dedupe by `event_id`),
- a fresh raise of the same kind (`attempt + 1`) hashes a **distinct** id.

See `contracts/signal-mapping.json` `id_rule` (attention-family paragraph)
and `test_signal_emission.py::test_attention_failed_notification_retry_is_one_event`.
