# DeepAgents Final Isolation and Lifecycle Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the three independently reproduced DeepAgents isolation and lifecycle gaps: protect the Runner bearer capability from approved shell commands, revoke stale ExecutionSandboxes when their Worker stops satisfying ownership/runtime/mode invariants, and keep normal Matrix startup readiness in `Starting` rather than `Failed`.

**Architecture:** Keep the existing Worker-only DeepAgents architecture and CRDs. Harden the Runner process before it opens its HTTP listener, add a fail-closed and re-entrant revoke state machine plus Worker watch to the ExecutionSandbox controller, separate HTTP delete ownership checks from sandbox-mode admission, and make explicit container failures the only Kubernetes readiness path to `Failed`.

**Tech Stack:** Python 3.12, FastAPI/Uvicorn, Linux `prctl(2)`, pytest/unittest, Ruff, Go 1.24, controller-runtime, Kubernetes fake client/client-go, Helm 3, kubeadm/containerd, Matrix/Tuwunel, Higress, MinIO, PostgreSQL.

## Global Constraints

- Work only in `/tmp/agentteams-deepagents-runtime` on branch `codex/deepagents-agentteams-runtime` until the branch is reviewed and merged.
- Treat `/home/agent1/.config/agentteams/deployment-secrets.yaml` as secret material: pass it to Helm by filename and never print, diff, stage, commit, or copy its values into logs.
- Do not modify CRD schemas. No new Runner credentials, Kubernetes ServiceAccount token, Matrix token, Higress key, MinIO key, or PostgreSQL credential may enter an execution command.
- For every behavior change, first add a focused test and run it to observe the expected failure; only then change production code.
- Changes under `deepagents-agentteams/` and `agentteams-controller/` must be recorded in `changelog/current.md` before the implementation branch is considered complete.
- Commit after each green logical unit. Do not combine the Runner boundary, controller revoke lifecycle, HTTP deletion, and readiness mapping into one commit.
- Preserve the existing exactly-once request-ID store, unknown-outcome behavior, timeout process-group kill, workspace validation, MinIO writeback, PostgreSQL checkpoints, non-root security context, and NetworkPolicy ceilings.

---

## Task 1: Make the Runner Bearer Token Unreadable to Approved Commands

**Files:**

- Modify: `deepagents-agentteams/src/deepagents_agentteams/runner.py`
- Modify: `deepagents-agentteams/src/deepagents_agentteams/runner_core.py`
- Create: `deepagents-agentteams/tests/test_runner_process_hardening.py`
- Modify: `deepagents-agentteams/tests/test_runner_core.py`

- [ ] **Step 1: Add failing unit tests for fail-closed startup hardening**

  Add tests for a public module-level function named `consume_and_harden_runner_token()` with these requirements:

  - it pops a non-empty `AGENTTEAMS_RUNNER_TOKEN` from `os.environ`;
  - it sets `RLIMIT_CORE` soft and hard limits to zero;
  - on Linux it calls `prctl(PR_SET_DUMPABLE, 0)` and checks the return code using `ctypes.get_errno()`;
  - missing token, unsupported platform, `setrlimit` failure, `CDLL`/`prctl` failure, or non-zero `prctl` return raises before Uvicorn is called;
  - after a successful call the returned token remains usable by `create_app`, but is absent from the process environment.

  Patch `uvicorn.run` and `create_app` in the startup failure test and assert neither is reached after a hardening error.

- [ ] **Step 2: Add a failing real-process `/proc` regression test**

  Start a dedicated Python helper process with a non-secret sentinel such as `runner-sentinel-not-a-secret`. Inside the helper, call `consume_and_harden_runner_token()`, build a temporary `RunnerService`, and execute a shell command that attempts to read `/proc/$PPID/environ`. Return only a boolean/result code from the helper; never emit the environment block. Assert:

  - the shell cannot read the hardened parent environment;
  - command output does not contain the sentinel;
  - the command environment does not contain `AGENTTEAMS_RUNNER_TOKEN`;
  - the test skips with an explicit Linux-only reason outside Linux.

- [ ] **Step 3: Add a failing subprocess descriptor-closure assertion**

  In `test_runner_core.py`, patch `subprocess.Popen`, exercise `RunnerService.execute`, and assert the call explicitly contains both `close_fds=True` and `start_new_session=True`.

