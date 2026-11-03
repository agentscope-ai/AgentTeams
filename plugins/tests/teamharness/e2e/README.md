# Loop pause/resume e2e (#1431 / #1402)

Runtime integration test for the #1431 closed loop: **pause stops new
delegations and downstream/next-round work, an in-flight task still saves its
result under pause, and an authorized resume followed by a fresh process
continues exactly once without repeating completed work.** The loop
pause/resume half goes through the controller's **Loop endpoints**
(`POST /api/v1/loops/{name}/pause|resume`), which gate on the durable
`loop.pause_sources` of the project meta.

Unlike the unit suite, this does not validate fake CRD-client field changes.
`test_loop_pause_resume_e2e.py` drives the **real Controller HTTP API**
(project create / replan / pause / resume / task inspection / Loop
endpoints) against a live object store, and every taskflow action goes
through `loop_driver.py`, which imports the **real `plugins/teamharness/mcp`
`taskflow` / `projectflow` modules** and runs them in separate processes.
Only the two Matrix notifier functions are stubbed
(`_send_delegate_notification`, `_task_completion_notification` — they append
`{action, kind, task_id, ...}` records to `$E2E_SENT_JSONL` instead of
sending); persistence (`mc` mirror/cp to the shared prefix), the pause gate,
transition validation and task state files all run on the actual code paths.

The scenario, in order:

1. **loop plan + Loop CR** — `plan_loop` through the driver (fresh process),
   then the Loop CR is created through the embedded kube-apiserver (anchored
   on task B, the task delegated right after; an anchor without a published
   TaskMeta would make every pause/resume a legitimate no-op).
2. **pause blocks new dispatch** — the **project** is paused first (hard
   ordering, see below), then the **loop** through
   `POST /loops/{name}/pause {"source":"user"}`; delegating a ready task
   fails with `paused`, no notification is recorded, `workflow.next` is
   empty, and node statuses are unchanged.
3. **role boundary (executable)** — a worker token gets 403 from the
   authorizer middleware before the handler runs; a manager token gets 403
   from the handler when releasing the user source.
4. **durable state** — `loop.pause_sources == ["user"]` is verified by
   reading the project meta straight from the object store with `mc`
   (the API response alone is not evidence; this also proves the two 403s
   changed no state).
5. **in-flight task saves its result under pause** — `submit_task` has no
   pause gate by design; the result must land in the object store and read
   back through `GET /tasks/{id}` as `submitted` with the stored summary,
   `result_path`, `submission_id` and a `submit_task` history entry.
6. **human-source boundary** — a `{"source":"project"}` resume cannot
   release the human veto (still paused, `sources == ["user"]`); only an
   admin `{"source":"user"}` resume releases it.
7. **authorized resume continues exactly once** — the **project** is
   resumed (after the loop sources are released — hard ordering, see
   below), a fresh driver process delegates the ready task exactly once
   (one recorded notification), a second fresh process reuses the recorded
   event (`reused: true`, no second send), and re-delegating the submitted
   task is rejected.
8. **failure diagnostics** — every driver/API failure message carries the
   result JSON plus the last few `E2E_SENT_JSONL` records.

The file additionally covers the #1402 B **later boundary**
(`test_pause_before_meta_commit_survives_conditional_commit`): the
driver's `--pause-loop-before-commit` flag (submit only) fires a REAL
loop pause (`POST /loops/{name}/pause {"source":"user"}`) right before
the first conditional project meta commit — after its ETag read —
forcing a real 409. The retry must re-snapshot + re-merge and preserve
the durable `pause_sources` while the in-flight submission still lands;
the test asserts the real 409 → 200 commit chain, the object-store
state, the real worker gate, and the admin user-source release. For that
injection the driver maps `E2E_CONTROLLER_URL` / `E2E_CONTROLLER_TOKEN`
onto `AGENTTEAMS_CONTROLLER_URL` / `AGENTTEAMS_AUTH_TOKEN` (setdefault,
an explicit `AGENTTEAMS_*` value always wins).

**Hard ordering:** the project pause runs *before* the loop pause, and the
loop sources are released *before* the project resume. The project endpoints
round-trip the meta through a typed struct whose `loopMeta` lacks
`pause_sources` (a proven latent controller clobber, tracked as a
follow-up), while the Loop endpoints' conditional write preserves the raw
map — reversed, the project pause would silently drop the durable sources.

