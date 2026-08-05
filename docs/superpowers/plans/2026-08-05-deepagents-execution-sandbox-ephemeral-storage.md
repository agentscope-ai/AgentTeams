# DeepAgents ExecutionSandbox Ephemeral Storage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enforce auditable, overridable, cluster-capped ephemeral-storage requests and limits for DeepAgents ExecutionSandbox Runner Pods.

**Architecture:** Introduce ExecutionSandbox-specific resource API types and a stateless `internal/sandboxpolicy` resolver shared by the HTTP ensure path and reconciler. The HTTP path persists effective defaults into newly created CRs, while the reconciler independently protects directly-created CRs, applies the resolved Kubernetes resources to Runner Pods and both `emptyDir` volumes, and converges invalid CRs to a stable `Failed` state with no live Runner resources.

**Tech Stack:** Go 1.25, controller-runtime, Kubernetes resource quantities and core/networking APIs, controller-gen, Helm 3, YAML, Markdown

## Global Constraints

- This feature applies only to DeepAgents ExecutionSandbox Runner resources; do not add `ephemeralStorage` to the generic `AgentResourceValues` used by other runtimes.
- Helm defaults are exactly `defaultRequest: 256Mi`, `defaultLimit: 2Gi`, and `maxLimit: 8Gi`.
- Controller environment variables are exactly `AGENTTEAMS_DEEPAGENTS_SANDBOX_EPHEMERAL_STORAGE_DEFAULT_REQUEST`, `AGENTTEAMS_DEEPAGENTS_SANDBOX_EPHEMERAL_STORAGE_DEFAULT_LIMIT`, and `AGENTTEAMS_DEEPAGENTS_SANDBOX_EPHEMERAL_STORAGE_MAX_LIMIT`.
- Worker override JSON paths remain `runtimeConfig.deepagents.execution.resources.requests.ephemeralStorage` and `.limits.ephemeralStorage`.
- Missing request or limit values default independently; malformed, zero, or negative ephemeral-storage quantities are rejected; effective request must not exceed effective limit; effective limit must not exceed max limit.
- HTTP ensure returns `400 Bad Request` and creates no ExecutionSandbox for an invalid Worker override.
- A directly-created invalid ExecutionSandbox converges to `status.phase=Failed`, `Ready=False`, reason `InvalidResources`, creates no Runner resources, and deletes same-name token Secret, Pod, Service, and NetworkPolicy left from an older valid generation.
- A valid Runner container receives Kubernetes `ephemeral-storage` request and limit; both `/workspace` and `/tmp` `emptyDir.sizeLimit` values equal the effective limit.
- Existing CPU and memory JSON, MinIO workspace persistence, PostgreSQL checkpoints, Matrix state PVCs, and non-DeepAgents runtimes retain their current behavior.
- Every production-code behavior change follows red-green-refactor. Generated deepcopy and CRD files are regenerated only after source type changes.
- Any changes below `agentteams-controller/` must be recorded in `changelog/current.md` before the final implementation commit.
- Never read, log, copy, commit, or pass `/home/agent1/.config/agentteams/deployment-secrets.yaml` to a subagent.

---

### Task 1: ExecutionSandbox API types and policy resolver

**Files:**
- Modify: `agentteams-controller/api/v1beta1/types.go`
- Modify (generated): `agentteams-controller/api/v1beta1/zz_generated.deepcopy.go`
- Create: `agentteams-controller/internal/sandboxpolicy/ephemeral_storage.go`
- Create: `agentteams-controller/internal/sandboxpolicy/ephemeral_storage_test.go`
- Modify: `agentteams-controller/api/v1beta1/types_test.go`