- [ ] **Step 4: Run the focused tests and confirm RED**

  Run:

  ```bash
  cd /tmp/agentteams-deepagents-runtime/deepagents-agentteams
  uv run --locked --extra dev pytest -q \
    tests/test_runner_process_hardening.py \
    tests/test_runner_core.py \
    tests/test_runner_api.py
  ```

  Expected: failures because `consume_and_harden_runner_token()` and explicit `close_fds=True` do not yet exist, and the current process remains inspectable.

- [ ] **Step 5: Implement the Linux process boundary before app construction**

  In `runner.py`:

  - import `ctypes`, `resource`, and `sys`;
  - define `_PR_SET_DUMPABLE = 4`;
  - implement `consume_and_harden_runner_token() -> str` so it pops the token first, rejects an empty token, rejects non-Linux hosts, sets `resource.RLIMIT_CORE` to `(0, 0)`, loads the process C library with `ctypes.CDLL(None, use_errno=True)`, configures the `prctl` signature, calls `prctl(_PR_SET_DUMPABLE, 0, 0, 0, 0)`, and raises `OSError` with the captured errno when the call fails;
  - call this function as the first security-sensitive operation in `main()`, before constructing `RunnerService`, `FastAPI`, or starting Uvicorn.

  Do not log the token or include it in an exception. In `runner_core.py`, add `close_fds=True` to `subprocess.Popen`; retain the current minimal environment and process-group behavior.

- [ ] **Step 6: Run focused and full Python gates**

  Run:

  ```bash
  cd /tmp/agentteams-deepagents-runtime/deepagents-agentteams
  uv run --locked --extra dev pytest -q \
    tests/test_runner_process_hardening.py \
    tests/test_runner_core.py \
    tests/test_runner_api.py
  uv run --locked --extra dev pytest -q
  uv run --locked --extra dev ruff check .
  ```

  Expected: all tests and Ruff checks pass. Inspect the real-process assertion to confirm it reports only the blocked/readability result, not the sentinel or any environment content.

- [ ] **Step 7: Commit the Runner boundary**

  ```bash
  cd /tmp/agentteams-deepagents-runtime
  git add deepagents-agentteams/src/deepagents_agentteams/runner.py \
    deepagents-agentteams/src/deepagents_agentteams/runner_core.py \
    deepagents-agentteams/tests/test_runner_process_hardening.py \
    deepagents-agentteams/tests/test_runner_core.py
  git commit -m "fix(deepagents): isolate runner bearer capability"
  ```

---

## Task 2: Revoke Stale ExecutionSandboxes Fail-Closed

**Files:**

- Modify: `agentteams-controller/internal/controller/execution_sandbox_controller.go`
- Modify: `agentteams-controller/internal/controller/execution_sandbox_controller_test.go`

- [ ] **Step 1: Add failing table tests for every revoke condition**

  Build a Ready ExecutionSandbox with same-name Service, immutable token Secret, Running Pod, and NetworkPolicy. Add table cases for:

  - referenced Worker is NotFound;
  - Worker controller label changes from the current controller;
  - Worker UID differs from `spec.workerRef.uid`;
  - effective Worker runtime changes from `deepagents` to `openclaw`;
  - DeepAgents runtime config is absent;
  - execution mode changes from `sandbox` to `disabled`.

  On the first reconcile, assert Service and Secret deletion is requested, Pod deletion is requested, and NetworkPolicy plus ExecutionSandbox remain while a test client deliberately retains the terminating Pod. On the next reconcile after allowing Pod disappearance, assert NetworkPolicy and ExecutionSandbox are deleted. Reconcile once more and assert idempotent success with no child recreation.

  Replace the current `TestExecutionSandboxReconcilerIgnoresSandboxReferencingForeignWorker` expectation: an owned sandbox that references a now-foreign Worker must be revoked; only an ExecutionSandbox whose own controller label is foreign remains ignored.

- [ ] **Step 2: Add failing API-error containment tests**

  Extend the existing intercepting-client pattern to inject non-NotFound errors while deleting Service, Secret, or Pod and while observing Pod deletion. Assert the reconciler returns an error, does not delete NetworkPolicy or the ExecutionSandbox CR, and never creates replacement children on the revoke path.

- [ ] **Step 3: Add failing Worker watch mapper and predicate tests**

  Add focused tests for:

  - `executionSandboxesForWorker` lists only the same namespace, same `agentteams.io/controller`, and same `agentteams.io/worker` label;
  - create and delete events for an owned Worker enqueue related sandboxes;
  - update events enqueue when either the old or new Worker has the current controller label, so removing/changing that label still triggers revocation;
  - unrelated and foreign Workers do not enqueue current-controller sandboxes.