## Skip conditions

The whole file skips when `E2E_CONTROLLER_URL` is unset, `E2E_MINIO != "1"`,
or any of `E2E_K8S_API_URL` / `E2E_K8S_CA` / `E2E_K8S_NAMESPACE` /
`E2E_K8S_TOKEN` is missing — the skip reason names exactly which vars are
absent. In a normal dev checkout all of them are absent, so
`pytest plugins/tests/teamharness/e2e -q` reports all-skipped. The
worker/manager role-boundary tokens (`E2E_K8S_WORKER_TOKEN` /
`E2E_K8S_MANAGER_TOKEN`) are NOT skip conditions: a run without them fails
loudly, because the role boundary must be exercised, not skipped.

## Prerequisites (for a real run)

- A reachable **object store** (RustFS or MinIO, S3-compatible) with the
  `mc` CLI on `PATH`.
- A running **agentteams-controller** in **embedded** kube mode (see below)
  that talks to that object store, with auth enabled (CLI token). Embedded
  mode is required: the Loop CR is created through the embedded
  kube-apiserver, and the role-boundary identities are entries in its
  static token file.
- Python 3.11 with `pytest` installed.

## Running locally (embedded controller, single machine)

```bash
# 1. object store (RustFS or MinIO, any S3 engine on :9000)
minio server /tmp/e2e-minio/data --address :9000 --console-address :9001 &
export MC_HOST_agentteams=http://minioadmin:minioadmin@127.0.0.1:9000
mc mb --ignore-existing agentteams/agentteams-storage

# 2. controller in embedded mode (kube-apiserver must be on PATH)
CGO_ENABLED=1 go -C agentteams-controller build -o /tmp/agentteams-controller ./cmd/controller
AGENTTEAMS_KUBE_MODE=embedded \
AGENTTEAMS_DATA_DIR=/tmp/at-e2e/data \
AGENTTEAMS_CONFIG_DIR=/tmp/at-e2e/config \
AGENTTEAMS_CRD_DIR="$PWD/agentteams-controller/config/crd" \
AGENTTEAMS_HTTP_ADDR=127.0.0.1:18090 \
AGENTTEAMS_FS_ENDPOINT=http://127.0.0.1:9000 \
AGENTTEAMS_FS_ACCESS_KEY=minioadmin \
AGENTTEAMS_FS_SECRET_KEY=minioadmin \
AGENTTEAMS_FS_BUCKET=agentteams-storage \
AGENTTEAMS_STORAGE_PREFIX=agentteams/agentteams-storage \
AGENTTEAMS_MATRIX_URL=http://127.0.0.1:9000 \
nohup /tmp/agentteams-controller > /tmp/at-e2e/controller.log 2>&1 &
echo $! > /tmp/at-e2e/controller.pid

# AGENTTEAMS_MATRIX_URL pointed at the object store is deliberate: the init
# loop counts any HTTP answer as "Matrix ready" (only connection errors
# retry), so registerAdmin fails fast and non-fatally and the CLI token is
# minted within seconds instead of after the 5-minute Matrix wait.

# 3. wait for the CLI token (written to /var/run/agentteams/cli-token;
#    pre-create the directory owned by your user if the controller can't)
until [ -s /var/run/agentteams/cli-token ]; do sleep 1; done

# 4. role-boundary identities: the embedded apiserver reads its static
#    token file (--token-auth-file) at STARTUP only, so the two identities
#    need a restart. The file is written only-if-absent by the apiserver
#    bootstrap, so appending never clobbers the admin line; etcd (kine),
#    the CRDs and the bucket all survive the restart.
WORKER_TOKEN="$(openssl rand -hex 32)"
MANAGER_TOKEN="$(openssl rand -hex 32)"
printf '%s,system:serviceaccount:default:agentteams-worker-e2e,worker-e2e,""\n' \
  "$WORKER_TOKEN" >> /tmp/at-e2e/data/pki/token.csv
printf '%s,system:serviceaccount:default:agentteams-manager,mgr-e2e,""\n' \
  "$MANAGER_TOKEN" >> /tmp/at-e2e/data/pki/token.csv
printf '%s' "$WORKER_TOKEN" > /tmp/at-e2e/e2e-worker-token
printf '%s' "$MANAGER_TOKEN" > /tmp/at-e2e/e2e-manager-token
kill "$(cat /tmp/at-e2e/controller.pid)" 2>/dev/null || true
sleep 2
pkill -x kube-apiserver 2>/dev/null || true
sleep 1
AGENTTEAMS_KUBE_MODE=embedded \
AGENTTEAMS_DATA_DIR=/tmp/at-e2e/data \
AGENTTEAMS_CONFIG_DIR=/tmp/at-e2e/config \
AGENTTEAMS_CRD_DIR="$PWD/agentteams-controller/config/crd" \
AGENTTEAMS_HTTP_ADDR=127.0.0.1:18090 \
AGENTTEAMS_FS_ENDPOINT=http://127.0.0.1:9000 \
AGENTTEAMS_FS_ACCESS_KEY=minioadmin \
AGENTTEAMS_FS_SECRET_KEY=minioadmin \
AGENTTEAMS_FS_BUCKET=agentteams-storage \
AGENTTEAMS_STORAGE_PREFIX=agentteams/agentteams-storage \
AGENTTEAMS_MATRIX_URL=http://127.0.0.1:9000 \
nohup /tmp/agentteams-controller >> /tmp/at-e2e/controller.log 2>&1 &
echo $! > /tmp/at-e2e/controller.pid
until [ -s /var/run/agentteams/cli-token ]; do sleep 1; done

# 5. run the e2e suite
E2E_CONTROLLER_URL=http://127.0.0.1:18090 \
E2E_MINIO=1 \
E2E_CONTROLLER_TOKEN="$(cat /var/run/agentteams/cli-token)" \
E2E_K8S_API_URL=https://127.0.0.1:6443 \
E2E_K8S_CA=/tmp/at-e2e/data/pki/ca.crt \
E2E_K8S_NAMESPACE=default \
E2E_K8S_TOKEN="$(cat /var/run/agentteams/cli-token)" \
E2E_K8S_WORKER_TOKEN="$(cat /tmp/at-e2e/e2e-worker-token)" \
E2E_K8S_MANAGER_TOKEN="$(cat /tmp/at-e2e/e2e-manager-token)" \
AGENTTEAMS_FS_ENDPOINT=http://127.0.0.1:9000 \
AGENTTEAMS_FS_ACCESS_KEY=minioadmin \
AGENTTEAMS_FS_SECRET_KEY=minioadmin \
AGENTTEAMS_STORAGE_PREFIX=agentteams/agentteams-storage \
pytest plugins/tests/teamharness/e2e -q -rs
```

