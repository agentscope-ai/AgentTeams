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
`execution.mode: sandbox`. Revocation removes resources in the following
order: Service, token Secret, Pod (and waits for its deletion), NetworkPolicy,
then the `ExecutionSandbox` object.

A DeepAgents Worker whose container is running but has not completed its first
valid Matrix sync is reported as `Starting` (`Ready=False`), not failed. Only
an explicit terminal container failure state is terminal; readiness follows the
persisted Matrix sync token.

For local verification, use the Runner hardening tests; their token value is a
non-secret sentinel and the assertions do not print token material:

```bash
uv run --locked --extra dev pytest -q tests/test_runner_process_hardening.py
```

On a disposable local cluster, a permission check can confirm that the Runner
process environment is not readable without reading or printing it:

```bash
kubectl -n "${AGENTTEAMS_NAMESPACE}" exec deployable-runner-pod -- \
  sh -ceu 'test ! -r /proc/1/environ; ! env | grep -Eq "^AGENTTEAMS_RUNNER_TOKEN="'
```

## Development

The repository vendors DeepAgents 0.7.3 under `../deepagents`. Install both
projects into the same Python 3.11+ environment before running integration
tests. The dependency-free contract tests can be run with:

```bash
PYTHONPATH=src python -m unittest discover -s tests -v
```