- [ ] **Step 4: Run the focused controller test and confirm RED**

  ```bash
  cd /tmp/agentteams-deepagents-runtime/agentteams-controller
  mkdir -p /tmp/agentteams-go-tmp
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct \
    go test ./internal/controller -run 'TestExecutionSandbox(Reconciler|Worker)' -count=1
  ```

  Expected: current validation errors/early returns leave children and the sandbox behind, and no Worker watch mapper exists.

- [ ] **Step 5: Implement the re-entrant revoke state machine**

  In `Reconcile`, retain the existing first guard that ignores ExecutionSandboxes not owned by this controller. After fetching the Worker:

  - route Worker NotFound to `revokeExecutionSandbox`;
  - return other Worker GET errors normally;
  - route controller-label mismatch, empty/mismatched UID, non-DeepAgents effective runtime, missing DeepAgents config, and non-sandbox mode to the same revoke method;
  - run resource/policy resolution and normal lifecycle only after all invariants pass.

  Implement `revokeExecutionSandbox(ctx, sandbox)` with this exact order and state boundary:

  1. best-effort delete same-name Service;
  2. best-effort delete same-name Secret;
  3. request same-name Pod deletion;
  4. GET the Pod; if it still exists, keep NetworkPolicy and ExecutionSandbox and return a five-second requeue after surfacing any accumulated API error;
  5. only after Pod NotFound, delete same-name NetworkPolicy;
  6. only if prior non-NotFound operations succeeded, delete the ExecutionSandbox CR.

  Treat NotFound as success. Aggregate independent containment failures with `errors.Join`, but never cross the Pod-existence or accumulated-error boundary to remove the NetworkPolicy/CR. The revoke path must not call `ensureExecutionSandboxToken`, `buildExecutionSandboxResources`, or `ensureExecutionSandboxObject`.

- [ ] **Step 6: Add the scoped Worker watch**

  Import controller-runtime `handler`. Implement `executionSandboxesForWorker(ctx, object)` by listing `ExecutionSandboxList` with `client.InNamespace(object.GetNamespace())` and exact matching labels for `LabelController` and `LabelWorker`. Add an `ExecutionSandboxWorkerPredicates(controllerName)` predicate whose update branch matches the old **or** new Worker controller label. Wire it in `SetupWithManager` using `Watches(&v1beta1.Worker{}, handler.EnqueueRequestsFromMapFunc(...))`.

- [ ] **Step 7: Run focused and full Go controller tests**

  ```bash
  cd /tmp/agentteams-deepagents-runtime/agentteams-controller
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct \
    go test ./internal/controller -run 'TestExecutionSandbox' -count=1
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct \
    go test ./... -count=1
  ```

  Expected: all revoke order, retry, ownership, mapper, and existing controller tests pass.

- [ ] **Step 8: Commit the lifecycle revoke behavior**

  ```bash
  cd /tmp/agentteams-deepagents-runtime
  git add agentteams-controller/internal/controller/execution_sandbox_controller.go \
    agentteams-controller/internal/controller/execution_sandbox_controller_test.go
  git commit -m "fix(controller): revoke stale execution sandboxes"
  ```

---

## Task 3: Permit Explicit Sandbox Delete After Mode Is Disabled

**Files:**

- Modify: `agentteams-controller/internal/server/execution_sandbox_handler.go`
- Modify: `agentteams-controller/internal/server/execution_sandbox_handler_test.go`

- [ ] **Step 1: Add failing HTTP Delete tests**

  Add a test that creates an owned Worker and matching ExecutionSandbox, changes only `worker.spec.runtimeConfig.deepagents.execution.mode` to `disabled`, calls Delete, and expects HTTP 204 plus sandbox NotFound. Add negative cases proving Delete still returns conflict and preserves the sandbox for:

  - foreign Worker controller label;
  - foreign ExecutionSandbox controller label;
  - current Worker UID different from `spec.workerRef.uid`;
  - Worker name or session-ID identity collision.

  Retain tests proving Ensure and Heartbeat still reject a disabled Worker.

