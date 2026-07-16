# Migrating a HiClaw fork to AgentTeams contracts

This release changes the active Kubernetes API group from
hiclaw.io/v1beta1 to agentteams.io/v1beta1. The migration is deliberately
maintenance-mode and reversible: old resources and CRDs are retained until a
later release.

## Before the maintenance window

1. Pin the old controller and runtime image tags.
2. Export all legacy Manager, Worker, Team, Human, and Project resources to
   hiclaw-resources-backup.yaml.
3. Back up the object-storage bucket containing agents/ and shared/.
4. Configure the dashboard password Secret before exposing its Service.

## Cutover

1. Stop the old controller so it cannot reconcile while converted resources
   are created.
2. Install the agentteams.io CRDs without deleting the legacy CRDs.
3. Preview the conversion and retain the JSON report:

       hiclaw migrate-api-group --dry-run --output migration-report.json

4. Apply the same conversion:

       hiclaw migrate-api-group --apply --output migration-report.json

5. Start the AgentTeams controller and verify that Manager, Worker, Team,
   Human, and Project counts match the report. Verify Project manifests and a
   sample of agents/ and shared/ objects before enabling the dashboard.

The migration command is idempotent. It creates or updates the
agentteams.io counterpart, records conflicts in the report, and never deletes
the legacy object.

## Compatibility window and rollback

AGENTTEAMS_* variables take precedence. Deprecated HICLAW_* aliases remain
accepted for one release and emit warnings.

To roll back during the compatibility window, stop the new controller, restore
the previous images, and restart the old controller against the retained
hiclaw.io resources. Restore object storage only if post-cutover validation
found data corruption; normal rollback does not require deleting new objects.

Do not delete legacy CRDs, legacy resources, previous images, or the storage
backup until the compatibility release has completed successfully.

## Integration acceptance

The fork's value-add (remediation, dashboard, milestones) is replayed onto the
renamed `agentteams.io` upstream baseline. Before this branch merges, every
item below must hold — these are the developer definition-of-done, separate
from the operator cutover steps above.

- **Rename consistency**: the API-group rename (`hiclaw.io/v1beta1` →
  `agentteams.io/v1beta1`) and the `AGENTTEAMS_*` env-var rename are applied
  end-to-end. No fork-internal `HICLAW_*` env-var reads remain outside the
  documented legacy-alias compatibility sites (`cmd/hiclaw/client.go` reads
  `HICLAW_CONTROLLER_URL`/`HICLAW_AUTH_TOKEN`/`HICLAW_AUTH_TOKEN_FILE` as
  deprecated aliases; the `hiclaw.io/team` annotation in `auth/middleware.go`
  is a legacy authorization fallback). In particular the env-var emit/read
  pairs are aligned (no dead features): `AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL`,
  `AGENTTEAMS_CMS_SERVICE_NAME`, `AGENTTEAMS_SOLO_OPERATOR` are emitted by the
  controller and read with the same name on the worker/manager side.
- **Dual CRDs**: both the new `*.agentteams.io` CRDs and the legacy
  `*.hiclaw.io` CRDs are present in `helm/hiclaw/crds/` and
  `hiclaw-controller/config/crd/` (legacy retained, no deletions, per the
  cutover rule). `make check-crd-sync` passes (controller ↔ Helm in sync).
- **Remediation replayed**: all eight remediation tiers (Tier 0–2C) are
  present in the replay with the three known porting gaps closed —
  `shared/lib/oss-credentials.sh` cached-cred fallback (Tier 0 #8), the
  `manager/tests/test-hiclaw-find-skill.sh` openhuman coverage (Tier 1D #12),
  and the `changelog/current.md` remediation entry (Tier 2C).
- **Controller compiles**: `go build ./internal/... ./cmd/...` is clean for
  both real targets — `cmd/controller` (CGO_ENABLED=1, the embedded operator)
  and `cmd/hiclaw` (CGO_ENABLED=0, the CLI). `controller-gen object` deepcopy
  is regenerated and `Project`/`ProjectList`/`TeamSpec.ModelProvider` carry
  their `DeepCopyObject`/schema.
- **Gates green**: the four `remediation-gates.yml` jobs pass — controller
  (`go test`), python-runtimes (copaw + hermes `pytest`), dashboard (server +
  web `npm test` + web `npm run build`), and helm (`helm dependency build` +
  `helm lint` + `helm template`). All touched `.sh` pass `bash -n`.

The following are known pre-existing replay gaps in newly-added code, tracked
as separate follow-ups (not blockers for the rename/replay landing): the
docker backend does not yet set `RestartPolicy: "unless-stopped"` for
`createMemberContainer` (`member_reconcile_test.go`); `project_controller`
records the leader twice in `Status.RecordedWorkers` (once as Name, once as
RuntimeName) — `project_controller_test.go` (3 tests). These do not affect
the build or the rename correctness.

