# Worker List Fields API

Status: implemented
API: `GET /api/v1/workers`

## Problem

Every `GET /api/v1/workers` row carried the full `WorkerResponse` — prompt
bodies (`soul`/`agents`), `env`, `skills`, `mcpServers`, `channelPolicy` —
even for consumers that only poll `phase` or `roomID` (issue #1475). The
endpoint gained an opt-in `?fields=` projection; this release also fixes
the identity gap in the `status` view that made row-order correlation
unsafe (issue #1477).

## Design

`?fields=` selects the row view:

- `spec` — stable configuration (identity, model and LLM tuning, runtime,
  package, exposed ports, team membership). Excludes prompt bodies, full
  configuration (`soul`/`agents`/`env`/`skills`/`mcpServers`/
  `channelPolicy`) and every status field.
- `status` — volatile runtime state (identity, `phase`, container state,
  heartbeat and task fields, team membership).
- `spec,status` — the union.
- `all` (or an absent/empty value) — the full response, byte-identical to
  the pre-projection implementation.

Parsing is fail-closed: unknown tokens, or `all` combined with a subset,
return `400` before any cluster access. Token order, trimming and
duplicates are tolerated. Projection happens after visibility filtering,
`?team=` filtering and L3 redaction, so those semantics are untouched.

### Identity contract

**Every view carries the worker's stable identity**: `name` (the CR name,
always present) and `workerName` (the display name, when set) — including
`fields=status`, which had no identity field at all before issue #1477.
List rows are keyed by `name`; a consumer must never correlate rows by
position in the list, because workers can be created, deleted, or
reordered between two list calls.

## List view compatibility matrix and migration

### This release: the default is still the full response

The server default is unchanged — an absent `?fields=` returns the full
`WorkerResponse`, so **no existing client is broken by this release**.
`?fields=` is an opt-in for new consumers.

The `agt` CLI's list command gained `--fields` (`all|spec|status|
spec,status`, default `all`, which sends `fields=all`): `agt get workers
-o json` output is byte-identical to before, and the single-worker form
`agt get workers <name>` always returns the full detail (the detail
endpoint has no projection) regardless of `--fields`.

### Backend capability × client type

| Client | This release (default full, `?fields=` opt-in) | Future: default flips to a summary view |
| --- | --- | --- |
| CLI, old (no `--fields`) | Full rows — unchanged. | List rows become summary: any read of `skills`/`env`/`mcpServers`/`soul` from the list silently degrades; table output degrades for fields outside the summary. Migrate: upgrade the CLI. |
| CLI, new (`--fields`, default `all` sends `fields=all`) | Full rows — unchanged. | Full rows — the explicit `fields=all` keeps working. |
| Dashboard, old (renders and initializes edit forms from list rows, no detail fetch) | Full rows — unchanged. | **Data-loss risk**: an edit/skill-assignment form initialized from a summary row lacks `skills`, so "add skill-b" submits `[skill-b]` and drops the existing `skill-a` (the report behind issue #1477). Migrate: ship the detail-fetch-before-write rule below. |
| Dashboard, new (list for display only; writes gated on the detail endpoint) | Safe — may even opt into a slim list view. | Safe — the write path never depends on the list row's field set. |
| Manager scripts | Full rows by default. `push-worker-skills.sh` already requests `--fields=all` explicitly. | Only scripts that request `fields=all` (or use the detail endpoint) stay correct. See "Known in-repo callers" below. |

### Dashboard rule: detail fetch before write

A Dashboard that initializes its detail/edit/skill-assignment view
directly from a list row breaks as soon as the row stops carrying
`skills` (see the table). The required rule:

1. **Write controls (skills, env, MCP, and any full-config editor) must
   stay disabled until the single-item detail fetch
   `GET /api/v1/workers/{name}` succeeds.** On a failed or still-loading
   detail fetch, the view renders read-only and shows the load error —
   it must not fall back to the list row's (possibly partial) field set.
2. The preserved-value contract: after the detail load, adding/removing a
   skill submits the *merged* list, so existing `skills`/`env`/`mcpServers`
   survive any edit.
3. The list view itself must **not** issue a per-row detail fetch
   (N+1): list rows render from the list payload alone; the detail fetch
   happens once, when the user opens one worker.

### Known in-repo callers

| Caller | Reads from the list | Status |
| --- | --- | --- |
| `manager/agent/skills/worker-management/scripts/push-worker-skills.sh` | `.skills` (the `--skill` target selector), `.roomID` (notifications) | **Migrated** (this PR): the list request now sends `--fields=all` explicitly; the `--add-skill`/`--remove-skill` baseline and the read-back verification use the single-worker detail endpoint, which is unaffected by the list view. |
| `manager/scripts/init/upgrade-builtins.sh` | `.name`, `.role`, `.runtime`, `.skills[]` | **Not yet migrated** — needs `--fields=all` (or a per-worker detail read) before the default flips. |
| `manager/agent/skills/worker-management/scripts/update-worker-config.sh` | `.phase`, `.message`, `.runtime` (runtime switch poll) | Works with `spec,status` or full; review at the flip. |
| `find-worker.sh`, `setup-mcp-server.sh`, `setup-mcp-proxy.sh`, `lifecycle-worker.sh`, `start-manager-agent.sh`, `install/agentteams-upgrade.sh` | `.name` (plus `phase`/`roomID` in a few) | Identity and status fields — safe with any view that keeps the identity contract. |
| Config export | — | There is **no list-based config export in this repository** as of this writing (the `agt` CLI has no `export` command and the controller exposes no export API). Wherever the Dashboard-side config export is implemented, it must request `fields=all` (or read the detail endpoint per worker). |

### Flipping the default to a summary

The server default may switch from the full response to a summary view
only when **both** hold:

1. **Every known caller is migrated**: each consumer that needs the full
   payload requests `fields=all` explicitly (or uses the detail
   endpoint), including the Dashboard, whose release carries the
   detail-fetch-before-write rule above. The "Known in-repo callers"
   table must be all-green — `upgrade-builtins.sh` is the known open
   item today.
2. **The version matrix is covered**: no deployment is known to pair an
   unmigrated client (old CLI, old Dashboard, unmigrated script) with a
   summary-default backend. This is an operational gate, checked at the
   flip, not a code change.

Until then, `?fields=` remains an opt-in optimization and the default
response stays byte-identical to the pre-projection implementation.