- [ ] **Step 2: Run the handler test and confirm RED**

  ```bash
  cd /tmp/agentteams-deepagents-runtime/agentteams-controller
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct \
    go test ./internal/server -run 'TestExecutionSandbox.*(Delete|Disabled|Foreign|Identity)' -count=1
  ```

  Expected: Delete returns HTTP 409 for the disabled Worker because it currently calls `deepAgentsWorker`.

- [ ] **Step 3: Separate ownership lookup from runtime admission**

  Extract `ownedWorker(w, r)` to validate the URL Worker name, GET the current Worker, and enforce the exact controller label. Make `deepAgentsWorker` call `ownedWorker` and then enforce effective runtime `deepagents`, non-nil config, and mode `sandbox`.

  Refactor the sandbox lookup so it accepts an already validated current Worker and then checks session-ID syntax, sandbox controller ownership, Worker name, current Worker UID, and session ID. Use:

  - `deepAgentsWorker` for Ensure and Heartbeat;
  - `ownedWorker` for Delete;
  - the shared identity-checking sandbox lookup for Heartbeat and Delete.

  Do not relax UID, controller, name, or session checks.

- [ ] **Step 4: Run focused and full server tests**

  ```bash
  cd /tmp/agentteams-deepagents-runtime/agentteams-controller
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct \
    go test ./internal/server -run 'TestExecutionSandbox' -count=1
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct \
    go test ./internal/server -count=1
  ```

  Expected: disabled-mode Delete returns 204; Ensure/Heartbeat and all ownership boundaries remain unchanged.

- [ ] **Step 5: Commit the HTTP lifecycle correction**

  ```bash
  cd /tmp/agentteams-deepagents-runtime
  git add agentteams-controller/internal/server/execution_sandbox_handler.go \
    agentteams-controller/internal/server/execution_sandbox_handler_test.go
  git commit -m "fix(controller): allow stale sandbox deletion"
  ```

---

## Task 4: Keep Normal Matrix Readiness Waiting in Starting

**Files:**

- Modify: `agentteams-controller/internal/backend/kubernetes.go`
- Modify: `agentteams-controller/internal/backend/kubernetes_test.go`

- [ ] **Step 1: Change the readiness tests first**

  In `TestK8sStatus_ReadyCondition`, change `Running + Ready=False with message` to use the real diagnostic shape (`Reason: "ContainersNotReady"`, message naming the unready Worker container) and expect `StatusStarting` while preserving the message. Add/adjust a no-Ready-condition case to expect `StatusStarting`.

  Keep explicit cases showing `CrashLoopBackOff`, `ImagePullBackOff`, and a non-zero terminated container remain `StatusFailed`, regardless of Ready-condition text.

- [ ] **Step 2: Run the focused test and confirm RED**

  ```bash
  cd /tmp/agentteams-deepagents-runtime/agentteams-controller
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct \
    go test ./internal/backend -run 'TestK8sStatus_(ReadyCondition|Container)' -count=1
  ```

  Expected: a Running Pod with `Ready=False` and `ContainersNotReady` text is currently returned as `Failed`; a missing Ready condition is currently treated as healthy.

- [ ] **Step 3: Implement precedence-safe status mapping**

  Preserve `podContainerFailureStatus` as the first and only readiness-related path that promotes a Worker to `Failed`. If no explicit container failure exists and normalized phase is Running:

  - Ready true remains `Running`;
  - Ready false becomes `Starting` and copies its non-empty message into `WorkerResult.Message` for diagnostics;
  - missing Ready condition becomes `Starting`.

  Update `podReadyCondition` comments and return semantics so absence is not treated as ready. Do not broaden the list of container failure reasons.

- [ ] **Step 4: Run focused and full backend tests**

  ```bash
  cd /tmp/agentteams-deepagents-runtime/agentteams-controller
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct \
    go test ./internal/backend -run 'TestK8sStatus|TestPodReadyCondition' -count=1
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct \
    go test ./internal/backend -count=1
  ```

  Expected: normal Matrix readiness remains `Starting`, while explicit container failures remain `Failed`.

- [ ] **Step 5: Commit the readiness correction**

  ```bash
  cd /tmp/agentteams-deepagents-runtime
  git add agentteams-controller/internal/backend/kubernetes.go \
    agentteams-controller/internal/backend/kubernetes_test.go
  git commit -m "fix(controller): keep Matrix readiness pending"
  ```

---

## Task 5: Document the Security Boundary and Run Static Release Gates

**Files:**

