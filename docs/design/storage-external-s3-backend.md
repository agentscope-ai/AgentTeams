# External S3-Compatible Storage Backend (`storage.provider=s3`)

AgentTeams can use an external S3-compatible object store as its storage
backend instead of the embedded MinIO. The chart ships MinIO as the
default; this document covers the optional `s3` provider, verified today
against **RustFS 1.0.4** (see [compatibility matrix](#verified-compatibility)).

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
| RustFS | `1.0.4` (`docker.io/rustfs/rustfs:1.0.4`, amd64/arm64) | single-node, local disk; single-node on NFS loopback |
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

## Managed RustFS (single-node)

Instead of operating the S3 backend yourself, the chart can deploy
RustFS for you: a single-node StatefulSet whose image tag is pinned
with the AgentTeams release — the same pinning policy as the bundled
MinIO (the default `storage.rustfs.image.tag` and the
`hack/mirror-images.sh` entry always match).

```yaml
# values.yaml
storage:
  provider: s3
  engine: rustfs              # chart-managed RustFS (requires provider=s3)
  bucket: agentteams-storage
  rustfs:
    image:
      tag: "1.0.4"            # pinned with the release
    resources: {}
    persistence:
      size: 10Gi
    # Optional: reference an existing Secret (RUSTFS_ACCESS_KEY /
    # RUSTFS_SECRET_KEY) instead of the inline defaults:
    # auth:
    #   existingSecret: my-rustfs-creds
```

The controller is wired to the in-chart RustFS automatically
(`AGENTTEAMS_FS_ENDPOINT=http://<release>-rustfs:9000`, credentials via
secretKeyRef). `storage.engine=rustfs` is mutually exclusive with an
external endpoint: the render fails when the engine is combined with
`storage.provider=minio`/`oss`, with `storage.mode=existing`, or with a
non-empty `storage.s3.endpoint` — the managed engine deploys its own
in-cluster RustFS and the endpoint is always derived from the in-chart
StatefulSet. To point the controller at an external S3/RustFS instead,
use `storage.engine=external` (the default) + `storage.mode=existing` +
`storage.s3.endpoint=<your endpoint>`.

**Version pinning.** Like MinIO, the RustFS image tag is pinned per
AgentTeams release.

**Upgrade.** Change `storage.rustfs.image.tag` and run `helm upgrade`.
Data lives on the StatefulSet PVC and is preserved across upgrades
(check the RustFS version's data-dir compatibility before upgrading).

**Rollback.** `helm rollback <release> <revision>` restores the
previous revision, including the previous RustFS image tag.

## Fresh install vs. switching an existing installation

The backend choices above (`minio` / `oss` / `s3` external / `s3` +
`engine=rustfs`) define what a **fresh** installation deploys. They are
not a migration mechanism.

Changing `storage.provider` (or `storage.engine`) on an already running
installation is **not a data migration**: the objects stay on the old
backend, the new backend starts empty, and every read and write is
interrupted for the duration of the switch window. Move the objects
yourself before flipping the values, then switch. Minimal sequence:

1. Bulk-copy every bucket to the target (`mc mirror --overwrite
   --remove src/<bucket> dst/<bucket>`, one per bucket).
2. Stop writes (scale the Workers down / maintenance window).
3. Change the values (`storage.provider`, `storage.engine`, endpoint,
   credentials) and run `helm upgrade`.
4. Verify: the controller log shows the new provider, and one Worker
   round-trips a file through the new backend.

This PR does not implement or test the switch itself — it is out of
scope for this PR. The full operational guide with reconciliation and
rollback is the [Migrating an embedded MinIO deployment to an external
backend](#migrating-an-embedded-minio-deployment-to-an-external-backend)
section below.

## Local installer boundary

The local (embedded) installer currently always ships the embedded
MinIO; it has no RustFS or external-S3 selection, and this PR (chart,
Helm only) does not change that. Local external S3 is tracked in
#1464 (open): the installer would honour the same `AGENTTEAMS_FS_*`
variables the controller already reads. The local RustFS selection
flow (a built-in RustFS container as an install-time choice) is a
**proposed** follow-up PR — it is not implemented, and local
installations cannot select RustFS today.

## Acceptance

Evidence for the review checklist, scoped to what was actually
exercised in this round (2026-10-31, Raspberry Pi 5, aarch64, Docker,
RustFS `docker.io/rustfs/rustfs:1.0.4`):

- **Original backend compatibility.** The default `minio + managed`
  render path emits no RustFS resource (asserted in
  `tests/check-helm-agentteams.sh`), and this round's chart change
  touches no template rendered by the default profile. The
  `oss + existing` and `s3 + existing` render paths are covered by
  the same helm test — all 18 scenario assertions PASS, including
  the new managed-RustFS security-context assertions
  (`fsGroup: 10001`, `fsGroupChangePolicy: OnRootMismatch`,
  `runAsUser: 10001`, `runAsGroup: 10001`, `runAsNonRoot: true`,
  `allowPrivilegeEscalation: false` on the RustFS StatefulSet), the
  default-upstream image repository assertion, and the fsGroup
  override pass-through.
- **RustFS volume permissions.** RustFS 1.0.4 runs as uid/gid 10001
  (`USER=rustfs`) and ships its `/data` dir as
  `drwxr-x--- rustfs rustfs`. On the Pi: (i) a fresh volume whose
  ownership is inherited from the image started cleanly —
  `/minio/health/live` = 200, bucket create and object read/write
  OK; (ii) a root-owned `0755` directory as the data volume — the
  default PVC shape when no `fsGroup` is applied — FATALs at
  startup: `Server runtime failed: Io error: Permission denied
  (os error 13)`. The chart therefore runs the StatefulSet
  non-root (`runAsUser`/`runAsGroup` 10001, `runAsNonRoot: true`)
  with `podSecurityContext.fsGroup: 10001` +
  `fsGroupChangePolicy: OnRootMismatch`, so kubelet applies group
  ownership to the PVC (and does not recursively chown a large
  volume at every start). This fix is verified at the render level
  (helm assertions); a real cluster PVC is still pending k8s
  acceptance.
- **Admin API / user policies.** On the Pi instance (1.0.4),
  `mc admin user add`, `mc admin policy create`, `mc admin policy
  attach`, and `mc admin user list` all succeeded.
- **File access.** On the Pi instance (1.0.4), `mc cp` / `mc cat`
  round-tripped a 15-byte object with identical content. Controller-
  side file access reuses the existing Pi evidence (controller S3
  read/write plus Worker FileSync single-write to RustFS, from the
  earlier 1.0.4 round).
- **Restart persistence.** On the Pi, after `docker restart` the
  object was still present (15 B, STANDARD). For the chart form, the
  StatefulSet PVC survives `helm upgrade` / `helm rollback`, and the
  RustFS data dir is not baked into the image (the container's
  `/data` is the volume mount).

**Not verified (listed as-is).**

- A real Kubernetes cluster end to end: `helm install` → PVC
  provisioning → Pod scheduling → controller wiring. The volume
  permission fix above is asserted at render level only.
- Multi-node RustFS.
- RustFS on NFS in production mode (see the RustFS docs; treat as
  risk).

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

### 5. Cutover

1. Freeze writes on the target (short window; scale the workers down).
2. Re-check **Gate A** (source unchanged vs baseline) — read-only.
3. Only if Gate A is non-empty (the freeze was breached and the source
   did receive writes): reconcile **those listed keys one by one** into
   the target, after confirming the source copy is newer. Never run a
   whole-bucket `mc mirror --overwrite src → dst` — the target holds the
   shadow-era writes, and a blanket copy reverts them.
4. Re-check **Gate B** (untouched baseline keys intact, the recorded
   shadow delta — creates, updates **and deletions** — still holding, and
   no key outside baseline ∪ created on the target).
5. Update the deployment: `storage.provider=s3`, `storage.s3.endpoint=...`,
   credentials secret. `helm upgrade`.
6. Verify: controller log shows `storage provider: s3 (external)`; one
   worker round-trips a file through MinIO.

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
