# DeepAgents AgentTeams Runtime

This package integrates the LangChain DeepAgents runtime with AgentTeams. It
contains the Worker process, Matrix/Higress/MinIO/PostgreSQL adapters, and the
credential-free execution runner used by `ExecutionSandbox` resources.

The package is intentionally kept outside the vendored `deepagents/` subtree.
AgentTeams-specific behavior must be implemented here using public DeepAgents
extension points unless an upstream limitation is documented in
`UPSTREAM_PATCHES.md`.

## Development

The repository vendors DeepAgents 0.7.3 under `../deepagents`. Install both
projects into the same Python 3.11+ environment before running integration
tests. The dependency-free contract tests can be run with:

```bash
PYTHONPATH=src python -m unittest discover -s tests -v
```
