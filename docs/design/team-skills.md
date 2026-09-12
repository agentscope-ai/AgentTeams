# Team Skills — Upload, Catalog, and Assign-Time Materialization

Status: implemented (embedded/docker mode; k8s-mode scan is a known
limitation)
APIs: `POST /api/v1/skills` · `GET /api/v1/skills?team=<name>` ·
`PUT /api/v1/workers {skills}` (existing)
Companion: `skill-catalog-api.md` (the read-only L1 catalog this builds on)

## Problem

A team's skills are today either **builtin** (shipped in the worker-agent
template, identical for every worker) or **deployment-wide** (staged under
`agents/global/skills/` by the dashboard, visible to every team). There is
no per-team skill layer: a marketing team cannot publish a skill to its own
workers without touching deployment-wide storage, and a worker's team
context is invisible to the skill catalog.

This design adds the **team skill layer** — storage, catalog read, upload,
and assign-time materialization — with content scanning as a hard gate.

## Contract (the single source for review)

```
read side (GET /api/v1/skills, existing endpoint):
  no team param     L1 (admin) → builtin + shared (deployment)        [unchanged]
  ?team=T           L1 any team / L2 own team / leader own team
                    → builtin + teams/T/skills/
                    L2/leader cross-team or unknown team → 404
                    (indistinguishable from "no such team", W8 anti-probing)

write side:
  POST /api/v1/skills            multipart/form-data
    scope=team        → teams/<t>/skills/<name>/      (admin any / L2 own team)
    scope=deployment  → agents/global/skills/<name>/  (admin only)
    common: structure validation + scan ① (best-effort) + exact-copy write

materialize (assign — PUT /workers {skills}, existing surface, NO new endpoint):
    → controller copies teams/<t>/skills/<s>/ → agents/<w>/skills/<s>/
      (scan ② MANDATORY: block or unavailable → not copied, warning surfaced)
    → worker sync loop materializes within its sync interval (≤5 min)
```

Roles that may publish: **admin** (any scope) and **L2 human** (own team,
`scope=team` only). Manager and team leaders **never** publish — leaders
read their team's catalog (the assign surface) but the write is denied at
the authorizer and re-checked in the handler (both layers pinned by tests).
Workers never.

## Storage layout

```
teams/<t>/skills/<name>/...        per-team skills (new)
agents/global/skills/<name>/...    deployment-wide (existing, dashboard)
agents/<w>/skills/<name>/...       materialized per worker (existing)
```

The skill's **name is the zip's single top-level directory**, which must
equal the `name` in `SKILL.md` frontmatter — the storage key, the catalog
identity, and the assign reference all agree. Upload is an exact copy
(`Mirror{Overwrite, Remove}` — a re-upload deletes files the new version
dropped), the same semantics `push-worker-skills.sh` already applies.

Team storage is seeded by `EnsureTeamStorage` (existing) — no new
seed prefix is required for skills.

## Upload (scan ①, best-effort)

`POST /api/v1/skills`, `multipart/form-data`: `file` (zip), `scope`
(`team`|`deployment`), `team` (required when `scope=team`).

Order: role gate (403) → structure validation (400) → team-scope check
(403/404) → scan ① → write.

Structure validation (400, self-describing):

- the zip contains **exactly one top-level directory** (the skill root);
- `SKILL.md` sits directly in the root, with valid YAML frontmatter whose
  `name` equals the directory name;
- the directory name is a valid skill name (kebab-case, ≤64 chars);
- zip-slip defence: absolute paths, backslashes, empty / `.` / `..`
  path components, and symlink entries are rejected;
- 64 MB cap on the zip **and** on the uncompressed payload (zip bomb).

Scan ① semantics (best-effort — the mandatory gate is scan ②):

| scan ① result | action |
|---|---|
| `block` (CRITICAL/HIGH) | 422 + findings (metadata only) |
| `pass` / `warn` | proceed; `warn` findings surfaced in the response |
| unavailable (no backend / container round trip failed) | proceed, response `scan.status="skipped"`, warning logged |

Response: `200 {name, scope, team?, files, scan:{status, findings?}}`.
Errors: `400` validation · `403` authz · `404` unknown/other team ·
`422` scan block.

## Content scan: the container round trip

The scanner is the qwenpaw skill scanner
(`qwenpaw.security.skill_scanner`) — it lives in the runtime image, and the
controller has no Python. A scan is therefore a round trip through the
**Docker Engine API** (raw HTTP over the mounted unix socket — the same
transport as `internal/backend`; the controller image is alpine and has no
docker CLI):

