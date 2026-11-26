# storage-migrate — MinIO → S3-compatible migration tool

Migration helper for moving an AgentTeams installation from an embedded MinIO
storage plane to an external S3-compatible backend (RustFS, S3, or any
S3-compatible store). It complements the operational guide in
[`docs/design/storage-external-s3-backend.md`](../../docs/design/storage-external-s3-backend.md):
the guide describes the phases and the gates, this tool executes them with a
resumable state machine.

## What it does

```
phase 0 preflight   reachability, capacity, substrate, IAM snapshot, data-plane round trip,
                    multipart leftovers, clock skew, ODM probe, durability claim
phase 1 data        one of three modes:
                      mirror  - mc mirror passes (default; no target-side deletes)
                      odm     - RustFS on-demand migration (configure + backfill + poll)
                      inplace - no-op; rio-v2 imports MinIO data on the target's first start
phase 2 IAM replay  users and policies recreated on the target (secrets via --iam-secrets-file)
phase 3 verify      V1 bucket lists, V2 counts/sizes, V3 md5 sampling, V4 IAM diff,
                    V5 durability, V6 write freeze, V7 multipart, V8 engine, V9 IAM consistency
phase 4 cutover     final delta, write probes on both sides, and a checklist for the human
                    (the tool never changes production by itself)
```

`cutover` and `mark-cutover-done` are explicit human gates: the state machine
refuses `cutover` unless `verify` is green, and refuses `mark-cutover-done`
until the checklist has been executed.

## Operator runbook

