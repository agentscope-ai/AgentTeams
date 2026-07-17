# Feature Proposals — Post-Reshape

> **Status:** Draft for Caspar · **Date:** 2026-07-16
> Each feature has: concept, insertion points with `file:line` anchors, rough code shape,
> and what it buys you. Not implementation — just enough to pick up and build.

---

## Feature A — Worker Output Verification

### Problem
Workers self-report task completion by writing `result.md` and updating `meta.json` status=completed.
Nobody checks whether the claimed deliverables actually exist or are non-empty. This is exactly the
AI-Pawtner PM problem: "ready for deployment" when tsc has 86 errors. The task lifecycle trusts the
worker's word.

### Concept
Add a lightweight verification step between "worker says done" and "manager marks completed."
When a worker reports a finite task as completed, the Manager (or Team Lead) runs a quick check
against the task's `meta.json` + `result.md` + claimed deliverables before accepting the completion.

This is NOT the Integration Auditor (that runs in Gitea CI on PRs). This is inside the task lifecycle —
catching "I'm done" claims that have no evidence before they hit `state.json` as completed.

### Insertion points

**1. Task completion flow** — `manager/agent/skills/task-management/references/finite-tasks.md:93`
Current step 2 is:
> Update `meta.json`: status=completed, fill completed_at. Push back to MinIO.

Insert a verification step between "worker pushes results" (step 1) and "manager marks completed" (step 2):

```
1. Pull task directory from MinIO (Worker has pushed results)
1.5. VERIFY: check claimed deliverables exist and are non-empty
2. Update meta.json: status=completed (only if verify passed)
```

**2. `meta.json` schema extension** — add optional `verifiable_claims` field:
```json
{
  "task_id": "task-20260716-143022",
  "title": "Implement auth middleware",
  "status": "assigned",
  "type": "finite",
  "assigned_to": "engineer-backend",
  "room_id": "!xxx:matrix.pawcommit.com",
  "created_at": "...",
  "deliverable": "auth.py with timing-safe comparison",
  "acceptance_criteria": ["hmac.compare_digest used", "no plaintext token comparison"],
  "verifiable_claims": [
    {"path": "shared/tasks/task-20260716-143022/result.md", "check": "nonempty"},
    {"path": "shared/tasks/task-20260716-143022/auth.py", "check": "exists"},
    {"path": "shared/tasks/task-20260716-143022/BUILD_VERIFICATION.md", "check": "nonempty", "required": false}
  ]
}
```

**3. New script** — `manager/agent/skills/task-management/scripts/verify-output.sh`:
```bash
#!/usr/bin/env bash
# verify-output.sh --task-id T
# Reads meta.json verifiable_claims, checks each against MinIO/local task dir.
# Exit 0 = all required claims pass. Exit 1 = any required claim fails.
# Outputs JSON: {"verified": true/false, "claims": [{"path":..., "check":..., "passed":...}]}
```

This mirrors the `manage-state.sh` pattern — a bash script the Manager calls during heartbeat or
on receiving a completion message.

**4. `manage-state.sh`** — add `verify` action:
```bash
bash $STATE_SCRIPT --action verify --task-id T
# Returns: {"verified": true, "claims": [...]} or {"verified": false, "failed_claims": [...]}
```

**5. HEARTBEAT integration** — `manager/agent/copaw-manager-agent/HEARTBEAT.md`
Add a step: for each active task with status=completed (worker-reported, not yet manager-confirmed),
run verify-output. If pass → manager confirms completion (current flow). If fail → mark blocked with
reason "output verification failed: {claims}" and nudge the worker.

### What it buys you
- Catches the "PM says ready, nothing actually works" pattern at the task level, not just CI
- No controller code changes — pure Manager skill/script layer
- `verifiable_claims` are optional; existing tasks without them behave as today (backward compatible)
- The Manager can refuse completion and send the worker back with specific failures

### Rough effort
~150 lines of bash (verify-output.sh) + ~30 lines of jq in manage-state.sh + HEARTBEAT.md update +
finite-tasks.md doc update. No Go changes, no CRD changes.