**Interfaces:**
- Consumes: Kubernetes quantity strings from `DeepAgentsExecutionConfig.Resources` and `ExecutionSandboxSpec.Resources`.
- Produces: `v1beta1.ExecutionSandboxResourceRequirements`, `v1beta1.ExecutionSandboxResourceValues`, `sandboxpolicy.Policy`, `sandboxpolicy.New(defaultRequest, defaultLimit, maxLimit string) (Policy, error)`, `sandboxpolicy.Default() Policy`, and `Policy.Resolve(*ExecutionSandboxResourceRequirements) (*ExecutionSandboxResourceRequirements, corev1.ResourceRequirements, resource.Quantity, error)`.

- [ ] **Step 1: Write the API and resolver tests first**

Add a deepcopy assertion to `types_test.go` using this observable mutation:

```go
src.Spec.Resources = &ExecutionSandboxResourceRequirements{
    Requests: ExecutionSandboxResourceValues{EphemeralStorage: "512Mi"},
    Limits:   ExecutionSandboxResourceValues{EphemeralStorage: "4Gi"},
}
cloned := src.DeepCopy()
cloned.Spec.Resources.Requests.EphemeralStorage = "1Gi"
if src.Spec.Resources.Requests.EphemeralStorage != "512Mi" {
    t.Fatalf("ExecutionSandbox DeepCopy aliased resources: %#v", cloned.Spec.Resources)
}
```

Create table-driven resolver tests that independently assert these literal outcomes:

```text
nil input                         -> request 256Mi, limit 2Gi, sizeLimit 2Gi
request 512Mi only               -> request 512Mi, limit 2Gi
limit 4Gi only                   -> request 256Mi, limit 4Gi
request 512Mi and limit 4Gi      -> request 512Mi, limit 4Gi
input CPU 250m and memory 256Mi  -> CPU/memory preserved in corev1.ResourceRequirements
```

Also assert the input object is unchanged and that `corev1.ResourceEphemeralStorage` contains the expected request/limit. Add error cases for `garbage`, `0`, `-1Gi`, request `3Gi` with limit `2Gi`, and limit `9Gi`. Add constructor error cases for default request greater than default limit and default limit greater than max limit.

- [ ] **Step 2: Run the focused tests and capture the expected RED state**

Run:

```bash
cd agentteams-controller
go test ./api/v1beta1 ./internal/sandboxpolicy
```

Expected: compilation fails because the ExecutionSandbox resource types and `sandboxpolicy` package do not exist yet. This is the required RED evidence.

- [ ] **Step 3: Add the ExecutionSandbox-specific API types**

Add these exact JSON fields and change both DeepAgents pointers to the new type:

```go
type ExecutionSandboxResourceRequirements struct {
    Requests ExecutionSandboxResourceValues `json:"requests,omitempty"`
    Limits   ExecutionSandboxResourceValues `json:"limits,omitempty"`
}

type ExecutionSandboxResourceValues struct {
    CPU              string `json:"cpu,omitempty"`
    Memory           string `json:"memory,omitempty"`
    EphemeralStorage string `json:"ephemeralStorage,omitempty"`
}
```

`DeepAgentsExecutionConfig.Resources` and `ExecutionSandboxSpec.Resources` must both be `*ExecutionSandboxResourceRequirements`. Do not modify generic agent resource types.

- [ ] **Step 4: Implement the minimal stateless resolver**

Implement these constants and signatures in `internal/sandboxpolicy/ephemeral_storage.go`:

```go
const (
    DefaultRequest = "256Mi"
    DefaultLimit   = "2Gi"
    MaxLimit       = "8Gi"
)

type Policy struct {
    defaultRequest resource.Quantity
    defaultLimit   resource.Quantity
    maxLimit       resource.Quantity
}

func New(defaultRequest, defaultLimit, maxLimit string) (Policy, error)
func Default() Policy
func (p Policy) Resolve(in *v1beta1.ExecutionSandboxResourceRequirements) (
    *v1beta1.ExecutionSandboxResourceRequirements,
    corev1.ResourceRequirements,
    resource.Quantity,
    error,
)
```

