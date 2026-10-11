# Coding-CLI Delegation — Operational Guidance

> Companion guidance for the `coding-cli-management` and `coding-cli` skills.
> Distilled from field operation of delegated coding-CLI sessions (field notes
> published on agentscope-ai/AgentTeams#1340). Content-only — no behavior change,
> no new surface.
>
> **Part A** — first increment: one-shot delegation (supported by the current skills).
> **Part B** — later capabilities (roadmap, not yet implemented; evaluated separately).

## Part A — First increment: one-shot delegation (currently supported)

The delegating agent invokes the coding CLI as an execution tool, reusing the existing identity, permissions, task flow, storage, and audit.

### Task-spec discipline

A delegated coding task should be a bounded, self-describing unit. The prompt handed to the CLI should carry:

- **Scope** — one coherent change; prefer a small set of related files over one-line fragments or open-ended epics.
- **Deliverables** — exact target files and expected outputs.
- **Acceptance criteria** — how correctness will be judged, stated so a third party could verify it.
- **Verification steps** — commands the runner should execute itself (build / test / lint), with output included in the report.
- **Stop conditions** — when to stop; include the escape hatch: "if blocked or a step cannot run, skip it and say so in the report."
- **Artifact location** — results and logs land in the shared task directory (`shared/tasks/<task-id>/`); session replies are not always re-readable, so "show me" must mean "write it to a file."
- **Declared intent for long waits** — runner-side policies can gate individual commands (a bare standalone wait was blocked pending declared intent); annotate the intent of long waits inline so they clear the gate.
- **Brief for a cold start** — the receiver starts with zero context: task and why it exists; relevant files by path (don't paste them); current state; what was tried and abandoned; decisions with rationale; acceptance criteria; constraints (must-not / must-preserve). Carry the exact task semantics — investigate-only means "do not edit files", a fix means "implement the fix", a refactor means "refactor, not rewrite".
- **Isolate per task** — give delegated work its own branch or worktree (use the runner's native branch/worktree support when it has some; otherwise create the branch before starting the session); parallel writers never share a checkout.

**Scaffolding scales with the runner.** For small/edge models, fully pre-write the change (near-ready spec, exact anchors). For stronger models, explicit goals + constraints + acceptance + self-verification are enough; over-constraining a strong model can reduce quality.

### Auth, opt-in and execution boundary

Delegation reuses the delegating agent's identity and credentials; it adds no new approval subsystem.

**Detection surface — existence only, never read values.**

- `qwen` — `~/.qwen` or env-based auth.
- `opencode` — `~/.config/opencode`, `~/.local/share/opencode/auth.json`, or env-based auth.
- Detection checks that a configured surface exists; it does not read or log its contents.

**Unattended semantics.** `qwen --yolo` auto-approves every tool call; `opencode --auto` still enforces explicit deny rules. A changed working directory or an added timeout is not a permission boundary — they alter behavior, not scope.

**Execution boundary.** The CLI runs as the delegating agent and inherits its filesystem and environment-credential visibility. Evaluate per deployment: workspace scope, reach into other team files, the delegator's credentials, mounted sockets, and management-plane capabilities. Mitigations are the CLIs' native controls — qwen `--approval-mode` tiers and `--include-directories` (declare any directory beyond the task workspace explicitly, e.g. shared toolchains or reference code, rather than relying on ambient filesystem reach), opencode deny rules — and/or an isolated execution environment.

**Verification.** Run the verify suite before relying on the delegation and record the tested version in the report — there is **no fixed version pin**: the deployment picks its own npm channel tag (stable by default; `0.25.0` is the most recent field-verified reference), and any version or channel change re-runs the suite:

1. Headless run succeeds and the artifact lands in the task directory.
2. Auth failure propagates as a non-zero exit code.
3. Timeout terminates the run.
4. The workspace boundary holds — no writes outside the task directory.

- **Progress channel.** Reuse, don't add: the console stream as primary progress surface, with file artifacts (results and logs in the task directory) as the durable record; supervision, when it exists, drives off status probes. An optional structured block is a welcome enhancement, never a dependency.
- **Concurrency and cost.** Per-runner session limits and model-endpoint auth are explicit configuration, not implicit behavior.

### Preflight and environment checks

Before the first real task:

- **Runner present where the session runs** — install or mount accordingly; verify with a trivial round-trip ("ping") before real work.
- **Auth configured end-to-end** — settings file or environment, including base URL and model; secrets never in desired state (`docs/design/member-runtime-config-contract.md`).
- **Environment hygiene** — if ambient variables break the runner, pin the invocation in a small wrapper and register that as the runner command.
- **Network matrix** — on some links, connections are reset selectively by TLS stack generation; use a current stack or a local relay, and document the finding for the deployment.
- **Placement.** Choose per side: a host-side daemon on the user's machine is the least-effort option (independent lifecycle, natural file locality, survives agent rebuilds); on a managed worker the in-container options are the host-independent ones — a subprocess for one-shot delegated runs, a daemon variant once parallelism matters; use the platform's dedicated node when it offers one. Keep files on mounts/shared volumes, and prefer the shape whose toolchain survives a re-install. Daemon and cross-boundary placement shapes belong to Part B.
- **Direction rule for user-local runners.** When the runner lives on the user's machine and the orchestrator in a managed deployment, assume inbound is unavailable (NAT/firewall): the local side must initiate — poll a queue, hold a connection, or run a local relay — and anything it sends out must leave through a surface the managed side accepts (a member-voice relay) until the runner itself has an identity. Membership fixes the voice, not reachability.
- **Runner egress, not just the orchestration plane.** The chosen placement must satisfy the runner's own model egress: in field testing one runtime reached the provider endpoint directly while the other needed a local relay (TLS stack fingerprinting on the path). Verify egress per placement, not only the orchestration plane.

### Scenario adaptation (toolchain, model, knowledge, artifacts)

A one-shot run is only as good as what the delegating agent has prepared in the workspace and on the host. For real development scenarios — mobile, embedded, web, device toolchains — prepare four layers before dispatching, then name them all in the task spec:

1. **Toolchain placement.** Build toolchains (SDKs, compilers, platform CLTs) live on a shared volume (e.g. a team NFS `toolchains/<platform>/` directory) and are referenced by **absolute path** in the spec — never assumed to be on the runner's default PATH. Pin the exact version in the spec; the runner install is generic, the toolchain is the scenario's.
Some toolchains are **host-OS/architecture-specific**, so "reachable by absolute path" is not
enough: iOS builds need a macOS host (a Linux agent container cannot run Xcode), and cross-compiles
need the target toolchain (e.g. `arm-none-eabi-gcc`) installed on the runner host. Verify the runner's
host can actually *host* the toolchain before dispatching, not just that the files are reachable.
2. **Model defaults.** The platform supplies the model default (provider settings rendered once at bootstrap); the task spec may override per run (`run-coding-cli.sh --model <name>`). Two tiers: platform default carries cost/policy, the per-run flag carries the job.
3. **Knowledge and file sharing.** Project knowledge (offline doc corpora, API docs, team conventions, prior specs) must be *reachable from the workspace* — mounted into the workspace or named by path in the spec. A runner cannot read what it cannot path-reach: "the team knows X" is not context, a path is. Share artifacts the same way: a stable location both sides can reach (team storage / MinIO), not a chat attachment.
4. **Artifact flow.** Name where deliverables land (build output dir → team storage key) and how completion is judged (build passes / tests pass), not "done".

**Default toolchain reality (field-verified on the standard worker base image).** The four layers assume the delegator names what the runner needs; the matrix below is what the standard base image already ships, so a delegator knows up front which scenarios close inside the worker and which must route build verification to a host or build node — before dispatching, not when the runner hits the wall:

| Scenario | Writes in worker | Build/verify in worker | Route |
|---|---|---|---|
| Web (Node / React / Vue) | yes | yes (Node 24 + npm 11 / pnpm) | closed in the worker |
| Python, stdlib-only | yes | yes (bare `python3` 3.12 — no pip, no third-party packages) | closed in the worker |
| Python, third-party (requests / pandas / numpy …) | yes | no — no pip in the base image | code in the worker; verify on a host or build node that has the packages |
| C / C++ | yes | yes (`gcc` / `g++` / `make`) | closed in the worker |
| Go / Rust | yes | no — no `go` / `cargo` in the base image | code in the worker; build/verify on a host or build node |
| Java / Android | yes | no — no `java` / `gradle` / Android SDK | code in the worker; build on a host with the SDK |
| iOS | yes | no — requires a macOS host with Xcode, which a Linux worker cannot provide | code in the worker; build on a macOS host or CI |
| HarmonyOS / Flutter / other device SDKs | yes | no — no platform CLT or SDK in the base image | code in the worker; build on a host with the platform toolchain |
| Infra-as-code (Dockerfiles / CI / k8s / Terraform) | yes | no — the worker is itself a container and ships no `docker` / `kubectl` / `helm` | write the files in the worker; apply and verify on the host |
| ML / data (numpy / pandas / torch) | yes | no — no data-science packages and no pip | code in the worker; run on a host or build node with the environment |
| Docs / comments / text config | yes | yes (no build step) | closed in the worker |

Two task-shape notes on top of the language axis:
- **Greenfield** (scaffolding a new project) is more constrained than modifying one: the base image can scaffold Node, stdlib-Python and C projects, but has no `go mod init` / `cargo new` / `gradle init`, so greenfield for those stacks ends at the source files.
- **Debug and refactor** inherit the build/verify column above — they need the failing test or the regression suite to run; if the worker cannot run the tests, the worker diagnoses from the code and the test run happens on the host.

When a task genuinely needs a toolchain the matrix marks missing, layer 1 applies: provision it on the shared volume and name it by absolute path in the spec — do not assume the base image carries it, and do not make the runner install it mid-task.

The same four layers apply to every scenario class (Android, iOS, web, HarmonyOS-style device SDKs, embedded/MCU) with its own toolchain, knowledge corpus, and artifact specifics.

### Pitfalls → what to do (field-verified)

| Symptom | What to do |
|---|---|
| Runner binary missing at spawn | install/mount where sessions run; ping before real work |
| Auth errors in sequence (authenticate → missing API key) | configure the full auth surface (base URL + model + key) first |
| Ambient environment breaks the runner | wrap the invocation; pin flags and settings |
| Connections reset on some links | use a modern TLS stack or a local relay |
| VCS operations denied inside sandboxes | keep git at the orchestrator; no git steps in delegation specs |
| Parallel sessions clobber files | one writer per file; isolate workspaces |
| A bare wait/sleep is blocked by policy | annotate intent on long waits; expect command-level gates even after the task is accepted |
| Cold-start handoff repeats old mistakes | carry "what was tried and abandoned" + decisions in the briefing |

## Part B — Later capabilities (roadmap; not yet implemented)

> Later capabilities — evaluated separately when a concrete delegation requirement needs them; ACP sessions are covered here, not in the first increment.

### API-level supervision contract (host-daemon form)

These apply to the session/daemon form (Part B shapes), not to the one-shot path.

- **Supervision probe** — the session status endpoint exposes an "active prompt" flag.
  A lightweight poller is sufficient; no bespoke completion endpoint needed.
- **First-responder voting** — the permission-vote endpoint answers with the selected option id or a cancel.
  A 404 means the vote was already taken — first-responder semantics at the transport level, composing with claim-before-act.
- **Session durability** — the transcript persists on disk and can be re-loaded after process death (suspend/crash/OOM).
  A turn in flight during the crash is lost — re-issue the task into the same session: idempotency is the contract, not recovery of the interrupted turn.
- **Polling-client protection** — the server's idle auto-close can be deferred by a grace setting.
  A poll-based (non-SSE-attached) supervisor must be protected from its session being reaped mid-work.
- **Model-endpoint reachability** — the runner's model base URL accepts any OpenAI-compatible endpoint, including a transparent local proxy.
  The standard lever to keep the model plane working behind restrictive/DPI links without touching session config.

### Supervision and steering

A delegated session is not fire-and-forget. The supervising side should:

- **Define signals** — completion / failure / blocked / pending-approval — and derive them from structured events or status probes, never from parsing prose.
- **Watch without babysitting** — consume the session's event stream and/or a light status probe; push a notification to the orchestrator on completion or failure instead of having someone watch a terminal. This mirrors the taskflow attention model (`docs/design/task-completion-notification.md`): signals first, sync-then-notify.
- **Steer losslessly** — mid-turn instructions queue; issuing a cancel stops the current turn and the queued instruction resumes. Use this to redirect long runs; do not assume mid-turn messages interrupt.
- **Keep state outside the session** — sessions expire between uses; durable state belongs in the workspace (task records, result files), not in session memory.
- **Resolve pending approvals on cancel** — a client that cancels a turn must resolve any pending permission request as cancelled (ACP semantics).
- **End the turn; let wakes drive** — no held session, no polling: completion, timeout and scheduled wakes each resume the orchestrator as a new turn, and the exchange is auditable in the session record. Don't poll, hurry-up, or interrupt — long reasoning runs can take 15–30 minutes; trust the finish notification.
- **Claim before acting** — several watchers may observe the same completion; take an occupancy token first (idempotent handling).
- **Size watch windows for worst case** — saturated local runners can queue sessions for ~10 minutes; an undersized window expires before completion (we missed one). Arm watchers before work starts; treat a "no activity observed" alert as a first-class signal; give long runs a deadline with a fallback chain: completion → timeout → scheduled self-wake → human.
- **Scheduled runs isolate or share** — default isolated runs (own per-job session; silent — wakes nobody; right for periodic inspection) vs shared runs (delivered into the target session = a real self-wake; required for fallback wake-ups).
- **Analysis helpers are read-only** — when you summon a second opinion or a two-model committee for root cause, end every prompt with the no-edits suffix ("This is analysis only. Do NOT edit, create, or delete any files. Do NOT write code."); an advisor gives a judgment — it does not drive the work.
- **Delivery ≠ acceptance ≠ execution — three gates.** A ledger entry is not a delivery: field reports document assignments recorded with no notification event ever emitted, so the worker was never reached and the result was written by the assigner. Require a delivery event for every assignment, a receipt (or worker-side activity evidence) for completion rather than the assigner's word — and keep refusals and "no activity observed" as first-class signals.
- **Route wakes explicitly.** With more than one orchestrator session live, a wake sent to the default target resumes the wrong session — a "vanished" wake is often a misroute, not a loss. Record the intended target at job creation; keep the owning session derivable from the wake payload, never guessed.
- **Treat wake jobs as at-least-once.** One-shot jobs have been seen lingering past their fire time and duplicated with identical names. Keep handlers idempotent (see "Claim before acting"), have one-shots auto-expire, clean fired records, and make lost deliveries recoverable — a "mark-after-confirm" shape (the wake is marked delivered only after confirmation) where the platform supports it.
- **Reset lifecycle state on wake.** Where the platform has an idle-sleep policy, a worker woken after a long idle can be re-slept by the very next idle scan because its idle marker is stale — a wake-to-re-sleep loop. Every wake and ensure-ready path must reset the idle clock: the wake and the lifecycle share the same moment. The platform's own wake/ensure-ready paths now reset the marker upstream (#1352, closing the framework half of the #1239 DEF-001 report); keep this as a contract for custom wake paths and verify the deployed version contains the fix.

### Wake delivery and relay

The supervisor's completion signal must reach the orchestrating agent as a wake. Field-tested properties of the delivery path:

- **Busy semantics are per-surface — probe, don't assume.** A task/submission API typically *queues* when busy, while a direct injection surface may *refuse* (HTTP 409). Latency when idle is seconds; when busy it is "next available turn". For refusing surfaces, the notification layer must retry with backoff or route through a queue-capable path.
- **The wake must come from a member's voice.** Runners have no room identity, and a message does not wake its own author (own-skip) — so when a relay is needed, the @mention leg must be spoken by a member. Observed patterns, in increasing cost:
  - **A (recommended).** Inject the notification into the delegating agent's own inbound surface using the room's session context; the agent wakes in that context and its reply posts back to the room as the agent itself — where the @mention wakes the next actor. The runner stays anonymous.
  - **B.** Post the room message directly as the agent (via its credentials) — enough when only *others* need waking.
  - **C.** Route through a coordinator/manager identity that @mentions the agent.
  - **D.** A dedicated bridge account that joins rooms and @mentions on behalf of runners (scale-up).
- **Cross-boundary reachability.** Same container/host: loopback, no extra auth. Host → agent container: the published port is the path (bridge IPs may not be routable), and the injection path must carry the **target agent scope** — without it the request silently lands on the default agent. Cross-node: routing + auth; otherwise degrade to the member-voice relay above.
- **Identity upgrade path (orthogonal axis).** Pure delegation (no member identity) and declared-member relaying are zero-cost today. The platform's current main also provisions an edge-worker form — full member treatment minus the container: real Matrix identity, personal/team rooms, allowlists, workspace, and a projected runtime file carrying the runner's own credentials — with **zero upstream changes**; the managed-runtime direction remains deferred (per #1357). Evaluate a rung when a concrete requirement appears, not before.

### Governed runners and injected requests

A runner operating under workspace governance will refuse injected work — treat refusal as a feature, not a fault:

- **Why it refuses.** An instruction appended after a memory/context block reads as smuggled content, not a user message; external side effects additionally require explicit authorization. In field testing a governed runner refused twice on exactly these grounds, then — once the request was re-sent as a standalone, authorized, verifiable message — verified the script, asked one clarifying question, and executed.
- **Design implications.** Wake/notification messages must be *standalone and attributed* (who sent it, under what authority, exactly what to do) and *verifiable* (let the agent inspect what it is asked to do). Platform-level provenance marking materially helps agents distinguish notifications from smuggled content.
- **Refusal is a signal.** Surface it back — it tells you what authorization or context is missing. Never blind-retry.
- **Budget for due diligence.** A governed runner spent ~16 minutes (queue + verification) before executing; size timeouts and messaging for that.

### Approval expectations

- **The routine is automated; the risky pauses.** Keep an allow-pattern (in-workspace edits, read-only inspection, build/test) and a hold-list that is never auto-answered: destructive operations, credential paths, secret reads, service managers, production-bound targets.
- **Strict option echoing** — always answer a permission request with one of the option ids carried by that request.
- **Never auto-select a mode-switching option** (e.g. "allow once and switch to default") — humans only.
- **Audit every decision** — who / when / what / why per answer; this is what lets a reviewer reconstruct an unattended run (the durable audit store behind `GET /api/v1/audit` is a natural home).
- **Unanswered requests stall sessions indefinitely.** Plan the human surface (`approval_level` / attention events) so long runs do not depend on someone being online.
- **The room leg already exists upstream.** The runner's channel base ships a productized permission relay: per-channel dispatch, a pending table, a rendered approval (allow-once / always-allow / deny, with a request-id suffix under multi-task), and the human reply parsed back into the strict option-id vote the flow requires (single-flight; a lost race surfaces as a 404 → cancel); unroutable or failed delivery auto-cancels — no orphaned pending state. The room leg of the approval bridge is therefore inherited by any channel; the open question is which surface renders the approval card and how it is audited — a routing/UX decision, not a protocol one.
- **Ride the attention model, don't add a surface.** Notifications should use the taskflow attention model (`request_attention`, kind=approval — sync-before-notify, idempotent per kind, mentions the leader and the human initiator, auto-resolved on result acceptance). The payload and routing conventions are now defined in the same design doc — an option payload (`options: [{id, label}]` with optional `suggested` / `expires_at`, strict option-id echoing, mode-switching options human-only, expiry resolving as denied-with-reason) and routing with `console-first` as the default and `room` as opt-in (exactly one route). The remaining gap is implementation: emitting and transporting those payloads on the attention-event path.

### Pitfalls → what to do (field-verified)

| Symptom | What to do |
|---|---|
| "Done" appears instantly | prompts ack asynchronously — derive completion from status/events; detect send failures explicitly |
| First status read is "not active" → false done | require "seen active at least once"; grace-timer never-started runs |
| Sessions expire between uses | keep durable state in files/artifacts |
| Permission-answer route differs between builds | pin the canonical form for the deployed version; treat stale answers as benign |
| An option also flips the global mode | never auto-select it |
| Unattended approval stalls a session | policy-answer with holds; escalate only the risky slice |
| Reviewers stop reading approvals | automate the routine; keep the audit trail as the safety net |
| Mid-turn steering doesn't interrupt | queue + cancel → resume |
| Completion missed — watch window shorter than queue delay | size windows for worst-case queue + execution (~10 min observed); arm watchers before work starts |
| "No activity observed" alert | treat as a first-class signal — it may be the only closure trigger |
| Watchers race on the same completion | claim an occupancy token before acting (idempotent wake handling) |
| Analysis helper starts editing files | end every analysis prompt with the no-edits suffix |
| Wake routed to a default target | record the intended session at job creation; keep the owner derivable from the payload |
| Duplicate or lingering wake jobs | at-least-once discipline: idempotent claim; auto-expire one-shots; clean fired records; re-deliverable lost wakes |
| Assignment recorded but never delivered | require a delivery event per assignment; receipts (or worker-side activity) for completion; refusals are signals |
| A woken worker re-slept immediately | reset the idle marker on wake / ensure-ready paths |
| Run longer than the watch window | hour-plus runs are normal; size windows for the long tail (~10 min queue observed + long execution) |
| Cross-boundary supervisor cannot vote | a local-only permission mode gates votes by loopback origin — a host-side/remote supervisor gets nothing under it; use first-responder voting (or that mode's intended local UI) for delegated sessions that cross a boundary |
| Injection lands on the wrong agent | the injection path must carry the target agent scope — without it the request silently lands on the default agent |
| Busy injection rejected (HTTP 409) | busy semantics are per-surface (some queue, some refuse); probe each surface; back off or use the queue-capable path for refusing ones |
| Deliverable lost on runner restart | judge closure by commit existence, not file presence; keep commits on the orchestration side where the sandbox blocks VCS |
| Model calls fail in one placement only | egress is placement-dependent (TLS fingerprinting); verify the runner's own reachability per placement, not only the orchestration plane |

## Related surfaces

- Adapter precedent and the natural home for runner installation / hook work: `plugins/teamharness/adapters/claude-code/`.
- Team-level task contract: `plugins/teamharness/skills/team/task-delegation/` and `task-execution/`.
- Completion / attention events: `docs/design/task-completion-notification.md`.
- Secrets rule: `docs/design/member-runtime-config-contract.md`.
- Remote-member role: `plugins/teamharness/prompts/agent/remote-member.md`.