---

## Feature B — Per-Worker LLM Usage Counter

### Problem
The plan explicitly says Higress exposes no per-consumer LLM-spend API. The dashboard can't show
which workers are burning tokens. A runaway worker (stuck in a tool-call loop) can silently consume
your entire API budget and you'd only notice when the provider cuts you off.

### Concept
The controller already calls `AuthorizeAIRoutes` for every worker on every reconcile cycle
(~5 min). Instead of just authorizing, also **count** the authorization events per consumer. This
isn't exact token cost — it's "worker X was authorized for N LLM calls this hour" — but it's enough
to spot a runaway worker or a dead route.

Actually, the better instrumentation point is the **gateway itself** — but we don't control Higress's
internals. The second-best point is the controller's reconcile path: count how many times each
consumer appears in `AuthorizeAIRoutes` calls. Since reconcile runs every ~5 min, this is a proxy for
"how many reconcile cycles touched this worker's AI route" — not per-request, but per-reconcile.

**Better approach:** Add a **request-counting middleware** to the controller's AI route proxy. The
controller doesn't proxy LLM traffic itself (Higress does), but it could expose a Prometheus counter
that workers increment via a lightweight `POST /api/v1/usage/llm` call before each LLM request.
This is opt-in per runtime and doesn't intercept traffic.

Actually, simplest viable approach: **Prometheus metrics already exist** (upstream commit `6b03ab0`
"expose AgentTeams controller metrics"). Check what's already instrumented and extend it.

### Insertion points

**1. Existing metrics** — `hiclaw-controller/internal/server/app.go:651`
The plan says: `BindAddress:"0"` → `":8091"` to enable the Prometheus endpoint.
Check `hiclaw-controller/internal/server/metrics.go` (if it exists) or wherever the Prometheus
registry is registered. Add per-consumer counters:

```go
// In metrics setup (wherever the Prometheus registry lives):
var LLMAuthorizationTotal = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Name: "agentteams_llm_authorizations_total",
        Help: "Number of AI route authorizations per consumer (proxy for LLM activity)",
    },
    []string{"consumer", "route"},
)
```

**2. Count in `modifyAIRoutes`** — `hiclaw-controller/internal/gateway/higress.go:189`
Inside `modifyAIRoutes`, when `add=true` and a route is successfully modified, increment the counter:
```go
// After successful route authorization:
LLMAuthorizationTotal.WithLabelValues(consumerName, routeName).Inc()
```

**3. Dashboard consumption** — `dashboard/server/src/server.js`
The dashboard proxy already polls the controller. Add a `/api/v1/metrics` proxy route that
forwards to the controller's `:8091/metrics` endpoint, then parse the `agentteams_llm_authorizations_total`
series in the dashboard's poll loop and display per-worker activity counts.

**4. WorkerResponse extension** — `hiclaw-controller/internal/server/types.go:61`
Add optional fields to what the dashboard already receives:
```go
type WorkerResponse struct {
    // ... existing fields ...
    LLMAuthorizationsLast24h int `json:"llmAuthorizationsLast24h,omitempty"`
}
```

**5. Populate in `workerToResponse`** — `hiclaw-controller/internal/server/resource_handler.go:1147`
Query the Prometheus registry for the worker's consumer counter and fill the field. Or simpler:
maintain an in-memory `map[consumer]map[route]int` in the LifecycleHandler (like `h.ready`),
reset on each reconcile cycle, and expose the running total.

