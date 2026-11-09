# TeamHarness Hermes Adapter

Projects the bundle contract (`../../contract.yaml`) onto the Hermes runtime
(hermes-agent, pinned `v2026.4.16` in `hermes/Dockerfile`), statically. After
`install.sh`, the TeamHarness MCP server is declared in the Hermes
configuration and the plugin (skills included) sits in the upstream
third-party plugin location; no runtime code executes at install time.

## What `install.sh` writes

Target: `~/.hermes/config.yaml` (or `$HERMES_HOME/config.yaml`; override with
`--config <path>`).

| Key | Value |
|-----|-------|
| `mcp_servers.teamharness` | `{command: python3, args: [<bundle root>/mcp/server.py], env: {<allowlisted NAME>: "${NAME}", …}}` — the narrow env allowlist below, projected by reference |
| `~/.hermes/plugins/agentteams-teamharness/` | `__init__.py` (the directory-plugin entry the loader requires) + `plugin.yaml` + `register.py` + the bundle's `skills/` tree, synced byte-for-byte |

### MCP child env allowlist

The supported hermes SDK builds the MCP child environment in two stages
(`tools/mcp_tool.py`): at config load, `_load_mcp_config()` makes
`~/.hermes/.env` available and resolves each `${NAME}` in the server
config from the environment (an unresolvable reference stays the literal
placeholder); at spawn, `_build_safe_env(user_env)` gives the child a
small safe baseline (PATH, HOME, TMPDIR, LANG, TERM, LC_ALL, SHELL, USER
plus `XDG_*`) from the parent process and the entry's `env` map —
**nothing else crosses the boundary** (no role overrides, no secrets, no
whole-env inheritance). So the entry must project the narrow allowlist of
`${NAME}` references the server needs (the variable name, never a value):

| Group | Variables |
|-------|-----------|
| state/runtime | `TEAMHARNESS_RUNTIME_CONFIG` (kept for compat), `TEAMHARNESS_SHARED_DIR`, `AGENTTEAMS_SHARED_DIR`, `AGENTTEAMS_STORAGE_PREFIX`, `AGENTTEAMS_SHARED_STORAGE_PREFIX`, `TEAMHARNESS_MATRIX_CONTEXT_FILE` |
| Matrix | `AGENTTEAMS_MATRIX_URL`, `AGENTTEAMS_WORKER_MATRIX_TOKEN`, `AGENTTEAMS_WORKER_NAME`, `AGENTTEAMS_MATRIX_USER_ID`, `AGENTTEAMS_MATRIX_DOMAIN` |
| storage credentials | `AGENTTEAMS_FS_ENDPOINT`, `AGENTTEAMS_FS_ACCESS_KEY`, `AGENTTEAMS_FS_SECRET_KEY` |

**Resolution rule** — a variable is written only when something can
resolve it: it is set (non-empty) in the installer's environment, **or**
its key appears in `~/.hermes/.env` (keys are parsed; values are never
read). Otherwise the child would receive a literal `${NAME}` it could
never resolve.

**Explicitly excluded** — `AGENTTEAMS_AGENT_ROLE` and
`AGENTTEAMS_WORKER_ROLE` (Worker role overrides stay blocked: role is
operator-set only, never MCP-child-set), and everything not in the
allowlist — the adapter never projects `PATH`, `HOME`, or arbitrary
variables into the entry (no whole-env inheritance); the safe baseline
above is the SDK's own, not something the adapter writes.

`register(ctx)` (called by the hermes plugin loader) registers the bundle's
skills via `ctx.register_skill(name, path)` and does **not** register the MCP
server — hermes-agent has no MCP registration API on `PluginContext`; MCP
servers are read from `mcp_servers` in config.yaml
(`tools/mcp_tool.py::_load_mcp_config`).

Every allowlisted variable is written as a `${NAME}` reference — the
variable name, never a value. hermes resolves each `${NAME}` from the
environment (including `~/.hermes/.env`, loaded at startup) and keeps the
placeholder verbatim when unresolvable; the resolution rule above is what
keeps the placeholder resolvable in the first place.

