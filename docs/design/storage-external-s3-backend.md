# External S3-Compatible Storage Backend (`storage.provider=s3`)

AgentTeams can use an external S3-compatible object store as its storage
backend instead of the embedded MinIO. The chart ships MinIO as the
default; this document covers the optional `s3` provider, verified today
against **RustFS 1.0.3** (see [compatibility matrix](#verified-compatibility)).

## What `provider=s3` means

| Aspect | `minio` (default) | `oss` (external cloud) | `s3` (external self-hosted) |
|---|---|---|---|
| Engine lifecycle | StatefulSet in this chart | external, cloud-managed | external, you operate it |
| Credentials | static root (chart secret) | STS via credential-provider sidecar | fixed root (chart secret or existingSecret) |
| Bucket creation | controller (EnsureBucket) | external | **controller (EnsureBucket)** |
| Worker users/policies | controller via `mc admin` | external | **controller via `mc admin`** |
| Credential sidecar | no | yes | no |

The backend must speak **S3** and the **MinIO-compatible admin API**
(`mc admin user/policy`): the controller manages per-worker credentials
with the `mc` binary bundled in the controller image. RustFS implements
both (verified; note RustFS duplicate operations are idempotent no-ops
rather than errors, which the controller's idempotent admin client
handles in both styles).

## Verified compatibility

Claims in this document are scoped to the versions and deployment modes
actually exercised:

| Component | Version | Notes |
|---|---|---|
| AgentTeams | `main` @ `6ede5977` (v1.2.5) + this PR | K8s chart install |
| `mc` (controller-embedded & test) | `RELEASE.2025-08-13T08-35-41Z` (commit `7394ce0d`) | same binary the controller image ships |
| RustFS | `1.0.3` (`rustfs/rustfs:1.0.3`, amd64/arm64) | single-node, local disk; single-node on NFS loopback |
| MinIO (migration source) | `RELEASE.2025-09-07` | single-node, migration source only |
| Test env | Raspberry Pi 5 (arm64), Docker | full migrate→verify→cutover E2E, 200 MiB / 100 objects / 3 users / 7 policies |

Not yet verified: multi-node RustFS, RustFS on NFS in production mode
(see RustFS docs; treat as risk), other S3 engines (S3-compatible
engines must pass the [reproducible test suite](#tests) before use).

## Enabling it

```yaml
# values.yaml
storage:
  provider: s3
  mode: existing
  bucket: agentteams-storage
  s3:
    endpoint: "http://rustfs:9000"      # reachable from the controller pod
    # Preferred: reference an existing Secret
    existingSecret: my-rustfs-creds
    accessKeySecretKey: accesskey
    secretKeySecretKey: secretkey
    # Alternative (dev only): inline credentials, chart renders the secret
    # accessKey: "rustfsadmin"
    # secretKey: "rustfsadmin"
```

`helm install` validates the combination at render time
(`provider=s3` requires `mode=existing`, an endpoint, and credentials).
The MinIO StatefulSet is not rendered and no credential-provider sidecar
is deployed.

## Tests

Two reproducible layers:

1. **Controller admin-lifecycle unit tests** (run in CI, no live engine):
   `agentteams-controller/internal/oss/minio_admin_backend_test.go`
   drives the real `MinIOAdminClient` against a fake `mc` in both
   observed engine behaviours (MinIO error-style idempotency and RustFS
   silent idempotency), asserting the exact `mc admin` call sequence,
   alias reuse, policy document shape, and teardown order.
   `cd agentteams-controller && go test ./internal/oss/`
2. **Helm render tests** (run in CI): `tests/check-helm-agentteams.sh`
   covers the s3 provider render (endpoint/secret/env wiring, no MinIO
   StatefulSet, no sidecar, `existingSecret` + custom key names) and the
   failure modes (missing endpoint / missing credentials /
   `mode=managed`).
3. **End-to-end against real engines**: `test/e2e/external-s3-backend.sh`
   brings up MinIO (source) and RustFS (target) containers and exercises
   the full S3 operation matrix plus the `mc admin` user/policy
   lifecycle on both engines, printing the tested version matrix.
   Requires Docker; see the script header for usage.

## Migrating an embedded MinIO deployment to an external backend

> This section is the operational guide for moving an existing
> AgentTeams installation from embedded MinIO to an external S3 backend
> (e.g. RustFS), including **final data reconciliation** and
> **rollback after writes have begun on the target**.

### Phase overview

```
0. freeze window         stop worker writes (scale down workers / maintenance mode)
1. bulk copy             copy all objects source → target (mc mirror); capture the baseline manifest
2. IAM replay            recreate users/policies on target (mc admin)
3. shadow                re-enable workers pointed at target; source stays read-only
4. shadow reconciliation read-only: source unchanged vs baseline; target = baseline + shadow delta
5. cutover               flip AgentTeams to provider=s3 — no blanket source → target copy
6. (if broken)           rollback — see below
```

**The one rule that matters:** once shadow starts, the target is the write
authority and the source is read-only. A blanket `mc mirror --overwrite
src dst` after that point is a data-loss operation — it reverts every
object a worker wrote to the target back to the stale source content.
Correctness is therefore measured against the **baseline manifest**
captured right after the bulk copy, not against "source equals target".

### Automated migration tool

[`scripts/storage-migrate/`](../../scripts/storage-migrate/README.md) implements
the phases above as a resumable state machine (`preflight → data → IAM replay →
verify → cutover`), including the baseline manifest, the shadow delta gates and
the rollback checklist. It supports three data modes (`mirror`, RustFS `odm`,
`inplace`) and refuses `cutover` until `verify` is green.

```bash
bash scripts/storage-migrate/storage-migrate.sh --src-endpoint … --dst-endpoint … \
  --mode mirror --state-dir /var/lib/storage-migrate --dry-run
```

The tool never changes production by itself: `--phase cutover` only produces a
checklist for a human to execute (see the
[Cutover runbook (operator)](#5-cutover-runbook-operator)), and
`--mark-cutover-done` records the operator's acknowledgement only — it does
not verify restarts or configuration. The container image
(`scripts/storage-migrate/Dockerfile.runner`) carries the whole toolchain for
hosts without `mc`/`jq`/`curl`.

### 1. Bulk copy

```sh
mc mirror --overwrite --remove "src/agents"  "dst/agents"
mc mirror --overwrite --remove "src/shared"  "dst/shared"
# ... one per bucket (agents / shared / teams / agentteams-config, or the
# bucket list of your deployment)
```

### 2. IAM replay

The AgentTeams controller creates one user per Worker plus a scoped
policy (`worker-<name>`, team- and role-scoped). Replay on the target
with the same semantics the controller uses:

```sh
mc admin user add dst <worker> <password>
mc admin policy create dst worker-<worker> policy.json
mc admin policy attach dst worker-<worker> --user <worker>
```

If you migrate **with** the new chart version, you can instead let the
controller do it: after the endpoint switch, `EnsureUser`/`EnsurePolicy`
recreate everything on first reconcile (idempotent on both engine
behaviours). Pre-replaying gives you a verifiable target before cutover.

### 3–4. Shadow + shadow reconciliation

Run workers against the target while the source stays **read-only**.

Immediately after the bulk copy, capture the **baseline manifest** — the
reconciliation reference for every later step:

```sh
# per bucket, right after phase 1 (before shadow starts)
# List the whole bucket: `mc ls --recursive --json` returns keys relative to
# the path you list (listing a prefix yields prefix-relative keys).
mc ls --recursive --json src/<bucket> | sort > /tmp/baseline-<bucket>.jsonl
```

While shadow runs, record the **shadow delta** — it is the acceptance
reference for Gate B, the cutover check and the rollback check:

- **created** — keys on the target that are absent from the baseline;
- **updated** — baseline keys whose content changed on the target (keep
  the new value; size alone is not enough to prove content);
- **deleted** — baseline keys that are absent from the target (an intended
  deletion during shadow, not a gate failure).

```sh
jq -r .key /tmp/baseline-<bucket>.jsonl | sort > /tmp/base-keys-<bucket>.txt
mc ls --recursive --json dst/<bucket> | jq -r .key | sort > /tmp/dst-keys-<bucket>.txt
comm -13 /tmp/base-keys-<bucket>.txt /tmp/dst-keys-<bucket>.txt > /tmp/shadow-created-<bucket>.txt
comm -23 /tmp/base-keys-<bucket>.txt /tmp/dst-keys-<bucket>.txt > /tmp/shadow-deleted-<bucket>.txt
# updated: baseline keys still present but with different bytes.
# Capture both sides into files and cmp them: never compare through
# $(mc cat ...), which strips trailing newlines (b"keep\n" == b"keep").
: > /tmp/shadow-updated-<bucket>.txt
while read -r k; do
  grep -qxF -- "$k" /tmp/shadow-deleted-<bucket>.txt && continue
  mc cat "src/<bucket>/$k" > /tmp/cmp-src.bin || { echo "UNREADABLE-SRC $k"; continue; }
  mc cat "dst/<bucket>/$k" > /tmp/cmp-dst.bin || { echo "UNREADABLE-DST $k"; continue; }
  cmp -s /tmp/cmp-src.bin /tmp/cmp-dst.bin && continue
  echo "$k" >> /tmp/shadow-updated-<bucket>.txt
  mkdir -p "/tmp/shadow-value-<bucket>/$(dirname "$k")"
  cp /tmp/cmp-dst.bin "/tmp/shadow-value-<bucket>/$k"   # raw bytes, no normalisation
done < /tmp/base-keys-<bucket>.txt
```

After shadow starts, source/target equality is **not** the gate (the
target is expected to hold newer and additional objects). Two gates
replace it:

**Gate A — the source did not change.** The source must still match the
baseline manifest exactly; any difference means the freeze was breached
and those keys need an explicit, per-key reconciliation (see cutover
step 3).

```sh
mc ls --recursive --json src/<bucket> | sort > /tmp/now-<bucket>.jsonl
diff /tmp/baseline-<bucket>.jsonl /tmp/now-<bucket>.jsonl   # must be empty
```

**Gate B — the target holds exactly the baseline plus the recorded shadow
delta.** Do **not** require "every baseline key still exists with its old
size": a deletion performed during shadow is intended state, and an updated
key legitimately has new content and a new size. Record the delta as shadow
happens — `created`, `updated` (with the new value), `deleted`, one key per
line — and gate against that record:

```sh
# per bucket, after shadow
mc ls --recursive --json dst/<bucket> | sort > /tmp/dst-<bucket>.jsonl
jq -r .key /tmp/baseline-<bucket>.jsonl | sort > /tmp/base-keys-<bucket>.txt
jq -r .key /tmp/dst-<bucket>.jsonl      | sort > /tmp/dst-keys-<bucket>.txt

# 1. every baseline key that shadow did NOT delete is still on the target
comm -23 /tmp/base-keys-<bucket>.txt /tmp/shadow-deleted-<bucket>.txt \
  | comm -23 - /tmp/dst-keys-<bucket>.txt          # must be empty

# 2. untouched baseline objects are byte-identical on both sides.
#    Capture raw bytes into files, then cmp — a failed read must fail the
#    gate (and $(mc cat ...) would strip the trailing newline before any
#    comparison, so b"keep\n" and b"keep" would look identical).
comm -23 /tmp/base-keys-<bucket>.txt \
         <(cat /tmp/shadow-updated-<bucket>.txt /tmp/shadow-deleted-<bucket>.txt | sort) \
  | while read -r k; do
      mc cat "src/<bucket>/$k" > /tmp/cmp-src.bin || { echo "UNREADABLE-SRC $k"; continue; }
      mc cat "dst/<bucket>/$k" > /tmp/cmp-dst.bin || { echo "UNREADABLE-DST $k"; continue; }
      cmp -s /tmp/cmp-src.bin /tmp/cmp-dst.bin || echo "MISMATCH $k"
    done                                           # must print nothing

# 3. the recorded delta holds: creates exist, updates carry the shadow
#    content, deletions stay absent (never resurrected from the source)
while read -r k; do mc stat "dst/<bucket>/$k" >/dev/null 2>&1 || echo "MISSING CREATE $k"; done < /tmp/shadow-created-<bucket>.txt
while read -r k; do
  mc cat "dst/<bucket>/$k" > /tmp/cmp-dst.bin || { echo "UPDATE UNREADABLE $k"; continue; }
  cmp -s /tmp/cmp-dst.bin "/tmp/shadow-value-<bucket>/$k" || echo "UPDATE LOST $k"
done < /tmp/shadow-updated-<bucket>.txt          # must print nothing
while read -r k; do mc stat "dst/<bucket>/$k" >/dev/null 2>&1 && echo "RESURRECTED $k"; done < /tmp/shadow-deleted-<bucket>.txt   # must print nothing

# 4. nothing outside baseline ∪ created appeared on the target
comm -13 /tmp/base-keys-<bucket>.txt /tmp/dst-keys-<bucket>.txt   # == created keys only
```

Differences that are **expected** at this point: objects created on the
target during shadow (absent from the baseline), objects modified on the
target during shadow (baseline key, new content and size) **and objects
deleted on the target during shadow (baseline key, now absent)**. Record
all three as the shadow delta; do not "fix" them by copying the source
over them — re-copying a shadow deletion from the source is exactly the
data resurrection Gate B forbids.

### 5. Cutover runbook (operator)

The cutover is a **human-executed sequence**. The migration tool never
freezes writers, flips the deployment config, or restarts consumers:
`--phase cutover` runs the final delta, writes both write probes, and
generates the nine steps below as `cutover-checklist.sh` (preview it before
the cutover with `--print-checklist --deploy-mode embedded|helm|both`, a
read-only print that needs no state and no mc). Each step is tagged
`[tool]` (done by the tool run), `[manual]` (an operator runs it) or
`[verify]` (check the evidence).

**Why running consumers keep the old endpoint.** The controller injects
`AGENTTEAMS_FS_ENDPOINT`/`AGENTTEAMS_FS_BUCKET`/`AGENTTEAMS_FS_STORAGE_PREFIX`
and the per-worker credentials into every Manager/Worker container at
creation time; a container's/pod's env is baked in and is **not** re-read on
"restart". The rebuild decision (`hashAppliedWorkerSpec`) hashes only the
member CR spec — it does not include the controller's own storage
configuration and there is no configuration generation — so changing the
controller config alone never recreates running members and leaves a mixed
backend. Only deleting the member container/pod makes the controller
recreate it from the current config. That is why this runbook recreates
every consumer after the config flip instead of "changing the endpoint and
re-applying".

Freeze order matters for the embedded mode: the embedded MinIO lives inside
the controller container, so the members are frozen **before** the
controller is touched (a stopped controller means a stopped source, and the
final delta must run before that).

1. `[tool]` Final delta + verify — already done by the tool run (see
   `report-cutover.json`). On a RustFS target the ODM source fallback stays
   off after cutover (`RUSTFS_ON_DEMAND_MIGRATION_ENABLED=false`).
2. `[manual]` Freeze all writers — external clients → members → controller.
   - embedded: `agt update worker <name> --state Stopped` (repeat per worker)
     and `agt update manager <name> --state Stopped`. Do **not** delete
     pods/containers to freeze — they are recreated immediately and keep
     writing. No CR/config changes during the window.
   - helm: `kubectl -n <ns> patch worker <name> --type merge -p '{"spec":{"state":"Stopped"}}'`
     (same for `manager`). Strict tier (optional):
     `kubectl -n <ns> scale deploy <release>-controller --replicas=0`.
   - `[verify]` no writes: object counts stable on both ends for N seconds.
3. `[manual]` Switch the deployment config atomically (provider + endpoint +
   bucket + credentials in ONE change).
   - embedded: the installer does not support external S3 yet (it hard-codes
     `MINIO_ENDPOINT`/`FS_ENDPOINT=http://127.0.0.1:9000` and
     `MINIO_BUCKET=agentteams-storage`); recreate the controller container
     by hand:
     ```sh
     docker inspect agentteams-controller --format '{{json .Config.Env}}'   # capture the full current env first
     docker rm -f agentteams-controller
     docker run -d --name agentteams-controller --network agentteams-net \
       --network-alias matrix-local.agentteams.io --network-alias aigw-local.agentteams.io --network-alias fs-local.agentteams.io \
       <all previous -e flags> \
       -e AGENTTEAMS_STORAGE_PROVIDER=s3 \
       -e AGENTTEAMS_FS_ENDPOINT=<dst endpoint> -e AGENTTEAMS_FS_BUCKET=<bucket> \
       -e AGENTTEAMS_FS_ACCESS_KEY=<key> -e AGENTTEAMS_FS_SECRET_KEY=<secret> \
       -v /var/run/docker.sock:/var/run/docker.sock --security-opt label=disable \
       -v agentteams-data:/data -v <workspace>:/root/agentteams-fs/agents/manager \
       -p 8080:8080 -p 8001:8001 -p 18088:8088 --restart unless-stopped <embedded image>
     ```
     **WARNING:** never use the installer's reinstall path to change storage
     config — it deletes `agentteams-data`, the workspace and the env file.
     Installer support for external S3 is tracked separately.
   - helm:
     ```sh
     helm upgrade --install <release> <chart> -n <ns> \
       --set storage.provider=s3 --set storage.mode=existing --set storage.bucket=<bucket> \
       --set storage.s3.endpoint=<dst endpoint> --set storage.s3.existingSecret=<secret>
     ```
     The MinIO StatefulSet stops rendering after the switch; **do not delete
     its PVC** — it is the rollback data source.
   - `[verify]` the controller log shows the s3 (external) provider in use
     and the new container/pod env shows the new `AGENTTEAMS_FS_*`.
4. `[manual]` Recreate every consumer (env is baked at creation; "restart"
   does NOT re-read env):
   - embedded: `docker rm -f <prefix><manager> <prefix><worker-1> ...` — the
     controller recreates them from the current config.
   - helm: `kubectl -n <ns> delete pod <prefix><manager>` and
     `kubectl -n <ns> delete pod <prefix><worker>`.
   - `[verify]` inside a recreated consumer: `env | grep AGENTTEAMS_FS_`
     shows the new endpoint and the same bucket as the controller.
5. `[manual]` Per-component read/write + permission checks: the controller
   (trigger one config deploy and confirm the object lands on the target),
   the manager (write + read one object), each worker (write + read with its
   own credentials — proves the per-worker IAM policy exists on the target),
   and the negative case (worker A must get a 403 reading worker B's prefix).
6. `[manual]` Resume traffic: `agt update worker --state Running` (or
   `kubectl patch` the CRs back); watch errors/audit.
7. `[manual]` Keep the source read-only for the shadow window; do not delete
   it.
8. `[manual]` Rollback (preserves target-side writes): freeze the target
   writers first, then per bucket
   `mc mirror --overwrite --remove <dst>/<bucket> <src>/<bucket>`.
   `--remove` is safe only while the source took no writes (Gate A empty);
   otherwise reconcile key by key and do NOT use `--remove`. Then flip the
   config back (provider=minio / the captured previous env), recreate all
   consumers again, and re-run step 5.
9. `[manual]` Record completion: `--mark-cutover-done` records an operator
   ACKNOWLEDGEMENT only — it does not verify the restarts, the
   configuration or any object.

### Rollback after writes have begun on the target

Risk: once workers write to the target, the target holds objects the
source does not have. A plain "flip the endpoint back" would lose them.

Procedure (RPO = 0 when performed before the next source-side backup
expires; RTO ≈ delta-sync time + cutover):

1. **Stop writes on the target** (freeze workers; scale to zero).
2. **Reverse delta sync** — copy everything the target has back to the
   source. Since the source was frozen read-only during shadow (Gate A),
   the reverse set is exactly the objects created/modified **and the
   deletions** the target saw after the bulk copy — shadow-era changes
   included, not just post-cutover ones:

   ```sh
   mc mirror --overwrite --remove dst/<bucket> src/<bucket>   # per bucket
   ```

   `--remove` is what carries target-side deletions back to the source;
   it is safe **only** under Gate A (the source gained nothing during
   shadow). If Gate A is non-empty, drop `--remove`, mirror, and delete
   the affected source keys explicitly after reconciling each one.

   (One-way import features such as RustFS on-demand-migration do not
   apply in reverse; `mc mirror` is the supported path here.)
3. **Reconcile source against the target** — after the reverse sync the
   source must match the target object-for-object (counts/sizes/checksum
   sampling). Differences versus the original baseline manifest are
   expected: the shadow delta now lives on the source.
4. **Flip back**: `storage.provider=minio`, `helm upgrade`, verify one
   worker round-trip.
5. Keep the target frozen (read-only) for the investigation window;
   wipe or re-sync it when the retry starts.

Rollback before any target write is simpler: flip the endpoint back and
discard the target state — the source is untouched.

### Retention after cutover

Keep the old MinIO (stopped, data retained, image locally saved) for at
least one full backup cycle plus two release windows; take a final
reconciliation snapshot and an archive of the data directory before
decommissioning.