- Modify: `deepagents-agentteams/README.md`
- Modify: `docs/zh-cn/deepagents-runtime.md`
- Modify: `docs/zh-cn/local-kubernetes-deployment.md`
- Modify: `changelog/current.md`

- [ ] **Step 1: Update operator and runtime documentation**

  Document:

  - Runner startup is Linux-only and fails before listening if core-limit or non-dumpable hardening fails;
  - approved commands receive a minimal environment, closed inherited descriptors, and cannot inspect the Runner process token through `/proc`;
  - changing Worker ownership, UID, runtime, config, or execution mode revokes an existing lease in Service/Secret/Pod/NetworkPolicy/CR order;
  - `Ready=False` during first Matrix sync is expected `Starting`, and only explicit container failure state is terminal;
  - local verification commands test blocked token inspection without ever printing secrets.

  Do not claim the shell is a general side-effect sandbox or protected from intentionally killing PID 1.

- [ ] **Step 2: Record image-affecting commits in the changelog**

  Use `git log --oneline f9449738..HEAD` to obtain the exact commit hashes from Tasks 1–4. Add one linked bullet per logical change to `changelog/current.md`, using the repository commit URL and actual hash. Do not use abbreviated placeholders in the committed changelog.

- [ ] **Step 3: Run formatting, schema, chart, and full regression gates**

  ```bash
  cd /tmp/agentteams-deepagents-runtime/deepagents-agentteams
  uv run --locked --extra dev pytest -q
  uv run --locked --extra dev ruff check .

  cd /tmp/agentteams-deepagents-runtime/agentteams-controller
  gofmt -w internal/controller/execution_sandbox_controller.go \
    internal/controller/execution_sandbox_controller_test.go \
    internal/server/execution_sandbox_handler.go \
    internal/server/execution_sandbox_handler_test.go \
    internal/backend/kubernetes.go \
    internal/backend/kubernetes_test.go
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct go test ./... -count=1
  GOTMPDIR=/tmp/agentteams-go-tmp GOPROXY=https://proxy.golang.org,direct go vet ./...

  cd /tmp/agentteams-deepagents-runtime
  make check-crd-sync
  bash tests/check-helm-agentteams.sh
  bash tests/check-worker-management-deepagents.sh
  bash tests/check-installer-qwenpaw-release-gate.sh
  helm lint helm/agentteams
  git diff --check
  git status --short
  ```

  Expected: all Python tests/Ruff checks, all Go tests/vet, CRD sync, shell gates, Helm lint, and whitespace checks pass. The only intended unstaged files before the documentation commit are the four documentation/changelog files listed in this task.

- [ ] **Step 4: Commit documentation and changelog**

  ```bash
  cd /tmp/agentteams-deepagents-runtime
  git add deepagents-agentteams/README.md \
    docs/zh-cn/deepagents-runtime.md \
    docs/zh-cn/local-kubernetes-deployment.md \
    changelog/current.md
  git commit -m "docs(deepagents): document final isolation hardening"
  ```

- [ ] **Step 5: Run an independent review gate**

  Invoke `superpowers:requesting-code-review` over the complete diff from `f9449738` through HEAD. Resolve every Critical or Important finding using `superpowers:receiving-code-review`; for any code correction, repeat RED→GREEN and add a separate commit. Re-run the entire Step 3 verification set after review corrections. Do not proceed to image build while a Critical/Important finding is open.

---

## Task 6: Build Immutable Images and Deploy to the LAN kubeadm Cluster

**Files:**

- Read only: `/home/agent1/.config/agentteams/deployment-secrets.yaml`
- Read only: `helm/agentteams/values.yaml`
- Runtime state: Kubernetes namespace `agentteams-system`

- [ ] **Step 1: Capture a non-secret pre-deployment baseline**

  ```bash
  cd /tmp/agentteams-deepagents-runtime
  git status --short
  git rev-parse HEAD
  kubectl get nodes -o wide
  helm list -n agentteams-system
  kubectl -n agentteams-system get pods,manager,worker,executionsandbox -o wide
  kubectl -n agentteams-system get pods \
    -o custom-columns='NAME:.metadata.name,READY:.status.containerStatuses[*].ready,RESTARTS:.status.containerStatuses[*].restartCount,IMAGE:.spec.containers[*].image'
  ```

  Require a clean feature worktree, three Ready Kubernetes nodes (`agent0`, `agent2`, `agent3`), Helm release `agentteams`, and no unrelated failing workload before deployment.

