# Skill Catalog API

Status: implemented
API: `GET /api/v1/skills`

## Problem

The workbench (and any API client) needs a browsable catalog of the skills a
team can assign to its workers. Before this endpoint there was no way to
list them through the controller API — the Dashboard Skill Center reads its
own private storage, which user-facing clients cannot reach, and the Worker
CRD only records what is already assigned, not what is available.

The catalog answers two questions:

1. **Which built-in skills exist, and for which runtimes?** Built-in skills
   are seeded into `agents/<worker>/skills/` at provisioning time by the
   deployer, from role/runtime-specific agent template directories. A skill
   shipped by the `copaw` template, for example, is only available to copaw
   workers — the catalog must say so, or clients will offer assignments that
   silently no-op.
2. **Which skills are staged for deployment-wide distribution?** The
   dashboard's skill-upload flow stages skills under
   `agents/global/skills/`; they can be distributed to any worker from the
   dashboard or via `PUT /workers` (`skills` field).
3. **Which skills ship inside plugin packages?** Plugins such as
   TeamHarness bundle their own skills; on the current QwenPaw runtime they
   are live in the workers that carry the plugin, while being invisible to
   every other catalog layer (templates, global, spec) — the catalog must
   report them or it under-reports what a worker can actually do.

## Design

The L1 read-only endpoint `GET /api/v1/skills` is served by a
`SkillsHandler` with three sources (the team layer — see the follow-up
section — adds a fourth via `?team=`):

1. **Builtin skills.** The handler scans the agent-template directories the
   deployer uses when provisioning workers. The template→runtime mapping is
   **derived from `service.BuiltinAgentDir`** — the same function the
   deployer calls — by iterating every supported runtime
   (`service.AllWorkerRuntimes`) for the worker and team-leader roles. The
   catalog therefore cannot drift from what workers actually receive: if the
   deployer's template selection changes, the catalog's per-runtime
   availability changes with it, automatically. Each
   `skills/<skill>/SKILL.md` frontmatter contributes `name` and
   `description`; the directory name is the fallback name. The same skill
   shipped by several templates is reported once, with the providing
   templates listed in `agents` and the union of their runtimes in
   `runtimes`.
2. **Plugin skills.** The handler scans the bundled plugin packages under
   `AGENTTEAMS_PLUGIN_DIR` (default `/opt/agentteams/plugins`, baked into
   the controller image; the same source the plugin build packages into the
   worker images). Discovery is **manifest-driven**: a skill appears iff its
   plugin's `plugin.yaml` (`kind: AgentTeamPlugin`) declares it in its
   `skills:` block — unlisted `SKILL.md` directories do not leak into the
   catalog, and no worker state is scraped. Each declared skill resolves to
   `<pluginDir>/<plugin>/<path>/SKILL.md` for `name` / `description` /
   `version` (the manifest `id` is the fallback name; the plugin package
   `metadata.version` is the fallback version). Entries carry
   `source: "plugin"` and `plugin: <name>`; they carry **no**
   `runtimes` / `agents` / `updated_at` — availability follows the plugin's
   deployment (a worker has the skill iff it has the plugin), not
   per-runtime templates or per-worker assignment. They are read-only by
   construction: the team-skill upload writes the team layer only and
   never touches plugin entries, and a plugin skill's lifecycle belongs
   to the plugin package. A missing plugin dir, a
   malformed manifest, or a missing `SKILL.md` degrades that plugin (or
   that one skill) to absence — never an error. Builtin names win on
   collision, as with the shared half.
3. **Shared skills.** The handler lists the first level of
   `agents/global/skills/` (read-only, on each request). Directory entries
   are skills; bare files and dot-entries are skipped. Builtin names win on
   collision. A listing failure (prefix absent, storage down) degrades to an
   empty shared set — the catalog still serves builtins with `200`.

### Retention semantics of the shared prefix

`agents/global/skills/` is a **staging area, not a distribution channel**:
no worker entrypoint consumes this prefix automatically. A shared skill
reaches a worker only through per-worker distribution (dashboard Worker
dialog, or L1/L2 `PUT /workers` `skills`), which also records the
assignment in `spec.skills`. Consequently, deleting
`agents/global/skills/{name}/`:

- removes the skill from this catalog and from the dashboard's global area, and
- does **not** touch already-distributed per-worker copies
  (`agents/<worker>/skills/{name}/`) or existing `spec.skills`
  assignments — there is no cascade. A worker whose assignment is later
  removed stops receiving the skill at its next sync/refresh.

### No content access

Only frontmatter metadata of builtin and plugin skills is read
(`name` / `description` / `version` / `requires` for builtins;
`name` / `description` / `version` for plugin skills); shared skills are
listed by name + `updated_at` (the listing timestamp) only. Skill bodies
and registry credentials are never exposed; the endpoint performs no
registry calls. The response schema is deliberately limited to `name` /
`description` / `source` / `version` / `requirements` / `updated_at` /
`agents` / `plugin` / `runtimes` (pinned by
`TestSkillsCatalogFieldDiscipline`).

## Contract

`GET /api/v1/skills` → `200`

```json
{
  "skills": [
    {"name": "file-sync", "description": "Sync files with centralized storage.", "source": "builtin", "version": "1.0.0", "requirements": {"require_bins": ["mc"]}, "agents": ["copaw-worker-agent", "worker-agent"], "runtimes": ["copaw", "deepseek-harness", "openclaw", "openhuman", "qwenpaw"]},
    {"name": "teamharness-communication", "description": "Message delivery protocol.", "source": "plugin", "version": "1.2.0", "plugin": "teamharness"},
    {"name": "shared-kb", "source": "shared", "updated_at": "2026-09-11T08:00:00Z", "runtimes": ["copaw", "deepseek-harness", "hermes", "openclaw", "openhuman", "qwenpaw"]}
  ],
  "total": 3
}
```

