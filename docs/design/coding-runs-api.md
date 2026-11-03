# Coding Runs API (Read-Only Coding-Task Transcript Visibility)

A delegated coding task — a one-shot run of a coding CLI (Qwen Code / OpenCode
/ …) that a Manager, team Leader, or authorized Worker launched in a worker via
the `coding-cli` skill (#1342) — tees its full stdout (reasoning, tool calls,
file edits, exit reason) to a per-task log:

```
/root/agentteams-fs/shared/tasks/{task-id}/workspace/coding-cli-logs/run-{YYYYMMDD}-{HHMMSS}.log
```

…and `mc mirror`s the workspace to object storage. **The transcript already
exists and is already in MinIO**, but there was no read path. The Controller
therefore serves the coding-run transcripts **read-only**, mirroring the
worker chats proxy (`worker-chats-api.md`) — same thin, byte-transparent,
`404-not-403` discipline, applied to coding runs instead of agent
conversations. The controller does not mount the worker filesystem, so it reads
through `oss.StorageClient` (mc), exactly like the project task endpoints —
which is why this works in **both embedded and K8s** modes (it does not proxy a
per-worker console port).

**Read-only by design.** There is no create / delete / tail-stream route: the
run is already finished (one-shot), and the log is immutable once the CLI exits.

**v2 — list metadata + structured detail.** A run's *outcome* — which CLI ran,
which worker it served, whether it succeeded / timed out / was blocked, how
long it took, what prompt it was given — cannot be told from the transcript
without reading every log, and the prompt does not live inside the workspace at
all. `run-coding-cli.sh` therefore appends one JSON line per run to
`coding-cli-logs/runs.jsonl` (write side below); the list reads that index once
per task, and `?format=json` serves the structured envelope. Both are
**additive**: the v1 list fields and the byte-transparent default detail are
unchanged, and a run with no `runs.jsonl` record (every run recorded before
v2) degrades to the v1 fields rather than failing. The list never reads a log
body — one index read per task, not N per run.

**Every recorded outcome is readable.** A transcript is the unit this API
enumerates and serves, so the writer never records an outcome without one: a
**blocked attempt** (a conflicting `.processing` marker, or a CLI missing from
PATH) leaves a transcript carrying the refusal diagnostic — the attempt shows
up in the list as `status: blocked` with its reason, and the detail serves the
diagnostic text like any other run. Index-only records (metadata with no
transcript object) are not expected from the wrapper and would stay invisible
here; that is why the writer's contract is "one index line ⇔ one transcript".

## Routes

| Route | Storage read | Returns |
|-------|-------------|---------|
| `GET /api/v1/coding-runs` | `ListObjects` over the caller's visible `…/coding-cli-logs/` prefixes, plus one `GetObject` of that task's `runs.jsonl` | `list[{task_id, run, team, cli, worker, status, exit_code, started_at, ended_at, log_bytes}]` — the runs the caller may read (bounded by a 200-run cap; the metadata keys are omitted when unrecorded) |
| `GET /api/v1/coding-runs/{taskID}/runs/{run}` | `GetObject …/coding-cli-logs/{run}` | the raw `.log`, byte-transparent (`text/plain`) — unchanged |
| `GET /api/v1/coding-runs/{taskID}/runs/{run}?format=json` | as above, plus `…/coding-cli-logs/runs.jsonl` and `…/coding-prompts/{prompt}` when present | the structured envelope `{task_id, run, team, meta, input, output}` |

## List item (v2)

```json
{
  "task_id": "t1", "run": "run-20261015-101500.log", "team": "alpha-team",
  "cli": "qwen",              // omitempty — absent when unrecorded
  "worker": "sysdev-coder",   // the worker this task served (delegator-supplied)
  "status": "ok",             // ok | failed | timeout | blocked
  "exit_code": 0,             // 0 is a legal value and shows; unknown omits
  "started_at": "2026-10-15T10:15:00Z",
  "ended_at": "2026-10-15T10:16:12Z",
  "log_bytes": 12345
}
```