## Usage

```sh
install.sh                    # merge into ~/.hermes/config.yaml + sync plugin dir
install.sh --dry-run          # print resulting config.yaml on stdout, change nothing
install.sh --config /path --plugin-home /other/path
uninstall.sh                  # remove the mcp_servers key + the plugin dir
uninstall.sh --dry-run
```

Both scripts are idempotent: the YAML is dumped canonically (sorted keys) and
files are only rewritten when the bytes would change, so two consecutive runs
are byte-identical. A freshly created config file is written `0600`; an
existing file's mode is preserved. No credentials are ever written. Dry-run
previews redact existing credential values (string values under keys ending in
`token`, `secret`, `password`, `passwd`, `apikey`, or `credentials`) as
`<redacted>`; the written file itself is never redacted. Uninstall removes
the plugin directory only when its `plugin.yaml` identifies as
`name: agentteams-teamharness`; anything else is left alone.

Requires `python3` with PyYAML — hermes-worker itself declares
`pyyaml>=6.0`, so any environment that runs the Hermes runtime has it.

## Upstream key provenance (NousResearch/hermes-agent @ v2026.4.16)

- `mcp_servers.<name>` — `tools/mcp_tool.py` (`_load_mcp_config` reads
  `config.get("mcp_servers")`; stdio entries carry `command`/`args`/`env`,
  HTTP entries `url`/`headers`; **no `cwd` field**).
- `~/.hermes/config.yaml` as the user config — repo `AGENTS.md`; `${ENV_VAR}`
  interpolation and unset-keeps-placeholder semantics —
  `website/docs/user-guide/configuration.md`.
- Plugin discovery & `register(ctx)` — `hermes_cli/plugins.py` (directory
  scan of `~/.hermes/plugins/<dir>/` + pip entry point
  `hermes_agent.plugins`); plugin fields `name/version/description/author/
  requires_env/provides_tools/provides_hooks`; a `register` exception
  disables the plugin, the runtime continues.
- Plugin-bundled skills — `website/docs/guides/build-a-hermes-plugin.md`
  (`plugins/<name>/skills/<skill>/SKILL.md` + `ctx.register_skill`).

## Deviations & limitations (disclosed)

1. **No `cwd` in the MCP entry.** hermes's MCP entry shape has no `cwd`
   field, so the adapter uses the absolute path
   `<bundle root>/mcp/server.py` with `python3`. This works because
   `server.py` imports its sibling modules (`message_tool`,
   `roomflow_tool`, ...) by name — Python puts the script's own directory on
   `sys.path` regardless of the process cwd. (The OpenClaw adapter can use
   `python3 -m mcp.server` + `cwd` because `McpServerConfig` has a `cwd`
   field.)
2. **`kind: mcp` in plugin.yaml is declarative.** The upstream plugin loader
   has no `kind` field; it parses `plugin.yaml` leniently (`data.get` with
   defaults) and ignores unknown keys, including our CRD header
   (`apiVersion`/`metadata`). The field documents intent for bundle tooling.
3. **No `requires_env`** — declaring `TEAMHARNESS_RUNTIME_CONFIG` there would
   make hermes skip the whole plugin (skills included) whenever the variable
   is absent; the MCP entry degrades gracefully on its own.
4. **config.yaml is re-dumped on install** — existing content is preserved
   key-for-key, but YAML comments in a hand-edited config are not preserved
   by the canonical dump. In deployed workers this file is machine-managed.
5. **Skill sync is additive** — `install.sh` copies changed/missing skill
   files but never deletes; a skill removed from the bundle lingers in the
   installed plugin dir until the dir is re-created (uninstall + install).

## Lifecycle

- **Install** — run `install.sh`; safe to re-run (byte-identical).
- **Upgrade** — update the bundle, re-run `install.sh`; the MCP entry is
  re-projected and changed skill files are re-synced.
- **Uninstall** — `uninstall.sh` removes the `mcp_servers.teamharness` key
  and the plugin directory (with the identity guard); other config keys are
  preserved.
- **Reload** — the Hermes worker picks up the configuration on its next
  start.