The same sequence, automated, is the `loop-pause-e2e` job in
`.github/workflows/test-controller.yml`.

## Notes for local re-runs

### `workflow.next` semantics

The controller's `workflow.next` lists every task whose raw status is
`planned` or `assigned` and whose dependencies are complete (the upstream
`_ready_nodes` semantics, mirrored in the controller's `buildWorkflow`).
A *paused* workflow therefore shows `next == []`, but a resume re-lists
the ready planned/assigned tasks — the e2e asserts `next == ["A"]` right
after the resume, not `[]`. When asserting that downstream work was not
released, write `"B" not in next` (or `next == ["A"]`), not `next == []`.

### Reset the bucket between runs

Task and project objects live at stable paths (`shared/projects/<id>`,
`shared/tasks/<id>`), so reusing the same bucket carries the previous
run's state forward: re-delegating a task that was `submitted` in the
last run fails with `task is 'submitted'`. Reset the bucket before every
local run:

```bash
mc rm --recursive --force agentteams/agentteams-storage
mc mb --ignore-existing agentteams/agentteams-storage
rm -rf /tmp/at-e2e/sent.jsonl /tmp/at-e2e/driver-workspace
```

## How the driver stubs the notifier

`loop_driver.py` imports `server` from `plugins/teamharness/mcp` (the same
pattern the unit suite uses), then installs two stubs before dispatching
through the real entry point:

- `server._send_delegate_notification` — resolved at call time through the
  refs layer, which looks at the server module's globals first, so a plain
  attribute assignment on `server` is visible at every proxy call site.
- `mcp.lifecycle.taskflow._task_completion_notification` — a module-level
  name in `taskflow.py`, so patching the module object works directly.

Both stubs append one JSON line per call to `$E2E_SENT_JSONL` and return a
success-shaped dict; the test then asserts on those records (send counts per
task, `reused` flags) instead of on network effects. Everything else —
project/task pulls, pause-gate checks, transition validation, `mc` sync,
signal events — runs unmodified.
