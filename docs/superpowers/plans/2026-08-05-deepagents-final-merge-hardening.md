# DeepAgents final merge hardening implementation plan

> Follow-up to the whole-branch review of `bd60a9df..d45d45ca`: 0 Critical,
> 5 Important, and 1 Minor findings. Build and deployment remain blocked until
> every task below passes implementation and independent task review.

## Task 1: Crash-safe Matrix delivery and Runner ambiguity handling

**Files:**
- Modify `deepagents-agentteams/src/deepagents_agentteams/matrix.py`
- Modify `deepagents-agentteams/src/deepagents_agentteams/sandbox.py`
- Modify relevant Python tests and documentation comments

- [ ] RED: prove a sync event is durably journaled before the cursor advances,
  survives restart after cursor persistence, and is not automatically executed
  twice when a crash occurs while its handler is in progress.
- [ ] Add a state-PVC-backed, atomically persisted event journal keyed by Matrix
  event ID. States must distinguish pending, processing, and completed.
- [ ] Drain pending events before requesting another sync. Mark processing
  durably before invoking the handler and completed only after success. A
  recovered processing event is fail-closed as an unknown outcome and is never
  automatically re-executed; notify the Matrix thread and then close it out.
- [ ] Move read markers after successful or explicit unknown-outcome handling.
  Keep the sync token fsync/rename/directory-fsync contract.
- [ ] Remove the Worker's automatic post-send Runner transport retry. One
  ambiguous transport result must return the existing unknown-outcome response
  after exactly one POST, so a replacement Pod cannot re-execute the command.
- [ ] Preserve heartbeat 404/410 reacquisition before a Runner request.
- [ ] Run focused and full Python tests and ruff; commit as
  `fix(deepagents): make delivery crash-safe`.

## Task 2: Live managed-Agent approval authorization

**Files:**
- Modify controller authenticated routes/handlers and tests
- Modify `deepagents-agentteams/src/deepagents_agentteams/engine.py`
- Modify `deepagents-agentteams/src/deepagents_agentteams/cli.py`
- Modify relevant Python configuration/engine tests

- [ ] RED: start a Worker with a coordinator not present in the snapshot, make
  that Matrix ID a managed Worker/Manager after startup, and prove approval is
  rejected without restarting or reloading the Worker.
- [ ] Add a bounded authenticated controller lookup that answers whether one
  exact Matrix user ID currently belongs to a managed Worker or Manager. Do not
  expose credentials or an unrestricted identity list.
- [ ] Authenticate the adapter lookup with the projected Worker ServiceAccount
  token read fresh for each request. A lookup failure must fail closed for an
  approval decision.
- [ ] Keep the projected global `matrix.agentUserIds` snapshot as defense in
  depth, but consult the live controller-owned answer before every approval.
- [ ] Preserve requester/team-admin/coordinator Human authorization only after
  the live Agent denial check.
- [ ] Run focused/full Go and Python tests plus vet/ruff; commit as
  `fix(deepagents): authorize approvals against live identities`.

## Task 3: Fail-closed sandbox policy convergence and strict Worker JSON

**Files:**
- Modify `agentteams-controller/internal/controller/execution_sandbox_controller.go`
- Modify controller tests
- Modify `agentteams-controller/internal/server/resource_handler.go`
- Modify server tests

- [ ] RED: valid-to-invalid updates for idle timeout, max lifetime, requested
  egress, and configured egress ceilings must currently leave seeded children;
  prove the desired cleanup/status behavior first.
- [ ] Route invalid duration and egress policies through a common fail-closed
  convergence path: delete Secret/Pod/Service/NetworkPolicy, clear endpoint and
  Pod name, observe the generation, and set stable `Failed`, `Ready=False`,
  `InvalidPolicy`. Preserve `InvalidResources` for compute/storage failures.
- [ ] The second reconcile of unchanged invalid policy must be stable.
- [ ] Add a bounded shared strict JSON decoder for Worker create/update:
  unknown top-level or nested fields, trailing JSON, empty bodies, and oversized
  bodies return 400 without creating or mutating a Worker.
- [ ] Do not change unrelated REST resources unless covered by explicit tests.
- [ ] Run focused/full Go tests and vet; commit as
  `fix(controller): converge invalid sandbox policies`.

## Task 4: Upgrade ordering, contracts, changelog, and full regression

**Files:**
- Modify `docs/zh-cn/deepagents-runtime.md`
- Modify `docs/zh-cn/local-kubernetes-deployment.md`
- Modify `deepagents-agentteams/README.md` if needed
- Modify `changelog/current.md`

- [ ] Put reviewed server-side CRD diff/apply before `helm upgrade`; explain
  compatibility/rollback and that Helm does not upgrade existing CRDs.
- [ ] Document the durable Matrix journal/unknown-outcome boundary, exactly-one
  Runner POST after an ambiguous result, and live controller identity denial.
- [ ] Add exact commit links for Tasks 1-3 to the DeepAgents changelog entries.
- [ ] Run CRD sync, Helm enabled/disabled lint/template and contract checks,
  full Go tests/vet, full Python tests/ruff, and `git diff --check`.
- [ ] Commit as `docs(deepagents): finalize merge hardening`.

## Post-plan gates

1. Independent task review after every task, with all Critical/Important issues fixed.
2. Fresh whole-branch review of the integration range.
3. Apply/dry-run CRDs and representative Worker/ExecutionSandbox objects.
4. Build and import immutable Controller, Manager, default Worker, DeepAgents
   Worker, and Runner images on both schedulable nodes.
5. Helm deploy using the protected values file, DeepSeek preflight, positive
   and negative policy checks, and Matrix -> Human approval -> Runner -> MinIO
   -> PostgreSQL -> reclaim E2E.
6. Final verification, feature push, Pull Request, and safe local `main` merge.