`Default()` must call `New(DefaultRequest, DefaultLimit, MaxLimit)` and panic only if those compile-time defaults become invalid. The zero `Policy` value must behave like `Default()` so existing tests and callers that construct dependency structs by literal remain compatible. `Resolve` must copy the input, fill only empty ephemeral fields, parse CPU/memory with their existing permissive quantity semantics, require positive ephemeral quantities, enforce both ordering rules, return canonical quantity strings, and never mutate `in`.

- [ ] **Step 5: Regenerate deepcopy code and verify GREEN**

Run:

```bash
make generate
cd agentteams-controller
go test ./api/v1beta1 ./internal/sandboxpolicy
```

Expected: both packages pass; generated deepcopy methods include both new ExecutionSandbox resource types. `make generate` also updates both CRD directories; leave those generated changes for Task 4 validation.

- [ ] **Step 6: Commit Task 1**

Before staging, add this logical entry to `changelog/current.md`; Task 4 will attach the exact implementation commit links after all three code commits exist:

```markdown
- feat(controller): enforce cluster-capped ephemeral storage for DeepAgents Runner sandboxes
```

```bash
git add agentteams-controller/api/v1beta1/types.go \
  agentteams-controller/api/v1beta1/types_test.go \
  agentteams-controller/api/v1beta1/zz_generated.deepcopy.go \
  agentteams-controller/internal/sandboxpolicy \
  agentteams-controller/config/crd \
  helm/agentteams/crds \
  changelog/current.md
git commit -m "feat(controller): define sandbox ephemeral storage policy"
```

---

### Task 2: Controller configuration and HTTP ensure enforcement

**Files:**
- Modify: `agentteams-controller/internal/config/config.go`
- Modify: `agentteams-controller/internal/config/config_test.go`
- Modify: `agentteams-controller/internal/server/http.go`
- Modify: `agentteams-controller/internal/server/execution_sandbox_handler.go`
- Modify: `agentteams-controller/internal/server/execution_sandbox_handler_test.go`
- Modify: `agentteams-controller/internal/app/app.go`

**Interfaces:**
- Consumes: `sandboxpolicy.Policy` from Task 1 and the three exact environment variables from Global Constraints.
- Produces: `Config.DeepAgentsSandboxEphemeralStorage sandboxpolicy.Policy`, `ServerDeps.DeepAgentsSandboxEphemeralStorage sandboxpolicy.Policy`, and `NewExecutionSandboxHandler(client, namespace, defaultRuntime, policy)`.

- [ ] **Step 1: Write failing config and HTTP tests**

Extend the config test to set `512Mi`, `4Gi`, and `6Gi`, resolve a nil resource object, and assert those exact effective strings. Add a panic test using max `1Gi` while the default limit is `2Gi`.

In the handler test helper, pass `sandboxpolicy.Default()`. Extend the successful ensure test to assert persisted `256Mi` request and `2Gi` limit. Add a Worker override test for `512Mi/4Gi`. Add a table test for invalid Worker overrides (`0`, request `3Gi` with limit `2Gi`, and limit `9Gi`) that asserts HTTP 400 and verifies `client.Get` returns NotFound for the derived ExecutionSandbox name.

- [ ] **Step 2: Run the focused tests and capture RED**

```bash
cd agentteams-controller
go test ./internal/config ./internal/server
```

Expected: tests fail because config, handler, and ServerDeps do not yet carry the policy and ensure does not default or reject ephemeral storage.

- [ ] **Step 3: Load and validate the platform policy**

Add a private loader used by `LoadConfig`:

