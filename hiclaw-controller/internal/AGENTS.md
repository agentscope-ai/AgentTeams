# Controller Internal Navigation Guide

This file helps AI Agents (and human developers) quickly understand the internal package structure of the AgentTeams Kubernetes operator. It complements the root [AGENTS.md](../../AGENTS.md); read that first for overall project structure.

Last reviewed: 2026-07-22

## Scope of this guide

| In scope | Out of scope |
|---|---|
| `hiclaw-controller/internal/` (all 29 packages) | Helm chart (`helm/`) |
| `hiclaw-controller/cmd/` (controller + CLI binaries) | Worker/Manager runtimes (copaw, hermes, openclaw, etc.) |
| `hiclaw-controller/api/v1beta1/` (CRD types) | Agent-facing content (`manager/agent/`) |
| `hiclaw-controller/test/` (integration tests) | Install scripts (`install/`) |

## Layered Architecture

```
cmd/controller/main.go ──► app/app.go (wiring + startup)
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
        controller/       server/         initializer/
        (reconcilers)     (HTTP API)      (bootstrap)
              │               │
              ▼               ▼
           service/        service/
        (domain logic)  (domain logic)
              │               │
    ┌─────┬──┴──┬─────┐      │
    ▼     ▼     ▼     ▼      ▼
backend/ oss/ matrix/ gateway/ executor/
```

## Package Ownership Map

| Package | Responsibility | Key files |
|---------|---------------|-----------|
| `app/` | Application wiring, dependency injection, startup | `app.go` |
| `controller/` | CRD reconcilers (Worker, Manager, Team, Human, Project) | `worker_controller.go`, `team_controller.go`, `member_reconcile.go` |
| `server/` | HTTP API handlers (REST CRUD, appservice, lifecycle, health) | `resource_handler.go`, `http.go`, `appservice_handler.go` |
| `service/` | Domain services and interfaces (Deployer, Provisioner) | `interfaces.go`, `deployer.go`, `provisioner.go` |
| `backend/` | Container runtime abstraction (Docker, Kubernetes, Sandbox) | `interface.go`, `docker.go`, `kubernetes.go`, `sandbox.go` |
| `auth/` | Authentication, authorization, middleware | `authenticator.go`, `authorizer.go`, `middleware.go` |
| `matrix/` | Matrix homeserver client + appservice integration | `client.go`, `appservice.go` |
| `gateway/` | Higress AI Gateway client (routes, consumers, MCP) | `higress.go`, `aigateway.go`, `client.go` |
| `oss/` | Object storage (MinIO) client + admin operations | `minio.go`, `minio_admin.go` |
| `executor/` | Package management + Nacos AI service | `package.go`, `nacos_ai_service.go` |
| `config/` | Controller configuration loading and derived types | `config_load.go`, `config_types.go`, `config_derived.go` |
| `agentconfig/` | openclaw.json generation, mcporter config | `generator.go`, `mcporter.go` |
| `accessresolver/` | Worker access resolution (team/project scope) | `resolver.go`, `defaults.go` |
| `apiserver/` | Embedded API server (Matrix + console proxy) | `embedded.go` |
| `credentials/` | STS credential types | `sts.go`, `types.go` |
| `credprovider/` | Cloud credential providers (Aliyun, kubeconfig) | `client.go`, `aliyun_credential.go` |
| `httputil/` | HTTP response helpers | `response.go` |
| `initializer/` | First-boot initialization (Matrix admin, gateway setup) | `initializer.go` |
| `mail/` | SMTP notification client | `smtp.go` |
| `managerstate/` | Manager workspace state management | `state.go` |
| `metrics/` | Prometheus metrics registration | `metrics.go` |
| `migration/` | Data migration helpers | `migration.go` |
| `proxy/` | Reverse proxy utilities | `proxy.go` |
| `remoteclient/` | Remote Kubernetes client factory | `client.go` |
| `slicesx/` | Generic slice utilities | `slicesx.go` |
| `store/` | Persistent state store abstraction | `store.go` |
| `validation/` | CRD field validation webhooks | `validation.go` |
| `watcher/` | File/config watchers | `watcher.go` |
| `workerdeps/` | Worker dependency resolution | `workerdeps.go` |

## Reconciler Ownership

| CRD | Controller file | Helper files |
|-----|----------------|--------------|
| Worker | `controller/worker_controller.go` | `member_reconcile.go`, `member_reconcile_service.go` |
| Team | `controller/team_controller.go` | `team_members_*.go`, `team_rooms.go`, `team_status.go` |
| Manager | `controller/manager_controller.go` | `manager_reconcile_*.go` |
| Human | `controller/human_controller.go` | `human_reconcile_*.go`, `humanidentity/` |
| Project | `controller/project_controller.go` | — |

## High-Risk Areas

- **CRD type changes** (`api/v1beta1/`): require `make generate` to regenerate `zz_generated.deepcopy.go` and `make sync-crds` to update Helm CRDs.
- **`service/provisioner.go` / `service/deployer.go`**: create/destroy containers — side effects on Docker/K8s state.
- **`config/`**: changes affect all runtime derivations (openclaw, copaw, hermes, qwenpaw, openhuman).
- **`backend/`**: container lifecycle operations; test with `make test-unit` (mocked) and `make test-integration` (envtest).
- **`gateway/higress.go`**: external API calls to Higress console; auth plugin has ~40s activation delay.

## Validation

```bash
# Unit tests (fast, no external deps)
make -C hiclaw-controller test-unit

# Integration tests (requires envtest / kubebuilder assets)
make -C hiclaw-controller test-integration

# All tests
make -C hiclaw-controller test-all
```

## Common Modification Paths

**To add a new Worker spec field:**
1. Add field to `api/v1beta1/worker_types.go`
2. Run `make generate && make sync-crds`
3. Update `controller/member_reconcile.go` or `controller/worker_controller.go`
4. Update `service/` or `backend/` if the field affects provisioning
5. Run `make -C hiclaw-controller test-unit`

**To add a new REST endpoint:**
1. Add handler in `server/` (follow `resource_handler.go` pattern)
2. Register route in `server/http.go`
3. Add service method in `service/` if business logic is needed
4. Run `make -C hiclaw-controller test-unit`

**To add a new CRD:**
1. Define types in `api/v1beta1/`
2. Run `make generate && make sync-crds`
3. Create reconciler in `controller/`
4. Register in `app/app.go`
5. Add Helm CRD template in `helm/hiclaw/crds/`
6. Run `make -C hiclaw-controller test-all`
