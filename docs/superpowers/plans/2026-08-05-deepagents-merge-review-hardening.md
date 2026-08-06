# DeepAgents Merge-Review Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` to implement this plan task-by-task with independent review gates.

**Goal:** Close every confirmed Critical/Important merge-review finding before building or deploying the DeepAgents AgentTeams runtime.

**Architecture:** Keep the vendored `deepagents/` subtree unchanged. Harden the AgentTeams adapter and controller contracts at their existing boundaries: controller-projected runtime documents, typed adapter config, REST/`agt` Worker CRUD, ExecutionSandbox ensure/reconcile, and Kubernetes Worker readiness. Prefer explicit rejection for unsupported OSS storage over a partial compatibility layer.

**Tech Stack:** Go 1.25, controller-runtime, Kubernetes core APIs, Cobra, Python 3.11+, pytest, LangChain DeepAgents, matrix-nio, Helm 3.

## Global Constraints

- Do not modify the vendored `deepagents/` subtree unless a defect cannot be fixed in `deepagents-agentteams/`; record any unavoidable patch in `deepagents-agentteams/UPSTREAM_PATCHES.md`.
- Preserve Matrix Human-only approval, room boundaries, MinIO object-key validation, checkpoint encryption, stable Runner request IDs, and fail-closed ambiguous execution behavior.
- The controller/adapter model URL contract must yield an OpenAI-compatible base ending in exactly one `/v1` for the managed Higress root, while preserving explicitly versioned/non-root external provider paths.
- DeepAgents is supported with `storage.provider=minio` in this release. Helm and the controller runtime projection must reject `storage.provider=oss` rather than silently constructing a MinIO client for OSS.
- Every current managed Agent Matrix identity visible to the controller (Managers and Workers, including Team Leaders) must be projected into `matrix.agentUserIds`; the adapter must never classify an ID in that set as Human, even when it is listed as a coordinator.
- `fileWrites: required` covers `write_file`, `edit_file`, and `delete`.
- `spec.identity`, `spec.soul`, and `spec.agents` must affect the DeepAgents system prompt. Unsupported package/skill behavior must be documented explicitly, not silently advertised.
- REST Worker create/update must preserve `runtimeConfig`; `agt create/update worker` must expose a safe DeepAgents sandbox path and an advanced runtime-config file path. Manager-facing instructions must use it.
- Existing sandboxes must converge to updated Worker execution resources, egress, idle timeout, and max lifetime before a Ready token is returned for the new generation.
- A reclaimed sandbox lease is reacquired only before a Runner request. Ambiguous post-request failures remain fail-closed and must never trigger a fresh request ID.
- A DeepAgents Worker Pod is Ready only after the first valid Matrix sync is accepted and its sync token is durable.
- Follow red-green-refactor for every behavior change. Run focused tests after each fix and the full Go/Python/Helm regression gate before deployment.
- On agent1, export `GOTMPDIR=/var/tmp/agentteams-go-tmp GOPROXY=https://goproxy.cn,direct` before Go, `make generate`, or `go vet` commands.
- Record image-affecting changes in `changelog/current.md` with exact commit links before the final hardening commit.
- Never read, log, copy, commit, or pass `/home/agent1/.config/agentteams/deployment-secrets.yaml` to a subagent.

---

### Task 1: Canonical model gateway and supported storage contract

**Files:**
- Modify: `agentteams-controller/internal/service/runtime_config.go`
- Modify: `agentteams-controller/internal/service/deployer_test.go`
- Modify: `deepagents-agentteams/src/deepagents_agentteams/gateway.py`
- Modify: `deepagents-agentteams/tests/test_gateway.py`
- Modify: `helm/agentteams/templates/00-validate.yaml`
- Modify: `tests/check-helm-agentteams.sh`

- [ ] Add failing Go tests proving the managed gateway root becomes `/v1`, an existing `/v1` is not duplicated, and a non-root external provider URL is preserved.
- [ ] Add failing Python tests proving `ChatOpenAI` receives a canonical OpenAI base and legacy root runtime documents are safely normalized.
- [ ] Add failing Helm and deployer tests proving DeepAgents + OSS is rejected with an actionable error while non-DeepAgents OSS remains valid.
- [ ] Implement one URL canonicalization rule at each trust boundary and an explicit DeepAgents/MinIO compatibility check.
- [ ] Run focused Go, Python, and Helm contract tests; then commit as `fix(deepagents): enforce gateway and storage contracts`.

