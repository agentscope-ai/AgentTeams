# DeepAgents AgentTeams Runtime

This package integrates the LangChain DeepAgents runtime with AgentTeams. It
contains the Worker process, Matrix/Higress/MinIO/PostgreSQL adapters, and the
credential-free execution runner used by `ExecutionSandbox` resources.

The package is intentionally kept outside the vendored `deepagents/` subtree.
AgentTeams-specific behavior must be implemented here using public DeepAgents
extension points unless an upstream limitation is documented in
`UPSTREAM_PATCHES.md`.

## Runner security and lifecycle boundary

The execution Runner is Linux-only. It consumes its bearer token before it
opens an HTTP listener, sets `RLIMIT_CORE` to zero, and uses
`prctl(PR_SET_DUMPABLE, 0)`. A missing token, unsupported platform, core-limit
failure, or non-dumpable failure stops startup before the application or
listener is created.

Approved commands run with a minimal, non-secret environment, closed inherited
file descriptors, and a new process session. The Runner token is removed from
the Runner environment and Linux's non-dumpable setting prevents approved
commands from inspecting the Runner process environment through `/proc`.
These are capability-reduction measures: `/bin/sh` is **not** a general
side-effect-free sandbox, and intentionally killing container PID 1 remains a
denial-of-service risk that this release does not solve.

An `ExecutionSandbox` lease is revoked when its Worker is deleted, changes
owner UID, changes runtime or DeepAgents configuration, or leaves
`execution.mode: sandbox`, and when the lease reaches its idle or maximum
lifetime. A cleanup finalizer is durable before any child is created. Delete
requests use the lease UID and resourceVersion; the finalizer then removes the
exact owned Service and token Secret, requests deletion of the exact owned Pod,
and retains the NetworkPolicy and lease until an uncached API-server read proves
that Pod generation is absent. Child deletes also require matching
controller/worker/sandbox labels, the lease owner UID, and UID/resourceVersion
preconditions. Only then is the NetworkPolicy deleted and the finalizer removed
with the current resourceVersion.

Worker events are mapped through an index on
`ExecutionSandbox.spec.workerRef.name`, scoped by namespace and controller
identity rather than the driftable Worker label. A failed cache lookup is
logged and retried through the uncached API reader; normal lease deadlines
remain the bounded safety revalidation.

A DeepAgents Worker whose container is running but has not completed its first
valid Matrix sync is reported as `Starting` (`Ready=False`), not failed. Only
an explicit terminal container failure state is terminal; readiness follows the
persisted Matrix sync token.

For local verification, use the Runner hardening tests; their token value is a
non-secret sentinel and the assertions do not print token material:

```bash
uv run --locked --extra dev pytest -q tests/test_runner_process_hardening.py
```

On a disposable local cluster, set the three identity variables to the current
controller, Worker, and `ExecutionSandbox` names. The following probe selects
exactly one Runner from all four identity labels, first checks that exec works,
then silently attempts to read one byte from `/proc/1/environ`. It never prints
that file or an environment block:

```bash
mapfile -t RUNNER_PODS < <(
  kubectl -n "${AGENTTEAMS_NAMESPACE}" get pod \
    -l "agentteams.io/controller=${AGENTTEAMS_CONTROLLER_NAME},agentteams.io/worker=${DEEPAGENTS_WORKER_NAME},agentteams.io/execution-sandbox=${EXECUTION_SANDBOX_NAME},agentteams.io/runtime=deepagents-runner" \
    -o name
)
if [ "${#RUNNER_PODS[@]}" -ne 1 ]; then
  printf 'expected exactly one Runner Pod, got %d\n' "${#RUNNER_PODS[@]}" >&2
  exit 1
fi
RUNNER_POD="${RUNNER_PODS[0]#pod/}"
kubectl -n "${AGENTTEAMS_NAMESPACE}" exec "${RUNNER_POD}" -- sh -c 'exit 0' >/dev/null
if kubectl -n "${AGENTTEAMS_NAMESPACE}" exec "${RUNNER_POD}" -- \
  sh -c 'dd if=/proc/1/environ of=/dev/null bs=1 count=1' >/dev/null 2>&1; then
  printf 'READABLE\n'
  exit 1
fi
printf 'BLOCKED\n'
```

Do not infer an approved command's environment by enumerating the environment
of a separate container-runtime exec process. Use the real-process pytest above
(or a Human-approved normal Runner command that returns only a boolean/status)
to verify the command environment without exposing values.

## Development

The repository vendors DeepAgents 0.7.3 under `../deepagents`. Install both
projects into the same Python 3.11+ environment before running integration
tests. The dependency-free contract tests can be run with:

```bash
PYTHONPATH=src python -m unittest discover -s tests -v
```