- **Source**: the task's `…/coding-cli-logs/runs.jsonl`, mapped by run name
  (one extra `GetObject` per task directory — never a per-run log read).
- **No fabricated timestamps**: `started_at` / `ended_at` come from the record
  only, where the wrapper writes UTC (`date -u`). The run **filename** is a
  container-LOCAL wall-clock stamp (`date +%Y%m%d-%H%M%S`) whose timezone is
  unknowable from the controller, so a record-less run reports **no**
  `started_at` rather than a local time asserted as UTC. (Do not add a
  filename fallback: it is wrong by the container's UTC offset.)
- **Tolerance**: a missing `runs.jsonl`, a bad line, or a line without a run
  name is skipped — a malformed index never fails the list.
- `runner` (the run's own container identity) is deliberately **not** in the
  list; it appears in the detail `meta`.

## Detail `?format=json`

```json
{
  "task_id": "t1", "run": "run-20261015-101500.log", "team": "alpha-team",
  "meta": { "cli": "qwen", "worker": "sysdev-coder", "runner": "sysdev-lead",
            "status": "ok", "reason": "", "exit_code": 0, "timeout_secs": 3600,
            "started_at": "…", "ended_at": "…", "log_bytes": 12345,
            "prompt_file": "…", "prompt_sha256": "…" },
  "input":  { "prompt_summary": "…first 200 chars…", "prompt_sha256": "…",
              "prompt": "…full text when mirrored…", "prompt_available": true },
  "output": { "transcript": "…the raw log…", "status": "ok", "exit_code": 0 }
}
```

- `output.transcript` is the same bytes the default route returns verbatim; the
  envelope adds no timestamp or wrapper of its own.
- `meta.reason` is the record's machine-readable outcome qualifier — it names
  the refusal on the blocked paths (`marker-conflict`, `cli-missing`) and the
  other write-side reason codes, and is absent when the outcome needs none. A
  consumer can render `status` + the transcript for the human, and `reason` for
  the machine.
- `input.prompt` is read from `…/{taskID}/coding-prompts/{basename}` — the
  prompt lives *beside* the workspace, and is only readable because the mirror
  rule below pushes it on every outcome. The basename comes from the record's
  `prompt_file` when present, else from the run name.
- **Degradation**: no `runs.jsonl` record → `meta` is an empty object and the
  transcript still serves; no prompt object → `prompt_available: false` with
  `prompt_summary` / `prompt_sha256` still present from the record. Neither is
  an error — the route never 500s on missing metadata.
- Any `format` value other than `json` (including none) keeps the v1
  byte-transparent `text/plain` response.

- **Data layout (dual-prefix)**, read via `oss.StorageClient`:
  - standalone task: `shared/tasks/{taskID}/workspace/coding-cli-logs/run-*.log`
  - team task: `teams/{team}/shared/tasks/{taskID}/workspace/coding-cli-logs/run-*.log`
- **Path segments are validated before any storage read**: `taskID` must pass
  `isSafeTaskID` (alphanumeric, no leading `.`/`_`/`-`, no `/`) and `run` must
  match `run-\d{8}-\d{6}\.log` — neither can carry path traversal or address a
  file other than a run transcript. Failures return `400`.
- **Read-only** (`text/plain`, no body cap — a transcript can be large; the
  project task artifact route has the same posture).

## Authorization

Two layers, the standard AgentTeams split (authorizer admits the *action*; the
handler enforces the *scope*).

### Authorizer layer (`internal/auth/authorizer.go`)

`GET /api/v1/coding-runs` is registered under `ActionGet, "coding-runs"`.

| Role | `get` / `list` coding-runs |
|------|---------------------------|
| L1 admin | allowed (full access) |
| manager SA | allowed (full access, same as the project endpoints) |
| team leader | **allowed** (handler scopes to own team) |
| L2 human | **allowed** (handler scopes to `accessibleTeams`) |
| worker | **denied** (workers run the CLIs; they do not read transcripts through the API) |

The `coding-runs` case is explicit in `authorizeTeamLeader` / `authorizeHuman`
(read allowed, everything else denied). Without it, L2 callers fall through to
the authorizer's `default: deny` and never reach the handler — the handler's
team-scope logic would be dead code and real L2 requests would `403`.

### Handler layer (team scope + W4, `CodingRunsHandler`)

- **L1 / manager** read every prefix: all teams **plus** the global standalone
  `shared/tasks/` prefix.
- **L2 team leader / human** read only the prefix of each team they belong to
  (`visibleTaskPrefixes` narrows the dual-prefix set). **Standalone tasks have
  no team ownership and stay L1-only** — an L2 caller can neither list nor read
  a `shared/tasks/…` run.
- **W4 anti-probing**: a run the caller cannot see returns the **same `404` as a
  missing run** — task/run existence across teams cannot be probed (never
  `403`).
- The optional `?team=` query narrows a detail read to one team's prefix
  (disambiguation); it is a filter, never authorization — the team-scope
  boundary above still applies.

## Status mapping

| Situation | Response |
|-----------|----------|
| `taskID` / `run` fails validation | `400` |
| run not found, or not in the caller's scope (W4) | `404` `{"message":"coding run not found"}` |
| no runs the caller may read | `200 []` (empty list, never `null`) |
| caller denied at the authorizer (worker) | `403` |
| storage read error mid-list | skipped for that prefix (the rest of the list still returns) |
| `runs.jsonl` missing / malformed | list & default detail unchanged; `?format=json` degrades (`meta` empty shell) |

## Write side: `runs.jsonl`

`run-coding-cli.sh` appends exactly one JSON line per run to
`{workspace}/coding-cli-logs/runs.jsonl` — the outcome index the read side
enriches from:

```json
{"v":1,"run":"run-20261015-101500.log","cli":"qwen","worker":"sysdev-coder","runner":"sysdev-lead",
 "status":"ok","reason":"","exit_code":0,"timeout_secs":3600,
 "started_at":"2026-10-15T10:15:00Z","ended_at":"2026-10-15T10:16:12Z","log_bytes":12345,
 "prompt_file":"/root/agentteams-fs/shared/tasks/t1/coding-prompts/20261015-101500.txt",
 "prompt_sha256":"…","prompt_summary":"…"}
```

- **One transcript per record, on every exit path from argument validation
  onwards** — ok, timeout (124), blocked-by-marker / blocked-missing-CLI
  (125), invalid approval mode / unknown CLI / runner failure. Each of those
  owns a `run-….log`: it is created up front and finalized with the diagnostic
  that explains the outcome, which a refusing exit writes to the transcript
  **and** stderr before appending the record. `log_bytes` is that transcript's
  size, so it is present for every recorded run. Argument-validation failures
  (missing `--cli` / missing prompt / missing workspace) are not runs: no
  record, no transcript. Empty values are omitted, matching the API's
  `omitempty` contract.
- **Claiming the transcript name.** The create is exclusive: an existing
  same-second transcript is never clobbered (that name belongs to the run
  holding the task), and the claim steps **forward one second at a time** so a
  burst of runs within one second still each get their own transcript. Only an
  existing name is treated as a collision — any other create failure (read-only
  or unwritable `coding-cli-logs/`, a full disk, an uncreatable directory)
  refuses the run at once with a visible diagnostic instead of retrying: a
  retry there would sit before the pre-flight guards and outside the CLI
  `--timeout`, so nothing could bound it. A candidate outside the enumerated
  `run-YYYYMMDD-HHMMSS.log` shape (a clock/date that cannot produce one) is
  refused the same way — claiming it would leave a transcript no reader could
  enumerate. Such refusals write no record (the directory cannot hold one, and
  a record without a transcript would break the one-to-one invariant) — like
  the argument-validation class, the run simply does not exist.
- **`status` is the script's exit code mapped**: `0 → ok`, `124 → timeout`,
  `125 → blocked`, anything else `failed`. `reason` disambiguates the blocked
  paths (`marker-conflict`, `cli-missing`) and is empty otherwise.
- `--worker <name>` records the worker the task served (passed by the
  delegating agent); `runner` records the executing container
  (`$AGENTTEAMS_WORKER_NAME`). `prompt_sha256` / `prompt_summary` come from the
  prompt file (sha256; the first 200 characters, single-lined).
- **Single writer**: runs of one task are serialized by the `.processing`
  marker, so appends do not interleave.
- **Mirror rule.** The skill mirrors `coding-cli-logs/` and `coding-prompts/`
  on **every** outcome (a failure must stay diagnosable, and the index must
  reach storage), while the full `workspace/` (the code changes) stays
  **success-only**. This is what makes `runs.jsonl` and `input.prompt` readable
  here at all.

## Example

```bash
# L1 admin lists every coding run (team + standalone), enriched from runs.jsonl
curl -s http://127.0.0.1:8090/api/v1/coding-runs -H "Authorization: Bearer $ADMIN_TOKEN"
# → 200 [{"task_id":"t1","run":"run-20261012-120000.log","team":"alpha-team",
#         "cli":"qwen","worker":"sysdev-coder","status":"ok","exit_code":0,
#         "started_at":"2026-10-12T11:59:58Z","ended_at":"2026-10-12T12:00:00Z",
#         "log_bytes":12345}, …]

# team leader (alpha) sees only alpha-team runs; standalone + other teams absent
curl -s http://127.0.0.1:8090/api/v1/coding-runs -H "Authorization: Bearer $ALPHA_LEAD_TOKEN"
# → 200 [{"task_id":"t1","run":"run-20261012-120000.log","team":"alpha-team", …}]

# open one run's transcript (byte-transparent)
curl -s http://127.0.0.1:8090/api/v1/coding-runs/t1/runs/run-20261012-120000.log \
  -H "Authorization: Bearer $ADMIN_TOKEN"
# → 200 "[run-coding-cli] cli=qwen workspace=…\n<full CLI transcript>"

# …or the structured envelope (same run)
curl -s "http://127.0.0.1:8090/api/v1/coding-runs/t1/runs/run-20261012-120000.log?format=json" \
  -H "Authorization: Bearer $ADMIN_TOKEN"
# → 200 {"task_id":"t1","run":"…","team":"alpha-team",
#         "meta":{"cli":"qwen","status":"ok","exit_code":0,…},
#         "input":{"prompt_summary":"…","prompt_available":true,"prompt":"…"},
#         "output":{"transcript":"…","status":"ok","exit_code":0}}

# cross-team / standalone read as an L2 caller → 404, indistinguishable from absent
curl -s http://127.0.0.1:8090/api/v1/coding-runs/t9/runs/run-20261012-120100.log \
  -H "Authorization: Bearer $ALPHA_LEAD_TOKEN"
# → 404 {"message":"coding run not found"}

# traversal attempt → 400 (rejected before any storage read)
curl -s "http://127.0.0.1:8090/api/v1/coding-runs/../etc/runs/run-20261012-120000.log" \
  -H "Authorization: Bearer $ADMIN_TOKEN"
# → 400 {"message":"invalid task id"}
```

## Notes

- **Data source is #1342** (the `coding-cli` skill's `run-coding-cli.sh`). Until
  #1342 merges, the log files do not exist yet, so the endpoints return an empty
  list / `404` — graceful, no error. This surface is the read side only and
  needs no #1342 code.
- **One-shot runs only** (#1342's current form). Real-time, serve-based
  sessions belong to the QwenCode runtime line (#1357, deferred) — a separate,
  later surface.
- **Transcript sensitivity**: the log is byte-transparent and is whatever the
  CLI printed — if the delegating agent put a secret in the prompt or a tool
  echoed one, it appears here. Access is bounded by the team scope above, but
  the "secrets do not go in prompts" responsibility sits with the delegating
  agent (the same contract the coding skill states); server-side redaction of
  arbitrary CLI stdout is out of scope for v1.
- **Dashboard consumption** is a separate follow-up (mirrors how the worker-chats
  dashboard panel consumes `worker-chats-api.md`).
- **v2 write side ships with the read side.** The enrichment is only as good
  as the `runs.jsonl` the wrapper writes: runs recorded by an older wrapper
  version (or before this change) have no line and stay on the v1 fields plus
  the filename-derived `started_at`. No backfill is attempted — the transcript
  itself remains the fallback record, and the list/index read costs one
  `GetObject` per task, not per run.

## Tests

- `internal/server/coding_runs_test.go` — over the `mcLikeOSS` mc-ls
  directory-view fake + fake K8s client:
  - list: `TestListCodingRuns_AdminSeesTeamAndStandalone`,
    `TestListCodingRuns_L2SeesOnlyOwnTeam`,
    `TestListCodingRuns_SkipsNonRunFiles`,
    `TestListCodingRuns_CapIsGlobalAcrossTeams` (the 200 cap stays global)
  - list enrichment (v2): `TestListCodingRuns_EnrichedFromRunsJSONL` (fields
    pass-through, `exit_code: 0` shown, second run degrading),
    `TestListCodingRuns_LocalFilenameIsNotReadAsUTC` (a container-local
    filename must not produce a `Z` instant — no fabricated start),
    `TestListCodingRuns_MalformedRunsJSONLIsTolerated`
  - blocked attempts: `TestBlockedAttemptVisibleToListAndDetail` (listed as
    `status=blocked` with `reason`, transcript readable verbatim), and
    `TestBlockedAttempts_EndToEnd_WriterStorageAPI` — the writer→storage→API
    regression: it runs the real `run-coding-cli.sh` through both blocked
    reasons, mirrors `coding-cli-logs/` + `coding-prompts/` into the fake
    store, and asserts the attempt is visible in the list and readable in the
    detail (skips when the shell toolchain is unavailable)
  - detail: `TestGetCodingRun_AdminReadsByteTransparent` (default
    `text/plain`, byte-identical),
    `TestGetCodingRun_L2OwnTeam`, `TestGetCodingRun_L2OtherTeam` (W4 404),
    `TestGetCodingRun_L2CannotSeeStandalone` (L1-only),
    `TestGetCodingRun_InvalidTaskID` (traversal 400),
    `TestGetCodingRun_InvalidRun` (400), `TestGetCodingRun_NotFound` (404)
  - detail `?format=json` (v2): `TestGetCodingRun_JSONFormat_FullMetadata`,
    `TestGetCodingRun_JSONFormat_PromptUnavailable`,
    `TestGetCodingRun_JSONFormat_NoMetadata` (empty `meta` shell, no 500),
    `TestGetCodingRun_JSONFormat_PromptBasenameFallback`
- `internal/auth/authorizer_test.go` — `TestAuthorizer_CodingRunsScoped` pins
  the authorizer boundary: L2 human + team leader `get`/`list` allowed (so the
  handler scope runs), worker denied.
- `tests/check-coding-cli-runs-metadata.sh` — the write side, over a stub CLI:
  - ok / timeout / bad-approval / both blocked reasons each append exactly one
    `runs.jsonl` line with the right `status` / `reason` / `exit_code`;
  - each of those leaves a **transcript that names the run and explains the
    outcome** (the blocked ones carry the refusal diagnostic) — the review
    regression for "recorded but invisible";
  - a closing invariant sweep asserts **every** recorded run names an existing
    transcript, whatever the outcome;
  - an unwritable/read-only `coding-cli-logs/` fails the run **promptly** with
    a visible diagnostic and without invoking the CLI (bounded by an external
    `timeout`, so a regression back to retrying shows up as a failure and not
    as a hang), and a same-second collision leaves the live transcript
    byte-identical while the claim steps to a free name;
  - a clock that cannot produce the enumerated `run-YYYYMMDD-HHMMSS.log` shape
    refuses the run instead of claiming a name no reader can enumerate;
  - `--worker` and `AGENTTEAMS_WORKER_NAME` land as `worker` / `runner`, empty
    values are omitted, runs append (one line **and** one transcript per run),
    and a non-UTC container (`TZ=Asia/Shanghai`) still records `started_at` in
    UTC.
  Wired into `.github/workflows/helm-lint.yml`.