- `source` is `"builtin"`, `"plugin"`, or `"shared"` (the L1 view; the
  `?team=` view adds `"team"` — see team-skills.md).
- `version` is the SKILL.md frontmatter `version` (top-level, falling back
  to `metadata.version`); for plugin skills it falls back further to the
  plugin package's `metadata.version`. Omitted when nothing declares it.
- `plugin` (plugin only) is the plugin package name (manifest
  `metadata.name`, else the plugin directory name).
- `requirements` (builtin only) mirrors the frontmatter `requires`
  declaration (`require_bins` / `require_envs` / `require_mcps`), parsed
  with QwenPaw 2.2.x semantics (`metadata.{openclaw,qwenpaw,clawdbot}.requires`
  shadows `metadata.requires`, which shadows top-level `requires`; a bare
  list is shorthand for `bins`). Enforcement is runtime-dependent: the
  qwenpaw 2.2.x registry gates skill activation on it; other runtimes have
  no equivalent gate yet — the field is exposed so workbenches can warn
  before assignment.
- `updated_at` (shared only, RFC3339 UTC) is the listing timestamp when the
  backend exposes it; omitted when unparseable. Builtin entries never
  carry it.
- `agents` is present for builtin skills (sorted, deduplicated template
  directory names).
- `runtimes` is sorted. For builtin skills it is the set of runtimes whose
  template ships the skill; for shared skills it is the full runtime list
  (any worker can be given a shared skill via per-worker distribution); for
  plugin skills it is **omitted** — a plugin skill is available to a worker
  iff the worker has the plugin (availability follows the plugin's
  deployment, not per-runtime templates).
- Output is sorted by `name`; missing template directories (deployment
  without some runtimes) are silently skipped.
- Errors: a backend read failure degrades to the remaining half (`200`).
  `400 team scope required` — non-admin callers: the catalog is the
  deployment-level (individual) skill layer, L1-only (see Authorization);
  the team-scoped read (`?team=`) follows with the team-skills work.

### Known limitation (tracked, out of v1)

`deepseek-harness` workers do not currently consume MinIO-seeded or
`spec.skills`-assigned skills: the dsh runtime prepares skills from its
in-image plugin manifest, so the runtime.yaml skill section is a no-op there.
The catalog still lists dsh in `runtimes` because the controller does seed
the files for dsh workers; assignments to dsh workers have no effect until
the runtime consumes them (follow-up). Clients should badge dsh
assignments accordingly.

## Authorization

`ActionList` on the `skills` resource kind (any other action is denied, not
defaulted). The catalog exposes the deployment-level ("individual") skill
layer, which is managed by the admin, so the handler enforces an **admin
(L1) only** role gate: non-admin callers (L2 humans, team leaders, workers,
manager) receive `400 team scope required` — the team-scoped catalog
(`?team=`) ships with the team-skills work (tracked in #1221). The response
is metadata-only (no PII, no credentials); per-worker assignment remains a
separate (write) concern via `PUT /workers`.

## Out of scope (v1)

- Registry-side listing (querying a registry for skills no worker references
  yet) — needs registry read credentials; separate requirement.
- Skill content download / upload (Dashboard Skill Center territory).
- Per-worker availability (`GET /workers/{name}/skills/available`) — the
  catalog is global metadata; per-worker truth (MinIO object existence) is a
  follow-up.
- Plugin skills remain read-only and non-assignable: they are not
  distributed via `spec.skills` (their lifecycle belongs to the plugin
  package), so the per-worker availability view above — when it ships —
  must derive them from the worker's plugin set, not from assignments.

## Follow-up: the team skill layer

The team-scoped read (`?team=`), the upload surface
(`POST /api/v1/skills`, `scope=team|deployment`), and the assign-time
materialization with the mandatory content scan (scan ②) are specified in
the companion design [team-skills.md](team-skills.md). This document stays
the reference for the L1 read-only catalog half; the team layer reuses its
response shape (new `source: "team"` entries) and its authorization
foundation.

## Tests

- `internal/server/skills_handler_test.go` — golden catalog (builtin
  dedup across templates with `agents` + `runtimes` union; shared directory
  entries; builtin-wins-on-collision; bare files and dot-entries skipped),
  template→runtime mapping consistency against `service.BuiltinAgentDir`
  for every (role, runtime) pair, response field discipline (no unexpected
  fields), frontmatter extension (`version` + `requires` in
  metadata-namespace / top-level / bare-list forms, namespace shadowing
  precedence, omitempty for undeclared skills), shared `updated_at`
  propagation (backend-supplied timestamp; builtin entries never carry
  it), non-admin no-team rejection (L2 human / team leader / worker /
  manager / missing caller → `400` with the self-explanatory message; the
  positive admin `200` path is pinned by the golden test), shared-half
  degradation on OSS list failure, empty `WorkerAgentDir` → shared-only
  catalog, plugin source (manifest-driven discovery; unlisted `SKILL.md`
  dirs excluded; frontmatter name/version with manifest-id and
  package-version fallbacks; no `runtimes`/`agents`/`updated_at` on plugin
  entries; builtin-wins-on-collision; missing plugin dir and malformed
  manifest degrade to absence).
- `internal/auth/authorizer_test.go` — authorization matrix for the
  `skills` resource kind.