```go
func mustDeepAgentsSandboxEphemeralStoragePolicy() sandboxpolicy.Policy {
    policy, err := sandboxpolicy.New(
        envOrDefault("AGENTTEAMS_DEEPAGENTS_SANDBOX_EPHEMERAL_STORAGE_DEFAULT_REQUEST", sandboxpolicy.DefaultRequest),
        envOrDefault("AGENTTEAMS_DEEPAGENTS_SANDBOX_EPHEMERAL_STORAGE_DEFAULT_LIMIT", sandboxpolicy.DefaultLimit),
        envOrDefault("AGENTTEAMS_DEEPAGENTS_SANDBOX_EPHEMERAL_STORAGE_MAX_LIMIT", sandboxpolicy.MaxLimit),
    )
    if err != nil {
        panic(fmt.Sprintf("invalid DeepAgents sandbox ephemeral storage policy: %v", err))
    }
    return policy
}
```

Store its result in `Config.DeepAgentsSandboxEphemeralStorage`.

- [ ] **Step 4: Enforce and persist effective resources in HTTP ensure**

Carry the policy through `App -> ServerDeps -> ExecutionSandboxHandler`. Before constructing a new CR, call:

```go
effectiveResources, _, _, err := h.ephemeralStorage.Resolve(execution.Resources)
if err != nil {
    httputil.WriteError(w, http.StatusBadRequest, "invalid execution sandbox resources: "+err.Error())
    return
}
```

Set `ExecutionSandboxSpec.Resources` to `effectiveResources`. Existing identity-collision behavior and token-return behavior remain unchanged.

- [ ] **Step 5: Verify GREEN and regression coverage**

```bash
cd agentteams-controller
go test ./internal/config ./internal/server
go test ./internal/app
```

Expected: all focused packages pass.

- [ ] **Step 6: Commit Task 2**

```bash
git add agentteams-controller/internal/config \
  agentteams-controller/internal/server \
  agentteams-controller/internal/app/app.go
git commit -m "feat(controller): validate sandbox resources on ensure"
```

---

### Task 3: Reconciler failure convergence and Runner Pod limits

**Files:**
- Modify: `agentteams-controller/internal/controller/execution_sandbox_controller.go`
- Modify: `agentteams-controller/internal/controller/execution_sandbox_controller_test.go`
- Modify: `agentteams-controller/internal/app/app.go`

**Interfaces:**
- Consumes: `Config.DeepAgentsSandboxEphemeralStorage` and `sandboxpolicy.Policy.Resolve` from Tasks 1-2.
- Produces: Runner Pods with effective container resources and two limited emptyDirs; stable `InvalidResources` failure convergence for direct CRs.

- [ ] **Step 1: Write failing Pod and invalid-CR tests**

Extend `TestBuildExecutionSandboxResourcesAreHardenedAndSecretSafe` so its resolved resources contain `512Mi/4Gi` and assert:

```go
if got := container.Resources.Requests[corev1.ResourceEphemeralStorage]; got.String() != "512Mi" {
    t.Fatalf("ephemeral request=%q, want 512Mi", got.String())
}
if got := container.Resources.Limits[corev1.ResourceEphemeralStorage]; got.String() != "4Gi" {
    t.Fatalf("ephemeral limit=%q, want 4Gi", got.String())
}
for _, volume := range pod.Spec.Volumes {
    if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.String() != "4Gi" {
        t.Fatalf("volume %s sizeLimit=%v, want 4Gi", volume.Name, volume.EmptyDir)
    }
}
```

Add a reconciler test with a direct CR limit of `9Gi`. Seed same-name Secret, Pod, Service, and NetworkPolicy objects. After reconcile, assert all four are NotFound, phase is `Failed`, and the Ready condition is false with reason `InvalidResources`. Reconcile a second time and assert no error and no resources, proving stable convergence instead of an error loop.

- [ ] **Step 2: Run the controller tests and capture RED**

```bash
cd agentteams-controller
go test ./internal/controller -run 'ExecutionSandbox' -count=1
```

Expected: the Pod lacks ephemeral-storage request/limit and emptyDir limits; invalid direct CR returns an error or leaves seeded resources.

- [ ] **Step 3: Resolve before creating credentials or Runner resources**