- [ ] **Step 2: Build all coupled images from the final reviewed commit**

  ```bash
  cd /tmp/agentteams-deepagents-runtime
  HARDENING_TAG="dev-$(git rev-parse --short=8 HEAD)"
  make build-agentteams-controller build-manager build-worker \
    build-deepagents-worker build-deepagents-runner \
    VERSION="$HARDENING_TAG" \
    OPENCLAW_BASE_IMAGE=higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/openclaw-base \
    OPENCLAW_BASE_VERSION=20260423-8359cbc
  ```

  Verify all five local tags exist and inspect their image IDs. If a build fails, use `superpowers:systematic-debugging`; do not silently change base tags or use floating `latest`.

- [ ] **Step 3: Transfer exact images to both schedulable containerd nodes**

  Save the exact image set once, hash it, and transfer it to both schedulable nodes:

  ```bash
  cd /tmp/agentteams-deepagents-runtime
  HARDENING_TAG="dev-$(git rev-parse --short=8 HEAD)"
  IMAGE_ARCHIVE="/var/tmp/agentteams-images-${HARDENING_TAG}.tar"
  docker save -o "$IMAGE_ARCHIVE" \
    "agentteams/agentteams-controller:${HARDENING_TAG}" \
    "agentteams/manager:${HARDENING_TAG}" \
    "agentteams/worker-agent:${HARDENING_TAG}" \
    "agentteams/deepagents-worker:${HARDENING_TAG}" \
    "agentteams/deepagents-runner:${HARDENING_TAG}"
  sha256sum "$IMAGE_ARCHIVE"
  scp -i /home/agent1/.ssh/id_ed25519_ops "$IMAGE_ARCHIVE" agent2@10.13.36.138:"$IMAGE_ARCHIVE"
  scp -i /home/agent1/.ssh/id_ed25519_ops "$IMAGE_ARCHIVE" agent3@10.13.36.173:"$IMAGE_ARCHIVE"
  ```

  On each node, compare `sha256sum`, import with `sudo ctr -n k8s.io images import`, and remove only that exact temporary archive after successful import. Use `sudo crictl inspecti` to record the config digest for all five tags and assert both nodes match the local image config digest.

- [ ] **Step 4: Review/apply CRDs and atomically upgrade Helm**

  ```bash
  cd /tmp/agentteams-deepagents-runtime
  HARDENING_TAG="dev-$(git rev-parse --short=8 HEAD)"
  kubectl apply --server-side --dry-run=server -f helm/agentteams/crds/
  kubectl apply --server-side -f helm/agentteams/crds/
  helm upgrade --install agentteams helm/agentteams \
    --namespace agentteams-system \
    --reuse-values \
    -f /home/agent1/.config/agentteams/deployment-secrets.yaml \
    --set-string controller.image.repository=agentteams/agentteams-controller \
    --set-string controller.image.tag="$HARDENING_TAG" \
    --set-string manager.image.repository=agentteams/manager \
    --set-string manager.image.tag="$HARDENING_TAG" \
    --set-string worker.defaultImage.openclaw.repository=agentteams/worker-agent \
    --set-string worker.defaultImage.openclaw.tag="$HARDENING_TAG" \
    --set-string worker.defaultImage.deepagents.repository=agentteams/deepagents-worker \
    --set-string worker.defaultImage.deepagents.tag="$HARDENING_TAG" \
    --set-string deepagents.runnerImage.repository=agentteams/deepagents-runner \
    --set-string deepagents.runnerImage.tag="$HARDENING_TAG" \
    --atomic --timeout 15m
  ```

  Because Helm defaults do not rewrite existing Manager/Worker CR `spec.image`, patch the existing `manager/default` and `worker/deep-researcher` to the exact final tags where necessary, then wait for `manager/default` and `worker/deep-researcher` to report `Running`. Never expose Secret data while inspecting rollout state.

- [ ] **Step 5: Validate readiness mapping during the DeepAgents rollout**

  During the Worker restart, poll both Pod Ready condition and Worker status. When the Pod is Running but its Matrix exec-readiness probe is not yet true, assert the Worker remains `Starting` and is not set to `Failed`; after the first accepted Matrix sync and durable token write, assert Pod Ready and Worker `Running`. Also confirm explicit unit coverage for CrashLoop/ImagePull/terminated failure paths from Task 4 remains green.