Phases 0-3 are automated; the cutover (phase 4) is a **human-executed
runbook**. The tool runs the final delta, writes both write probes, and
generates `cutover-checklist.sh` — it never freezes writers, flips the
deployment config, or restarts consumers. The full command blocks for both
deployment modes (embedded and helm) are in the
[design document, §5 Cutover runbook (operator)](../../docs/design/storage-external-s3-backend.md#5-cutover-runbook-operator);
preview them before the cutover with
`bash scripts/storage-migrate/storage-migrate.sh --print-checklist --deploy-mode embedded|helm|both`
(read-only: no state read/write, no mc needed). The nine steps, each tagged
`[tool]`/`[manual]`/`[verify]` in the generated checklist:

1. `[tool]` Final delta + verify — already done by the tool run (see
   `report-cutover.json`).
2. `[manual]` Freeze ALL writers — external clients, then members, then the
   controller: embedded `agt update worker <name> --state Stopped` and
   `agt update manager <name> --state Stopped`; helm
   `kubectl -n <ns> patch worker <name> --type merge -p
   '{"spec":{"state":"Stopped"}}'`. Do not delete pods/containers to freeze
   (they are recreated immediately and keep writing); no CR/config changes
   during the window; the final delta must run before the embedded
   controller stops (the embedded MinIO lives in it).
3. `[manual]` Switch the deployment config in ONE change (provider +
   endpoint + bucket + credentials): embedded =
   `docker rm -f agentteams-controller`, then `docker run` with the captured
   previous flags plus `-e AGENTTEAMS_STORAGE_PROVIDER=s3 -e
   AGENTTEAMS_FS_ENDPOINT=… -e AGENTTEAMS_FS_BUCKET=…` and the target
   credentials (the installer does not support external S3 yet; **never use
   the installer reinstall path** — it deletes the data volume, the
   workspace and the env file); helm = `helm upgrade --install <release>
   <chart> -n <ns> --set storage.provider=s3 --set storage.mode=existing
   --set storage.s3.endpoint=… --set storage.s3.existingSecret=<secret>`
   (the MinIO StatefulSet stops rendering after the switch; do not delete
   its PVC — it is the rollback data source).
4. `[manual]` Recreate every consumer — env is baked at pod/container
   creation and "restart" does not re-read it: embedded
   `docker rm -f <prefix><manager> <prefix><worker-1> …`; helm
   `kubectl -n <ns> delete pod <prefix><manager>` and
   `kubectl -n <ns> delete pod <prefix><worker>`. Then, inside a recreated
   consumer: `env | grep AGENTTEAMS_FS_` must show the new endpoint and the
   same bucket as the controller.
5. `[manual]` Per-component read/write + permission checks: controller (one
   config deploy lands on the target), manager (write + read one object),
   each worker with its own credentials (proves the per-worker IAM policy
   exists on the target), and the negative case (worker A must get a 403
   reading worker B's prefix).
6. `[manual]` Resume traffic: `agt update worker --state Running` (or
   `kubectl patch` the CRs back); watch errors/audit.
7. `[manual]` Keep the source read-only for the shadow window; do not delete
   it.
8. `[manual]` Rollback (preserves target-side writes): freeze the target
   writers first, then per bucket `mc mirror --overwrite --remove
   <dst>/<bucket> <src>/<bucket>` — `--remove` is safe only while the source
   took no writes (Gate A empty); otherwise reconcile key by key and do NOT
   use `--remove`. Then flip the config back (provider=minio / the captured
   previous env), recreate all consumers again, and re-run step 5.
9. `[manual]` Record completion: `--mark-cutover-done` records an operator
   ACKNOWLEDGEMENT only — it does not verify restarts or configuration.

## Requirements

`bash >= 4`, `mc` (`RELEASE.2025-08-13` or newer), `curl` (8.x for `--aws-sigv4`),
`jq`, `flock`. The dependency gate checks versions, not just presence.

Alias creation retries for up to 30 seconds by default: RustFS can answer
health probes before its storage quorum is ready and return
`Service not ready: waiting for storage_quorum`. Override the retry budget
with `STORAGE_MIGRATE_ALIAS_RETRIES=<n>` when a slower target needs it.

## Usage

```bash
# dry run: preflight + plan only, no mutation
bash scripts/storage-migrate/storage-migrate.sh \
  --src-endpoint http://minio:9000 --src-ak AK --src-sk SK \
  --dst-endpoint http://rustfs:9000 --dst-ak AK2 --dst-sk SK2 \
  --mode mirror --state-dir /var/lib/storage-migrate --dry-run

# full run (preflight -> data -> IAM -> verify)
bash scripts/storage-migrate/storage-migrate.sh <same args>

# cutover, after verify is green
bash scripts/storage-migrate/storage-migrate.sh <same args> --phase cutover
# … execute the generated cutover-checklist.sh, then:
bash scripts/storage-migrate/storage-migrate.sh <same args> --mark-cutover-done
```

Cutover runbook options (they only affect the generated checklist text and
the read-only print; they never touch the state machine):

- `--deploy-mode embedded|helm|both` (default `both`) — which deployment-mode
  command blocks `cutover-checklist.sh` prints; recorded in the state
  (`stage_detail.deploy_mode`) and the cutover report.
- `--print-checklist` — print the cutover checklist to stdout and exit 0.
  Read-only: it does not read or write the state, creates no state
  directory, and needs no mc. Use it to preview the runbook before the
  cutover or to feed it into a change-management ticket.

The state machine resumes: re-running the same command continues from the
current stage. `state.json` lives in `--state-dir` (default
`./storage-migrate-state`), together with the report, the log and the IAM
snapshot.

## Running inside the container (recommended in production)

Production nodes usually have no `mc`/`jq`/`curl` on the host. `Dockerfile.runner`
builds an image with the whole toolchain; the host only provides three mounts:

```bash
docker build -f scripts/storage-migrate/Dockerfile.runner -t agentteams-storage-runner .
docker run --rm --network host \
  -v "$PWD/scripts/storage-migrate:/opt/storage-migrate:ro" \
  -v /var/lib/storage-migrate:/state \
  agentteams-storage-runner \
  bash /opt/storage-migrate/storage-migrate.sh --src-endpoint … --state-dir /state --dry-run
```

## Tests

```bash
bash scripts/storage-migrate/tests/storage-migrate-state-machine.sh   # phase gating, arg parsing
bash scripts/storage-migrate/tests/storage-migrate-dryrun-fake.sh     # dry run against a fake S3 stack
bash scripts/storage-migrate/tests/storage-migrate-runbook.sh         # cutover runbook assertions (read-only, no mc)
bash scripts/storage-migrate/tests/storage-migrate-e2e.sh             # full chain on real engines (Docker)
bash test/e2e/external-s3-backend.sh                                  # S3 plane + migration regression on real engines
```

All four run in CI on every change. `storage-migrate-e2e.sh` starts MinIO and
RustFS containers, seeds objects and IAM, then walks dry run → data → IAM
failure → resume → verify → cutover → the human gate, adds a negative gate
case, and finishes with an independent comparison (key/size sets, md5 per
object, users and policies) that does not trust the tool's own report.

## Integration contract (for a future API/UI)

There is no HTTP API or UI today — the tool is a CLI. What it *does* expose is a
stable contract that a controller endpoint, a dashboard, the workbench plugin or
a Manager skill can wrap without touching the migration logic:

| Surface | What a caller gets |
|---|---|
| `--status` | one JSON document on stdout, no S3 access, no state mutation: `schema` (`storage-migrate.status/v1`), `stage` (`NONE` … `CUTOVER_DONE`), `preflight`/`verify` maps of item id → verdict, and the artifact paths |
| `--dry-run` | the plan plus `dryrun-plan.json`, with zero mutation |
| `state.json` / `report.json` | the resumable state machine and every item verdict (`P0-*`, `V1-V9`); these are the source of truth `--status` renders |
| `cutover-checklist.sh` | the human gate, generated as a script |
| exit code | `0` ok, `1` general, `2` preflight red, `3` verify/IAM red, `4` usage, `130` interrupted — usable as a job result |

A future integration would put a thin job runner in front of the tool (the
migration must run where the engines are reachable, i.e. the environment that
carries `Dockerfile.runner`), expose `POST` to start and `GET` (backed by
`--status`) to poll, and map `stage`/verdicts to UI state. Two notes for that
work: the Manager cannot run the script itself (no Docker or host shell access
from its container), so a Manager- or chat-driven flow needs such an API or a
dispatched job; and the tool must keep pausing at the two human gates
(`cutover`, `mark-cutover-done`) — a UI should surface them as explicit
confirmations rather than automating them away.

## Exit codes

`0` ok · `1` general failure · `2` preflight red · `3` verify/IAM red ·
`4` usage/dependency error · `130` interrupted (state marked `interrupted=true`).

## Scope

The tool moves data and IAM between two S3 endpoints and produces the cutover
checklist. It does not start the target engine, flip the deployment's
configuration, or delete the source: those stay operational decisions described
in the guide. The tool never freezes writers, flips config or restarts
consumers — the [runbook in the design document, §5](../../docs/design/storage-external-s3-backend.md#5-cutover-runbook-operator)
covers that for both deployment modes.