1. **Resolve the scan container.** The default Manager CR, only when its
   `spec.runtime` is `qwenpaw` (production managers run qwenpaw). No
   Manager CR or a non-qwenpaw runtime → unavailable.
2. **Upload the payload** as one tar (archive `PUT`) to
   `/tmp/.skillscan/<uuid>/`.
3. **Run the probe** (detached exec, inspect-poll): the probe writes one
   JSON line to `/tmp/.skillscan/<uuid>.verdict`.
4. **Read the verdict** (archive `GET`), then `rm -rf` the scratch
   (best effort).

The **probe is the gate** — it bypasses the runtime's off/warn/block
configuration:

- prefers `SkillScanner().scan_skill(...)` (no runtime-config gating);
- on older runtimes without that class, falls back to
  `scan_skill_directory(block=True)` — a raised `SkillScanError` is a
  **block** finding (block mode only raises on CRITICAL/HIGH), and a
  `None` result (scanner disabled or skill whitelisted by runtime config)
  is a **block** finding, not a pass;
- a scanner-import failure, a missing skill directory, or a hard crash
  yields `unavailable` — an infrastructure failure, **never a pass** (the
  controller sees a missing verdict file and errors).

Verdict mapping: CRITICAL/HIGH → `block` · MEDIUM/LOW/INFO → `warn` ·
none → `pass`. Findings carry metadata only (rule id, severity, file,
line, title) — never file contents.

Verdicts are cached by content hash (30 min TTL, 100 entries, FIFO) — the
same cache is shared by upload (①) and materialization (②), so assigning
a just-uploaded skill costs no extra exec.

**k8s mode (known limitation, v1):** the SPDY-exec variant is not
implemented; the scan fails closed. Uploads mark `scan.status="skipped"`
(best-effort semantics still apply); materialization **refuses to copy**
(scan ② mandatory). Embedded/docker clusters are the production
deployment today.

## Assign-time materialization (scan ②, mandatory)

`PUT /workers {skills}` is the assign surface (already live; no new
endpoint). The member reconcile passes the worker's effective team name
into `PushOnDemandSkills`:

- **team layer wins on a name clash**: a skill whose `SKILL.md` exists
  under `teams/<t>/skills/<s>/` is materialized by the controller
  (download → scan ② → exact-copy mirror into
  `agents/<w>/skills/<s>/`); the remaining skills go through the
  unchanged builtin recovery path (Manager push script / Worker-copy
  verification);
- scan ② outcome: `block` → **not copied**, warning returned (surfaces in
  the worker's reconcile warning, non-blocking); unavailable (no backend /
  round trip failed) → **not copied**, same warning (the gate does not
  default open); `warn` → copied, findings logged;
- one bad skill accumulates into the warning without breaking the
  worker's other assignments;
- the manager's assign path passes `""` (not team-scoped) — no team-layer
  consultation.

Audit: the materialization emits a structured log line (worker/team/skill/
scan/files). Once the capability foundation (audit record layer) lands,
this switches to a formal audit record — the seam is noted in code.

## Security review notes

- **404 anti-probing**: cross-team and unknown-team reads/writes are
  indistinguishable (status + body), both L2 and leader.
- **Layer isolation**: team skills never enter `agents/global/skills/`;
  deployment scope is admin-only; the builtin template directories are
  never written by this API.
- **Scan bypass paths**: (a) runtime config off/warn — bypassed by the
  probe calling the scanner directly / forcing block mode; (b) whitelist —
  a whitelisted skill returns `None` → block finding; (c) scanner absent
  or broken → unavailable → ① skips (logged), ② refuses; (d) non-qwenpaw
  manager → no scan container → unavailable, same as (c).
- **Secret hygiene**: findings and all logs carry metadata only.
- **zip safety**: traversal/symlink/size checks before any file touches
  the controller's temp dir or storage.

## Test coverage (see each commit)

- authorizer: publish role matrix (admin/L2 pass; manager/leader/worker
  denied — pinned);
- upload: scope matrix, structure 8-negative (missing SKILL.md, name
  mismatch, no frontmatter, two roots, zip-slip, symlink, bare root, bad
  name), zip bomb, scan gate (block 422 / warn surfaced / unavailable →
  skipped + written / nil scanner → skipped), exact-copy replace;
- skillscan: probe output parsing, container selection matrix,
  fake-Docker end-to-end, cache/TTL/FIFO, k8s fail closed, payload tar
  layout, probe contract pins;
- materialization: happy path, exact-copy replace, block not copied,
  unavailable fails closed, team-over-builtin priority, non-team skill
  stays on the builtin path, mixed, standalone (`teamName ""`).