---

### Task 2: Human approval identity, inline prompt, and delete coverage

**Files:**
- Modify: `agentteams-controller/internal/controller/member_reconcile.go`
- Modify: `agentteams-controller/internal/controller/worker_controller.go`
- Modify: `agentteams-controller/internal/controller/team_controller.go`
- Modify: `agentteams-controller/internal/service/deployer.go`
- Modify: `agentteams-controller/internal/service/runtime_config.go`
- Modify relevant controller/service tests
- Modify: `deepagents-agentteams/src/deepagents_agentteams/config.py`
- Modify: `deepagents-agentteams/src/deepagents_agentteams/graph.py`
- Modify: `deepagents-agentteams/tests/test_config.py`
- Modify: `deepagents-agentteams/tests/test_graph.py`

- [ ] Add failing controller tests with an unrelated Worker Matrix ID outside the current Team and verify it is projected into `matrix.agentUserIds` for standalone and Team DeepAgents runtime documents.
- [ ] Add a deterministic, sorted, de-duplicated controller helper that lists current Worker and Manager status Matrix IDs; pass the snapshot into every DeepAgents runtime-config deployment path. Keep the Manager fallback identity.
- [ ] Add failing adapter tests showing an unrelated Agent coordinator is never in `human_approver_ids`.
- [ ] Add failing graph tests showing `fileWrites: required` includes `delete`, and `identity`/`soul`/`agents` content appears in a structured system prompt without changing the fixed AgentTeams security boundary text.
- [ ] Parse `desired.inlineConfig` into typed immutable config and compose it with explicit section boundaries. Do not interpret package/skill fields in this task.
- [ ] Run focused controller/service and adapter config/graph tests; commit as `fix(deepagents): harden approval identity and prompt policy`.

---

### Task 3: Manager-capable Worker runtimeConfig CRUD

**Files:**
- Modify: `agentteams-controller/internal/server/types.go`
- Modify: `agentteams-controller/internal/server/resource_handler.go`
- Modify relevant server tests
- Modify: `agentteams-controller/cmd/agt/create.go`
- Modify: `agentteams-controller/cmd/agt/update.go`
- Modify/add CLI tests
- Modify: `manager/agent/skills/worker-management/SKILL.md`
- Modify: `manager/agent/skills/worker-management/references/create-worker.md`
- Modify: `manager/agent/skills/worker-management/scripts/update-worker-config.sh`

- [ ] Add failing REST tests proving create/update persist `runtimeConfig` and Worker responses return the non-secret runtime config.
- [ ] Add failing CLI tests for `--runtime-config-file` (JSON or YAML), invalid files, and a convenience DeepAgents sandbox configuration with Human approval required for file writes and MCP by default.
- [ ] Add `runtimeConfig` to create/update/response types and preserve pointer semantics. On update, omit means unchanged.
- [ ] Expose an advanced `--runtime-config-file`; add a safe DeepAgents convenience path that enables `execution.mode=sandbox`, requires Human approval for file writes/MCP, and accepts explicit coordinator Matrix IDs. It must not silently invent a Human identity when neither flags nor AgentTeams admin env can supply one.
- [ ] Update Manager-facing second-person instructions so creating or switching to DeepAgents supplies execution/approval configuration and warns that advanced egress/resources use a manifest/config file.
- [ ] Run focused server/CLI/script tests; commit as `fix(manager): configure DeepAgents sandbox workers`.

---

### Task 4: ExecutionSandbox policy refresh and lease replacement

**Files:**
- Modify: `agentteams-controller/internal/server/execution_sandbox_handler.go`
- Modify: `agentteams-controller/internal/server/execution_sandbox_handler_test.go`
- Modify controller tests only if reconciliation coverage needs extension
- Modify: `deepagents-agentteams/src/deepagents_agentteams/sandbox.py`
- Modify: `deepagents-agentteams/tests/test_sandbox.py`

- [ ] Add failing handler tests for an existing Ready sandbox after Worker execution resources, egress, idle timeout, or max lifetime changes. The handler must update the CR and return Pending without a token until `status.observedGeneration` catches up.
- [ ] Build the desired ExecutionSandbox spec for both create and existing paths, resolve resources before mutation, preserve identity collision checks, and update only when the policy envelope differs.
- [ ] Confirm reconciler tests prove stale immutable Pods/Services are recreated and NetworkPolicy is updated after the spec change.
- [ ] Add failing adapter tests where heartbeat returns 404/410 before a Runner request. Clear the cached lease, ensure/hydrate a replacement exactly once, then issue the request. Do not reacquire after a Runner request or ambiguous transport error.
- [ ] Run focused server/controller and sandbox tests; commit as `fix(deepagents): refresh execution sandbox leases and policy`.