Add `EphemeralStorage sandboxpolicy.Policy` to `ExecutionSandboxReconciler` and pass the configured policy from `app.go`. In `Reconcile`, resolve into the effective copy before `ensureExecutionSandboxToken`:

```go
effectiveResources, podResources, emptyDirLimit, err := r.EphemeralStorage.Resolve(effective.Spec.Resources)
if err != nil {
    return reconcile.Result{}, r.failInvalidResources(ctx, &sandbox, err)
}
effective.Spec.Resources = effectiveResources
```

Change `buildExecutionSandboxResources` to receive the already-resolved `corev1.ResourceRequirements` and `resource.Quantity`. Set the container `Resources` directly, and create both volumes as:

```go
sizeLimit := emptyDirLimit.DeepCopy()
corev1.EmptyDirVolumeSource{SizeLimit: &sizeLimit}
```

Use a separate copied quantity for each pointer.

- [ ] **Step 4: Converge invalid CRs without retry loops**

Implement `failInvalidResources` to delete same-name Secret, Pod, Service, and NetworkPolicy while ignoring NotFound. Then update status with:

```go
sandbox.Status.ObservedGeneration = sandbox.Generation
sandbox.Status.Phase = "Failed"
sandbox.Status.Endpoint = ""
sandbox.Status.PodName = ""
apiMeta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
    Type:               "Ready",
    Status:             metav1.ConditionFalse,
    ObservedGeneration: sandbox.Generation,
    Reason:             "InvalidResources",
    Message:            err.Error(),
})
```

Only Kubernetes delete/status failures return reconcile errors; the invalid user configuration itself returns nil after convergence.

- [ ] **Step 5: Verify GREEN, full controller tests, and vet**

```bash
cd agentteams-controller
go test ./internal/controller -run 'ExecutionSandbox' -count=1
go test ./internal/... ./cmd/agt/...
go vet ./internal/... ./cmd/agt/...
```

Expected: all commands exit 0.

- [ ] **Step 6: Commit Task 3**

```bash
git add agentteams-controller/internal/controller/execution_sandbox_controller.go \
  agentteams-controller/internal/controller/execution_sandbox_controller_test.go \
  agentteams-controller/internal/app/app.go
git commit -m "feat(controller): enforce runner ephemeral storage limits"
```

---

### Task 4: Helm contract, CRD sync, docs, and changelog

**Files:**
- Modify: `helm/agentteams/values.yaml`
- Modify: `helm/agentteams/templates/controller/deployment.yaml`
- Modify: `agentteams-controller/config/crd/workers.agentteams.io.yaml`
- Modify: `agentteams-controller/config/crd/executionsandboxes.agentteams.io.yaml`
- Modify: `helm/agentteams/crds/workers.agentteams.io.yaml`
- Modify: `helm/agentteams/crds/executionsandboxes.agentteams.io.yaml`
- Modify: `docs/zh-cn/deepagents-runtime.md`
- Modify: `docs/zh-cn/local-kubernetes-deployment.md`
- Modify: `changelog/current.md`

**Interfaces:**
- Consumes: exact Helm keys and environment variables from Global Constraints; Task 1 generated CRD schema; Task 1-3 commit hashes.
- Produces: installable chart defaults, synchronized CRDs, documented Worker overrides, and image-change release notes.

- [ ] **Step 1: Add Helm defaults and Controller environment rendering**

Under `deepagents.sandbox`, add:

```yaml
ephemeralStorage:
  defaultRequest: 256Mi
  defaultLimit: 2Gi
  maxLimit: 8Gi
```

Render the three exact environment variables with quoted values in the Controller Deployment. Keep them unconditional so an enabled Controller always has a valid policy, even before DeepAgents is enabled.

- [ ] **Step 2: Regenerate and verify CRD schema**

```bash
make generate
make check-crd-sync
```

