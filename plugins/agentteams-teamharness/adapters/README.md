# Runtime adapters

This bundle carries one runtime adapter per supported AgentTeams runtime.
The bundle contract (`../contract.yaml`) is the runtime-neutral source of
truth; each adapter only projects it onto its runtime.

| Runtime | Status |
|---------|--------|
| qwenpaw | functional |
| openclaw | functional (MCP entry + staged plugin root) |
| hermes | functional (MCP entry + plugin dir) |

## Package lifecycle contract

`../scripts/install.sh` and `../scripts/uninstall.sh` are the documented
package entrypoints: they detect the supported local runtimes and dispatch
to the matching adapter.

Install detection (default `--runtime auto`): every detected runtime is
installed, so coexisting runtimes are all covered.

| Runtime | Detected when |
|---------|---------------|
| qwenpaw | `qwenpaw` is on PATH |
| openclaw | `openclaw` is on PATH, or `$OPENCLAW_CONFIG` is set, or `~/.openclaw/openclaw.json` exists |
| hermes | `hermes` is on PATH, or `$HERMES_HOME` is set, or `~/.hermes/config.yaml` exists |

Uninstall detection is artifact-driven, so residue is removed even after
the runtime binary is gone:

| Runtime | Uninstalled when |
|---------|------------------|
| qwenpaw | `qwenpaw` is on PATH |
| openclaw | the config file (`$OPENCLAW_CONFIG` or `~/.openclaw/openclaw.json`) or the staged plugin root `<config dir>/plugins/agentteams-teamharness` exists |
| hermes | `$HERMES_HOME` is set, or `~/.hermes/config.yaml` or `~/.hermes/plugins/agentteams-teamharness` exists |

- `--runtime <name>` (repeatable) selects a runtime explicitly and bypasses
  detection for it. The adapters are idempotent, so selecting a runtime with
  nothing installed is a successful no-op.
- `--dry-run` is forwarded to every adapter and writes nothing.
- The entrypoints never override `$OPENCLAW_CONFIG`, `$OPENCLAW_WORKSPACE`,
  `$HERMES_HOME`, or `$HERMES_PLUGIN_HOME`; the adapters read them.
- Failures are collected per runtime and reported as
  `<runtime>: installed|failed (exit N)|skipped (not detected)`; install
  exits 1 when no runtime was selected or a selected adapter failed,
  uninstall exits 1 when a selected adapter failed (nothing detected is a
  clean no-op).
- With `TEAMHARNESS_INSTALL_LOG` set, each run that changed at least one
  runtime appends one JSON line:
  `{"event":"install","runtimes":[...],"pluginDir":"..."}` (or
  `"event":"uninstall"`).
- **Interpreter contract**: the entrypoints and every adapter script run on
  the system shell — **bash 3.2 or later, macOS system bash included**. They
  deliberately avoid bash-4-only constructs (associative arrays,
  `mapfile`/`readarray`, case-modifying expansions such as `${var^^}`,
  `[[ -v ]]`, `wait -n`, `&>>`, `|&`, `coproc`, nameref declarations); the
  per-runtime status state in the entrypoints is plain variables plus
  `set_status`/`get_status` helpers.
  `plugins/tests/teamharness/test_shell_portability.py` pins the contract:
  a static denylist scan over every lifecycle script, plus a real-interpreter
  dry-run of the OpenClaw install/uninstall when a bash 3.2 is resolvable
  (`TEAMHARNESS_BASH32` path, or `bash3.2` / `bash-3.2` on PATH); the full
  lifecycle suite is parametrized over the same interpreter. On macOS the
  system `/bin/bash` is 3.2.57, so macOS CI exercises this automatically.

The experimental deepseek-harness adapter remains in the `plugins/teamharness`
compatibility path and is not part of this portable bundle.