---

### Task 5: ExecutionSandbox CPU and memory validation convergence

**Files:**
- Modify: `agentteams-controller/internal/sandboxpolicy/ephemeral_storage.go`
- Modify: `agentteams-controller/internal/sandboxpolicy/ephemeral_storage_test.go`
- Modify: `agentteams-controller/internal/server/execution_sandbox_handler_test.go`
- Modify: `agentteams-controller/internal/controller/execution_sandbox_controller_test.go`

- [ ] Add failing resolver tests for negative CPU/memory requests or limits and request greater than limit. Preserve zero and one-sided CPU/memory values if Kubernetes accepts them, and preserve all valid existing quantity formats.
- [ ] Add failing HTTP tests proving an invalid current Worker CPU/memory sandbox policy returns 400 and creates or mutates no ExecutionSandbox.
- [ ] Add a direct-CR reconciler test with invalid CPU/memory and seeded Secret/Pod/Service/NetworkPolicy; it must converge to `Failed/InvalidResources`, delete all siblings, and remain stable on a second reconcile.
- [ ] Extend the shared sandbox policy resolver to require non-negative CPU/memory quantities and request <= limit when both sides are present. Keep ephemeral-storage defaults/cap behavior unchanged.
- [ ] Run focused sandboxpolicy/server/controller tests and vet; commit as `fix(controller): converge invalid sandbox compute resources`.

---

### Task 6: Matrix-synchronized readiness, docs, changelog, and full regression

**Files:**
- Modify: `deepagents-agentteams/src/deepagents_agentteams/matrix.py`
- Modify: `deepagents-agentteams/src/deepagents_agentteams/cli.py`
- Modify relevant Python tests
- Modify: `agentteams-controller/internal/backend/kubernetes.go`
- Modify: `agentteams-controller/internal/backend/kubernetes_test.go`
- Modify: `deepagents-agentteams/README.md`
- Modify: `deepagents-agentteams/src/deepagents_agentteams/__init__.py`
- Modify: `docs/zh-cn/deepagents-runtime.md`
- Modify: `docs/zh-cn/local-kubernetes-deployment.md`
- Modify: `changelog/current.md`

- [ ] Add failing Matrix/CLI tests proving readiness is not signalled before a valid sync response is accepted and its sync token is durably saved; signal once after either initial catch-up or incremental resume.
- [ ] Add a DeepAgents-only Kubernetes exec readiness probe against a file under the per-Pod `/tmp` emptyDir. The CLI removes stale state at startup and creates the file only from the post-sync callback.
- [ ] Add backend tests proving other runtimes retain existing probes/template behavior.
- [ ] Remove the two extra EOF blank lines and make `git diff --check` clean.
- [ ] Document: MinIO-only support in this release, canonical `/v1` contract, global Agent deny-list, inline prompt support, unsupported package/skill behavior, Manager CLI runtime-config flow, sandbox policy refresh/reacquisition, and Matrix-synchronized readiness.
- [ ] Add exact commit links for Tasks 1-5 to the existing DeepAgents changelog entries.
- [ ] Run `make check-crd-sync`, Helm lint/template enabled+disabled, full controller/API/CLI tests and vet, full adapter pytest suite, `git diff --check`, and image-context diff checks.
- [ ] Commit as `fix(deepagents): gate readiness on Matrix synchronization`.

---

## Post-plan gates

After every task passes independent task review:

1. Run a new whole-branch merge-readiness review and close every Critical/Important issue.
2. Primary agent performs Kubernetes server-side dry-runs for CRDs and representative Worker/ExecutionSandbox manifests.
3. Build immutable Controller, Manager, default Worker, and any changed DeepAgents images; import exact images into both schedulable nodes.
4. Deploy Helm with the protected secret file, run DeepSeek preflight, positive and negative ExecutionSandbox checks, Matrix -> Human approval -> Runner -> MinIO -> PostgreSQL -> reclaim E2E, and capture non-secret evidence.
5. Run final verification, push feature branch, create Pull Request, and safely integrate with local `main` without overwriting user-owned changes.
