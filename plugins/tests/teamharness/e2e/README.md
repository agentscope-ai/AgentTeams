# Loop pause/resume e2e (#1431)

Runtime integration test for the #1431 closed loop: **pause stops new
delegations and downstream/next-round work, an in-flight task still saves its
result under pause, and an authorized resume followed by a fresh process
continues exactly once without repeating completed work.**

Unlike the unit suite, this does not validate fake CRD-client field changes.
`test_loop_pause_resume_e2e.py` drives the **real Controller HTTP API**
(project create / replan / pause / resume / task inspection) against a live
object store, and every taskflow action goes through `loop_driver.py`, which
imports the **real `plugins/teamharness/mcp` `taskflow` module** and runs it in
a separate process. Only the two Matrix notifier functions are stubbed
(`_send_delegate_notification`, `_task_completion_notification` — they append
`{action, kind, task_id, ...}` records to `$E2E_SENT_JSONL` instead of
sending); persistence (`mc` mirror/cp to the shared prefix), the pause gate,
transition validation and task state files all run on the actual code paths.

The four scenarios, in order:

1. **pause blocks new dispatch** — after `POST /pause`, delegating a ready
   task fails with `paused`, no notification is recorded, `workflow.next` is
   empty, and node statuses are unchanged.
2. **in-flight task saves its result under pause** — `submit_task` has no
   pause gate by design; the result must land in the object store and read
   back through `GET /tasks/{id}` as `submitted` with the stored summary,
   `result_path`, `submission_id` and a `submit_task` history entry.
3. **authorized resume continues exactly once** — after `POST /resume`, a
   fresh driver process delegates the ready task exactly once (one recorded
   notification), a second fresh process reuses the recorded event
   (`reused: true`, no second send), and re-delegating the submitted task is
   rejected.
4. **failure diagnostics** — every driver/API failure message carries the
   result JSON plus the last few `E2E_SENT_JSONL` records.

## Skip conditions

The whole file skips when `E2E_CONTROLLER_URL` is unset or
`E2E_MINIO != "1"`. In a normal dev checkout both are absent, so
`pytest plugins/tests/teamharness/e2e -q` reports all-skipped.

## Prerequisites (for a real run)

- A reachable **MinIO** (S3) with the `mc` CLI on `PATH`.
- A running **agentteams-controller** that talks to that object store:
  either the embedded mode (see below) or your existing local stack, with
  auth enabled (CLI token).
- Python 3.11 with `pytest` installed.

## Running locally (embedded controller, single machine)

```bash
# 1. object store
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

# AGENTTEAMS_MATRIX_URL pointed at MinIO is deliberate: the init loop counts
# any HTTP answer as "Matrix ready" (only connection errors retry), so
# registerAdmin fails fast and non-fatally and the CLI token is minted within
# seconds instead of after the 5-minute Matrix wait.

# 3. wait for the CLI token (written to /var/run/agentteams/cli-token;
#    pre-create the directory owned by your user if the controller can't)
until [ -s /var/run/agentteams/cli-token ]; do sleep 1; done

# 4. run the e2e suite
E2E_CONTROLLER_URL=http://127.0.0.1:18090 \
E2E_MINIO=1 \
E2E_CONTROLLER_TOKEN="$(cat /var/run/agentteams/cli-token)" \
AGENTTEAMS_FS_ENDPOINT=http://127.0.0.1:9000 \
AGENTTEAMS_FS_ACCESS_KEY=minioadmin \
AGENTTEAMS_FS_SECRET_KEY=minioadmin \
AGENTTEAMS_STORAGE_PREFIX=agentteams/agentteams-storage \
pytest plugins/tests/teamharness/e2e -q
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