Inspect both Worker and ExecutionSandbox schemas and confirm `ephemeralStorage` exists only below their ExecutionSandbox-specific request/limit paths. Generic Worker/Manager `spec.resources` must not gain the field.

- [ ] **Step 3: Update both Chinese deployment guides**

In both DeepAgents Worker examples add request `ephemeralStorage: 512Mi` and limit `ephemeralStorage: 4Gi`. Document the `256Mi/2Gi/8Gi` default/default/max relationship, aggregate container ephemeral-storage enforcement, both `emptyDir.sizeLimit` values, HTTP/direct-CR invalid behavior, and local-path's node-local/non-HA boundary. Do not include credentials or API keys.

- [ ] **Step 4: Record image-affecting changes in the changelog**

Run:

```bash
git log --format='[%h](https://github.com/agentscope-ai/AgentTeams/commit/%H)' \
  --reverse b76d0918..HEAD
```

Append the three exact Markdown links printed by that command to the existing `feat(controller)` bullet in `changelog/current.md`, so every image-affecting implementation commit is traceable.

- [ ] **Step 5: Render and validate the Helm contract**

```bash
helm lint helm/agentteams \
  --set-string credentials.llmApiKey=validation-only \
  --set-string gateway.publicURL=http://10.13.36.129:18080 \
  --set deepagents.enabled=true

helm template agentteams helm/agentteams \
  --namespace agentteams-system \
  --set-string credentials.llmApiKey=validation-only \
  --set-string gateway.publicURL=http://10.13.36.129:18080 \
  --set deepagents.enabled=true \
  > /tmp/agentteams-ephemeral-storage-render.yaml
```

Inspect the rendered Controller env and verify the three exact values. Apply the rendered CRDs to a disposable server-side dry-run or use `kubectl apply --dry-run=server` for representative Worker and ExecutionSandbox manifests containing `ephemeralStorage`.

- [ ] **Step 6: Run final static and regression verification**

```bash
make check-crd-sync
git diff --check
cd agentteams-controller
go test ./internal/... ./cmd/agt/... ./api/v1beta1
go vet ./internal/... ./cmd/agt/...
```

Expected: every command exits 0. Remove only the disposable rendered file after evidence is captured.

- [ ] **Step 7: Commit Task 4**

```bash
git add helm/agentteams \
  agentteams-controller/config/crd \
  docs/zh-cn/deepagents-runtime.md \
  docs/zh-cn/local-kubernetes-deployment.md \
  changelog/current.md
git commit -m "feat(helm): configure sandbox ephemeral storage policy"
```

---

## Post-plan deployment and E2E gate

After all four tasks pass their task reviews and the whole-branch review is clean, the primary agent—not a subagent—must:

1. Build new immutable Controller, Manager, and default Worker images from final HEAD because Manager/Worker embed the Controller-built `agt` CLI. Retag/reuse the previously validated DeepAgents Worker and Runner images only when source-diff evidence proves both DeepAgents build contexts are unchanged.
2. Import every required immutable image into both schedulable nodes and verify exact image IDs with `crictl`.
3. Install CRDs and the Helm release using `local-path`, the LAN gateway URL, and `/home/agent1/.config/agentteams/deployment-secrets.yaml` without printing secret values.
4. Verify all PVCs Bound, all platform workloads Ready, DeepSeek preflight success, and a DeepAgents Worker with `512Mi/4Gi` limits.
5. Verify a direct `9Gi` ExecutionSandbox becomes `Failed/InvalidResources` with no Runner resources.
6. Execute Matrix task -> Human approval -> Runner command -> MinIO workspace sync -> PostgreSQL checkpoint -> idle/max-lifetime reclaim, capturing resource status and logs without credentials.
7. Run completion verification, final review, merge safely around the user's dirty main worktree, push the feature branch, create the Pull Request, and then perform the user-authorized local main merge only if it does not overwrite user-owned changes.