### Simpler alternative (no Go changes)
Add a **heartbeat field** to the CoPaw worker's heartbeat report. The worker already calls
`hiclaw worker report-ready` / heartbeat. Add `llm_calls_since_last_heartbeat` to the heartbeat
payload — the worker knows how many LLM calls it made since last heartbeat (it's the one making them).
The controller stores it in `WorkerStatus` and the dashboard displays it.

**Insertion point:** `copaw/src/copaw_worker/bridge.py` — wherever the heartbeat is constructed,
add a counter for LLM API calls since last heartbeat. The worker increments it each time it calls
the Higress gateway URL.

### What it buys you
- Dashboard shows "worker X: 127 LLM calls in last hour" — enough to spot anomalies
- A worker stuck in a loop shows 1000+ calls while others show 10-20
- Doesn't need Higress internals — uses data the controller or worker already has
- Prometheus-compatible so you can alert on it later

### Rough effort
Go approach: ~50 lines (counter + increment + populate in response). 
CoPaw approach: ~30 lines of Python (counter in bridge.py) + 5 lines Go (WorkerStatus field) +
~20 lines dashboard JS (display).

---

## Feature C — Cross-Team Dependency Graph

### Problem
Projects are scoped to one team (decision #2). Cross-team requests are Manager-mediated. But there's
no visualization of "team A's project depends on team B's deliverable." The dashboard has a per-project
DAG view (`2ba63cd` "v2 task-detail drawer, status kanban, project plan/DAG view"), but it can't show
cross-project/cross-team dependencies.

### Concept
Add optional `dependsOn` to `ProjectSpec` referencing other Projects. The reconciler doesn't need to
enforce ordering (that's the Manager's job), but it records the edges. The dashboard's DAG view
extends to render cross-project edges.

### Insertion points

**1. `ProjectSpec`** — `hiclaw-controller/api/v1beta1/types.go:863`
```go
type ProjectSpec struct {
    Team        string        `json:"team"`
    Description string        `json:"description,omitempty"`
    ProjectName string        `json:"projectName,omitempty"`
    Repos       []ProjectRepo `json:"repos"`
    Workers     []string      `json:"workers,omitempty"`
    // NEW:
    DependsOn   []string      `json:"dependsOn,omitempty"` // project names this project waits on
}
```

**2. `ProjectStatus`** — `hiclaw-controller/api/v1beta1/types.go:895`
```go
type ProjectStatus struct {
    // ... existing fields ...
    // NEW:
    Dependencies []ProjectDependency `json:"dependencies,omitempty"`
}

type ProjectDependency struct {
    Project    string `json:"project"`     // the depended-on project name
    Phase      string `json:"phase"`       // current phase of the dependency (Ready/Completed/...)
    Satisfied  bool   `json:"satisfied"`   // true when dependency phase is Completed or Ready
}
```

**3. Reconciler** — `hiclaw-controller/internal/controller/project_controller.go:60`
In `Reconcile`, after the existing storage/manifest logic, resolve `spec.dependsOn`:
```go
// After existing reconcile logic:
if len(proj.Spec.DependsOn) > 0 {
    deps := make([]v1beta1.ProjectDependency, 0, len(proj.Spec.DependsOn))
    for _, depName := range proj.Spec.DependsOn {
        var depProj v1beta1.Project
        if err := r.k8s.Get(ctx, client.ObjectKey{Name: depName, Namespace: proj.Namespace}, &depProj); err != nil {
            deps = append(deps, v1beta1.ProjectDependency{Project: depName, Phase: "NotFound", Satisfied: false})
            continue
        }
        deps = append(deps, v1beta1.ProjectDependency{
            Project:   depName,
            Phase:     depProj.Status.Phase,
            Satisfied: depProj.Status.Phase == "Ready" || depProj.Status.Phase == "Completed",
        })
    }
    proj.Status.Dependencies = deps
}
```

**4. Dashboard DAG** — `dashboard/web/src/plan-parse.js` (or wherever the DAG renderer lives)
Extend the DAG to fetch all projects (not just the current one), build a combined graph with
cross-project edges from `status.dependencies`, and render with the dependency projects as
ghosted/linked nodes.

**5. Manager awareness** — `manager/agent/skills/project-management/SKILL.md`
Add a note: when a project has unsatisfied dependencies, the Manager should not assign tasks
for it yet — it should nudge the admin or the upstream team's lead. The HEARTBEAT can check
`project.Status.Dependencies` and surface blocked-by-dependency projects.

### What it buys you
- Visual "project A is waiting on project B" in the dashboard
- Manager can't accidentally assign work that's blocked by an upstream deliverable
- No enforcement (the reconciler doesn't block task assignment) — just visibility, which is enough
  for a solo operator

### Rough effort
~40 lines Go (types + reconciler) + CRD yaml update (`config/crd/projects.agentteams.io.yaml` +
Helm copy) + deepcopy regen + ~50 lines dashboard JS. Moderate — touches CRD schema so needs
the `make check-crd-sync` gate.

---

## Feature D — Full-Stack Worker Health Indicator

### Problem
The controller tracks container Phase/State (Pending/Running/Sleeping/Failed). But our hard-won
lesson from 2026-06-01: "Working in Element ≠ infrastructure configured." A worker can be Running
but have broken git creds, stale MinIO sync, or a dead LLM route. The dashboard shows "Running" ✓
but the worker is actually non-functional.

The current `WorkerResponse` (`types.go:61`) exposes: Phase, ContainerState, MatrixUserID, RoomID,
Message, ExposedPorts. No git/LLM/sync health.

### Concept
Add a `Health` summary to `WorkerResponse` with a 5-point check:
1. **Container** ✓ — already exists (Phase=Running)
2. **LLM** — can the worker reach its Higress route?
3. **Git** — does the worker have valid Gitea credentials?
4. **MinIO sync** — is the worker's file-sync healthy?
5. **Heartbeat** — has the worker reported recently? (`LastHeartbeat` already in `WorkerStatus:388`)

### Insertion points

**1. `WorkerStatus`** — `hiclaw-controller/api/v1beta1/types.go:376`
Add health fields:
```go
type WorkerStatus struct {
    // ... existing fields ...
    LastHeartbeat string `json:"lastHeartbeat,omitempty"`  // already exists (line 388)
    LastActiveAt  string `json:"lastActiveAt,omitempty"`   // already exists (line 389)
    // NEW:
    HealthChecks *WorkerHealthChecks `json:"healthChecks,omitempty"`
}

type WorkerHealthChecks struct {
    LLM      HealthCheck `json:"llm"`      // gateway route reachable
    Git      HealthCheck `json:"git"`      // gitea creds valid
    Sync     HealthCheck `json:"sync"`     // minio file-sync healthy
    Container HealthCheck `json:"container"` // container running
    Heartbeat HealthCheck `json:"heartbeat"` // recent heartbeat
}

type HealthCheck struct {
    Status   string `json:"status"`   // healthy | degraded | down | unknown
    Detail   string `json:"detail,omitempty"`
    CheckedAt string `json:"checkedAt,omitempty"`
}
```

**2. Populate in `GetWorkerRuntimeStatus`** — `hiclaw-controller/internal/server/lifecycle_handler.go:177`
After the existing backend status check, run health probes:

```go
// After existing backend.Status check (line ~195):
if result.Status == backend.StatusRunning {
    resp.HealthChecks = h.probeWorkerHealth(ctx, &worker, name)
}
```

**3. New `probeWorkerHealth` method** — same file:
```go
func (h *LifecycleHandler) probeWorkerHealth(ctx context.Context, worker *v1beta1.Worker, name string) *v1beta1.WorkerHealthChecks {
    checks := &v1beta1.WorkerHealthChecks{}
    
    // Container — already known from backend.Status
    checks.Container = v1beta1.HealthCheck{Status: "healthy", CheckedAt: time.Now().UTC().Format(time.RFC3339)}
    
    // LLM — probe the worker's Higress route
    // Resolve the worker's modelProvider → gateway URL, send a lightweight /v1/models GET
    checks.LLM = h.probeLLM(ctx, worker)
    
    // Git — check if the worker's gitea-mcp registration exists in Higress
    // (proxy: if the consumer is authorized on the mcp-gitea-<worker> route, git is wired)
    checks.Git = h.probeGit(ctx, worker)
    
    // Sync — check last MinIO sync timestamp from worker status or a sync health file
    checks.Sync = h.probeSync(ctx, worker)
    
    // Heartbeat — compare LastHeartbeat to now
    checks.Heartbeat = h.probeHeartbeat(worker)
    
    return checks
}
```

**4. Probes — implementation sketches:**

- **LLM probe**: `h.probeLLM` — resolve `worker.Spec.ModelProvider` via `GatewayClient.ResolveModelProvider`,
  then `GET <gatewayURL>/v1/models` with the worker's consumer key. 200 = healthy, 401/403 = degraded,
  timeout = down. The gateway URL + consumer key are already available in the reconcile path.

- **Git probe**: `h.probeGit` — check if `mcp-gitea-<worker>` exists in the Higress MCP server list
  and the worker's consumer is in `allowedConsumers`. `GatewayClient` already has MCP server methods.

- **Sync probe**: `h.probeSync` — check `worker.Status.LastActiveAt` or a `shared/health/sync.json`
  file the worker writes on each sync cycle. If `LastActiveAt` is >30min old → degraded, >2h → down.

- **Heartbeat probe**: `h.probeHeartbeat` — parse `worker.Status.LastHeartbeat`, compare to now.
  <10min = healthy, <30min = degraded, >30min = down.

**5. Dashboard display** — `dashboard/web/src/panels/` (worker card panel)
Replace the single "Phase: Running" badge with a 5-dot health strip:
```
[Container ✓] [LLM ✓] [Git ✓] [Sync ✓] [Heartbeat ✓]  →  Healthy
[Container ✓] [LLM ✗] [Git ✓] [Sync ✓] [Heartbeat ✓]  →  Degraded (LLM down)
```
Color-coded: green (healthy), yellow (degraded), red (down), gray (unknown).

**6. The team-leader readiness 403 bug** — `hiclaw-controller/internal/auth/authorizer.go:90`
`authorizeTeamLeaderWorkerAction` already lists `ActionReady` (line 98):
```go
case ActionWake, ActionSleep, ActionEnsureReady, ActionReady, ActionStatus:
    return a.requireSameTeam(caller, req)
```
So team leaders CAN call `ActionReady` on their team's workers. The `copaw/AGENTS.md:388` bug report
says `authorizeTeamLeaderWorkerAction` doesn't list `ActionReady` — but it does. **This bug may already
be fixed** by your Tier 0 remediation (`requireSameTeam` now fails closed, `4e40fac`). Check whether
the `AGENTS.md:388` note is stale before investigating further. If team leads still get 403, the issue
is likely in how the team leader's team identity is resolved (the `requireSameTeam` reverse-lookup path
from Tier 0 #1), not the action list.

### What it buys you
- Dashboard shows the real health of every worker, not just "container is running"
- Directly implements our 5-point check protocol from 2026-06-01 memory
- Catches the "appears working in Element but git/LLM is broken" pattern that cost us hours
- The probes are lightweight (1 HTTP call each, cached for the poll interval)

### Rough effort
~200 lines Go (types + probes + populate) + ~80 lines dashboard JS/CSS (health strip). 
Moderate-large — touches `WorkerStatus` (CRD schema) and adds runtime probes. The probes need
careful timeout handling to not slow down the status endpoint.

### Simpler v1 (no Go changes)
Just use the fields that already exist: `Phase`, `ContainerState`, `LastHeartbeat`, `LastActiveAt`.
The dashboard computes "stale heartbeat" client-side (LastHeartbeat > 30min ago → yellow) and
shows it alongside the container status. No probes, no new Go code. ~30 lines of dashboard JS.
Not as complete but catches the most common failure (heartbeat stopped) for zero backend cost.

---

## Implementation Priority (my opinion)

| Feature | Effort | Value | ROI |
|---|---|---|---|
| **A** (output verification) | Low (bash only) | High (catches the PM problem) | **Highest** |
| **D-simple** (heartbeat staleness) | Low (JS only) | Medium (catches dead workers) | High |
| **B-CoPaw** (heartbeat LLM count) | Low-Med | Medium (spot runaway workers) | Medium |
| **C** (dependency graph) | Medium (CRD change) | Medium (visibility) | Medium |
| **D-full** (5-point probes) | High (Go + probes) | High (full health) | Medium |
| **B-Go** (Prometheus counters) | Medium | Medium | Lower |

A first, D-simple second, then B or C depending on what hurts more.