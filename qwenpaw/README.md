# AgentTeams QwenPaw Worker Runtime

This package contains the QwenPaw worker runtime baseline for AgentTeams.

The package owns the local runtime lifecycle:

- prepare the QwenPaw working directory and default workspace
- restore and persist eligible worker files through object storage
- apply `runtime.yaml` model, MCP, Matrix, DingTalk, and AgentSpec package changes
- maintain local QwenPaw health state and controller readiness/heartbeat reports
- overlay QwenPaw Matrix behavior for AgentTeams startup readiness, first-sync stability, invite auto-join, and mention compatibility

The package intentionally does not wire the runtime into controller/Helm
selection, build the worker image, package TeamHarness/WorkerFlow adapter
artifacts, or implement remote-worker credential/JWT flows. Those integration
surfaces are separate PR scopes.

## Development

Run the focused unit suite with:

```bash
PYTHONPATH=qwenpaw/src python -m pytest qwenpaw/tests -q
```
