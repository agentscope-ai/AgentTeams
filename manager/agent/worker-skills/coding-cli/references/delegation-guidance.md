# Coding-CLI Delegation — Delegator Reference

Self-contained reference for the delegating side (Worker, team Leader, or an authorized Worker) of a one-shot coding-CLI delegation.

## What a one-shot delegation is

The delegating agent hands a bounded task to a coding CLI; the run executes against the shared task directory; the result and its logs land there; the delegating agent reviews the result before reporting completion. One delegation is one bounded run — there is no session to return to.

## What a one-shot delegation does not provide

- No long-lived or resumable session — each run starts from the files on disk.
- No mid-run supervision or steering — a running delegation cannot be redirected.
- No approval bridge — the run is unattended; risky operations are not paused for a human mid-run.

These are later capabilities, evaluated separately (see the management-side guidance). Do not assume them; if a task needs them, it is not a one-shot task.

## Task-spec discipline

The prompt you hand over is the whole contract. It should carry:

- **Scope** — one coherent change; exact target files, not an open-ended epic.
- **Deliverables** — what must exist or change when the run finishes.
- **Acceptance criteria** — how correctness is judged, stated so a third party can verify it.
- **Verification steps** — commands the runner executes itself, with output in the report.
- **Stop conditions** — when to stop; include the escape hatch: if a step cannot run, skip it and say so in the report.
- **Artifact location** — results and logs go to the shared task directory, not to the chat reply; "show me" must mean "write it to a file".
- **Brief for a cold start** — the receiver starts with zero context: what and why, relevant files by path, current state, what was tried and abandoned, decisions with rationale, constraints (must-not / must-preserve); carry the exact task semantics — investigate-only means "do not edit files".

**Scaffolding scales with the runner.** For small/edge models, pre-write the change nearly ready (near-ready spec, exact anchors); for stronger models, explicit goals plus constraints and acceptance are enough.

## Scenario reality: which scenarios close in the worker

The base image ships Node 24 + npm/pnpm, a C/C++ toolchain, and a bare Python 3.12 (**no pip, no third-party packages**), so web, C/C++, stdlib-only Python, and docs/comment changes close inside the worker (write + build + verify). It does **not** ship Go, Rust, Java/Android, device-SDK, or data-science toolchains, and — because the worker is itself a container — no `docker`/`kubectl`/`helm` for infra work: for those, the worker writes the code or files and build/verification happens on a host or build node; iOS additionally needs a macOS host (a Linux worker cannot build it). When a task needs a toolchain the base image lacks, provision it on a shared volume and name it by absolute path in the spec — do not assume it is on the runner's PATH. Full matrix (including the Python stdlib/third-party split and infra-as-code) and the four-layer scenario preparation (toolchain / model / knowledge / artifacts): see the management-side guidance.

## Where results and logs land

- Task directory: `shared/tasks/<task-id>/` (workspace, artifacts, reports).
- CLI output: `shared/tasks/<task-id>/workspace/coding-cli-logs/`.
- Saved prompt: `shared/tasks/<task-id>/coding-prompts/`.
- Session replies are not always re-readable — sync the task directory from storage before reviewing; the files on disk are the record.