- [ ] **Step 6: Live-test automatic revoke on mode change**

  With no command executing, ensure one disposable ExecutionSandbox exists for `deep-researcher`. Patch only `spec.runtimeConfig.deepagents.execution.mode` from `sandbox` to `disabled`. Assert:

  - Worker watch triggers without waiting for idle timeout;
  - Service and token Secret disappear before final sandbox removal;
  - while the Runner Pod still exists/terminates, its NetworkPolicy and ExecutionSandbox still exist;
  - after Pod disappearance, NetworkPolicy and ExecutionSandbox disappear;
  - no replacement Runner child is created.

  Restore only the execution mode to `sandbox`, wait for `worker/deep-researcher` to become Running, and confirm a later Ensure creates a fresh lease with a new immutable token Secret.

- [ ] **Step 7: Run a Human-approved token-boundary probe**

  Submit a new Matrix task whose approved shell action attempts to read the Runner parent `/proc` environment but prints only `BLOCKED` or `READABLE`, never the environment or token. Approve exactly once through the Human flow. Require:

  - output is `BLOCKED`;
  - Runner `/healthz` is HTTP 200;
  - exactly one `/v1/execute` request is recorded for the request ID;
  - no Runner, Matrix, Higress, MinIO, PostgreSQL, or Kubernetes credential appears in Worker/Runner logs.

- [ ] **Step 8: Re-run the successful end-to-end data path**

  In a fresh Matrix thread, request creation of a uniquely named text file containing a non-secret sentinel, approve the file write once, and verify:

  - exactly one successful `/v1/execute` call;
  - expected file content in the Worker workspace and MinIO object;
  - PostgreSQL contains the corresponding DeepAgents checkpoint/write records;
  - the Runner lease heartbeat advances during use;
  - the configured five-minute idle timeout deletes Service, Secret, Pod, NetworkPolicy, and ExecutionSandbox in the approved order.

- [ ] **Step 9: Re-run live fail-closed negative cases**

  Create and delete isolated test ExecutionSandbox CRs for: 9 GiB ephemeral-storage request, CPU request greater than limit, 500 ms duration, and invalid CIDR. Require `Failed` with the existing `InvalidResources` or `InvalidPolicy` reason and no surviving Service, Secret, Pod, or permissive NetworkPolicy. Confirm the normal `deep-researcher` Worker remains Running.

- [ ] **Step 10: Run final cluster and repository verification**

  Re-run all Task 5 static gates, then capture:

  ```bash
  kubectl -n agentteams-system get pods,manager,worker,executionsandbox -o wide
  kubectl -n agentteams-system get pods \
    -o custom-columns='NAME:.metadata.name,READY:.status.containerStatuses[*].ready,RESTARTS:.status.containerStatuses[*].restartCount,IMAGE:.spec.containers[*].image'
  helm list -n agentteams-system
  git status --short
  git log --oneline f9449738..HEAD
  ```

  Completion criteria: all platform Pods Ready, zero unexpected restarts, Worker and Manager Running on the final immutable tags, no stale ExecutionSandbox, Helm deployed, full repository gates green, and a clean worktree.

---

## Task 7: Integrate the Reviewed Branch Through GitHub

**Files:** None beyond any review-approved corrections.

- [ ] **Step 1: Verify branch scope and merge readiness**

  Use `superpowers:verification-before-completion` and `superpowers:finishing-a-development-branch`. Compare `codex/deepagents-agentteams-runtime` to `main`, verify no user-owned main-worktree documentation edits were copied or overwritten, and ensure the plan, approved design, implementation, tests, docs, and changelog are all committed.

- [ ] **Step 2: Publish and open the Pull Request**

  Use the `github:yeet` skill because the user explicitly approved push and Pull Request creation. Push `codex/deepagents-agentteams-runtime`, create a ready (non-draft) PR targeting `main`, and include:

  - the three closed audit findings;
  - test and static-gate evidence;
  - immutable image tag/digest evidence;
  - Matrix approval, `/proc` probe, MinIO, PostgreSQL, revoke, idle reclaim, and negative-case evidence;
  - rollback note stating no CRD schema change occurred.

- [ ] **Step 3: Merge locally only after the remote PR exists**

  Preserve the four pre-existing user-modified docs in `/home/agent1/CsAgnet`. If the PR branch and local `main` can be merged without touching those uncommitted files, perform the previously authorized local merge; otherwise stop and report the exact overlapping paths rather than stashing, resetting, or overwriting user work. Do not claim remote PR merge unless it actually occurred.
