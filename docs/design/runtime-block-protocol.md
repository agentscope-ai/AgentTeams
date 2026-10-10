# Optional Runtime Block Protocol (`org.agentteams.run` v1)

**Status**: optional cross-component contract. The dashboard-side parser and
normalizers are shipped in the dashboard repository; adoption by runtimes is
incremental and explicitly opt-in. This section is the controller/runtime-side
reference requested in #1312.

**Source of truth**: the dashboard-side field spec
(`agentteams-dashboard/docs/INTERFACES.md`, section "运行时块协议
(`org.agentteams.run` v1)"; implementation: `src/lib/a2ui/protocol.ts`,
parser: `src/lib/a2ui/parser.ts`). Where this document and the dashboard
implementation disagree, the dashboard implementation wins; the two are kept
aligned via the tracking issue (agentscope-ai/AgentTeams#1312).

## 1. What it is

Runtimes deliver a turn's streamed content to rooms as Matrix messages.
Without structure, the dashboard derives presentation (thinking folds,
tool-call cards, confirmation cards, run sentinels) from body-text
heuristics. This protocol gives a runtime an **opt-in typed alternative**:
attach a structured envelope under the message content key
`org.agentteams.run`. Messages without the key — or with an envelope that
yields no valid blocks — fall back to the existing heuristics, so a message
is always rendered either as structured blocks or from its body. (An
individual block that fails validation is skipped, not repaired — see §4.2.)
Adoption is therefore safe to roll out incrementally, per runtime.

## 2. Envelope

Matrix message `content['org.agentteams.run']`:

| Field | Type | Required | Notes |
|---|---|---|---|
| `version` | `"1"` \| `"0"` \| absent | no | `"0"`/absent = legacy lenient shape; `"1"` = normalized shape. Any other value = unknown version → the parser returns `undefined` and the caller falls back to the text heuristics |
| `run_id` | string | no | correlates the messages of one run (reserved in v1) |
| `step_id` | string | no | current step within a run (reserved in v1) |
| `blocks` | Block[] | yes | see below |

## 3. Blocks (`type`-discriminated union)

The table below is the **v1 wire input** shape. The v1 parser hands each
block to its normalizer whole, and the normalizers read the fields at the
block's **top level** — do not nest them under a `payload` key on the wire.
A nested-`payload` tool block is rejected by the v1 normalizers; the flat
form is what the shipped parser tests exercise.

| `type` | Wire fields (top level) | Meaning |
|---|---|---|
| `text` | `text: string`; `isStreaming?` | visible text fragment |
| `thinking` | `content: string`; `isStreaming?` | reasoning fragment (rendered as a collapsible card) |
| `tool_call` | `tool_name: string` (required, non-empty); `arguments: object` (absent → `{}`); `status: 'pending' \| 'running' \| 'succeeded' \| 'failed'` (absent or other → `running`); optional `tool_call_id: string` (non-empty), `started_at` / `finished_at` (epoch ms), `result` (any value) | tool-call card; `tool_call_id` is the stable id for deduplication across revisions/replays |
| `confirmation` | `tool_name: string` (required, non-empty); `confirmation_id: string` (required, non-empty); optional `parameters: string`, `external_files: string`, `expires_at` (epoch ms) | tool-guard approval card; `confirmation_id` links the approval/deny reply |
| `error` | `kind: 'cancelled' \| 'failed' \| 'quiet'` (required); `title: string` (required) | run-closing sentinel; any other `kind` value rejects the block (§4.2) |

**Normalized output shape** (what downstream consumers see — not wire
input): each accepted block becomes `{ type, payload }`, where `payload` is
the normalized object. For example, the flat v1 wire block
`{ type: 'tool_call', tool_name: 'read_file', arguments: {}, status: 'running' }`
is emitted as `{ type: 'tool_call', payload: { tool_name: 'read_file',
arguments: {}, status: 'running' } }`.

**Legacy shape** (`version: "0"` / absent): lenient pass-through — a block's
top-level `content`, `text`, `payload` (object, copied as-is), and `messages`
are copied to the output with no field validation. The nested-`payload` field
style belongs to this legacy shape and to the normalized output, not to v1
wire input.

## 4. Versioning & fallback semantics

1. `resolveProtocolVersion`: `"1"` → v1; absent / `null` / `"0"` → legacy;
   anything else → unknown. The parser returns `undefined` — which is the
   **only** condition under which the caller falls back to the body-text
   heuristics — when: the envelope is absent or not an object; the version is
   unknown; `blocks` is not an array; or the envelope yields **zero** valid
   blocks.
2. v1 normalization: absent optional fields get safe defaults (e.g. `status`
   defaults to `running`); unknown fields are stripped so the payload stays
   serializable. A block missing a required field (a `confirmation` without a
   non-empty `confirmation_id`, an `error` with a `kind` outside
   `cancelled` / `failed` / `quiet`, a `tool_call` without a non-empty
   `tool_name`) is **skipped, not repaired** — there is no per-block fallback
   to the body text. If any valid blocks remain, the caller renders exactly
   those and does **not** re-parse the body: e.g. a valid `text` block plus a
   `confirmation` without `confirmation_id` renders the text only, and any
   tool-guard approval text sitting in `body` produces no card.
3. Unknown block types are skipped silently; an unknown **envelope** version
   falls back wholesale. Forward-compatibility promise: a newer-version
   envelope renders under the body heuristics on an older dashboard — the
   message itself always renders; only structure is lost.

## 5. Adoption guide (runtime adapters)

1. Emit **only** what `version: "1"` defines; do not rely on extra fields
   (unknown fields are stripped, so any hidden dependency on them is lost).
2. Start with the block types closest to what the adapter already emits —
   typically `thinking`, `tool_call`, `error` (exactly the shapes the legacy
   heuristics approximate).
3. Keep each message's visible `body` human-readable: it is the fallback
   rendering (consulted only when the envelope yields zero valid blocks) and
   the human audit trail in clients that do not parse the envelope. When
   valid blocks exist the dashboard renders those and does not re-parse the
   body, so do not rely on body text to carry structure the blocks should
   carry.
4. Keep `tool_call_id` stable across revisions of the same call; keep
   `confirmation_id` stable across the request/reply pair.
5. Adoption is per-runtime and per-block-type; there is no all-or-nothing
   requirement.

## References

- Field spec & parser: `agentteams-dashboard/docs/INTERFACES.md`
  (section "运行时块协议 (`org.agentteams.run` v1)"), `src/lib/a2ui/protocol.ts`.
- Tracking & alignment: agentscope-ai/AgentTeams#1312.
