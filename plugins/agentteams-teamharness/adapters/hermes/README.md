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
| `mcp_servers.teamharness` | `{command: python3, args: [<bundle root>/mcp/server.py], env: {TEAMHARNESS_RUNTIME_CONFIG: "${TEAMHARNESS_RUNTIME_CONFIG}"}}` |
| `~/.hermes/plugins/agentteams-teamharness/` | `__init__.py` (the directory-plugin entry the loader requires) + `plugin.yaml` + `register.py` + the bundle's `skills/` tree, synced byte-for-byte |

`register(ctx)` (called by the hermes plugin loader) registers the bundle's
skills via `ctx.register_skill(name, path)` and does **not** register the MCP
server — hermes-agent has no MCP registration API on `PluginContext`; MCP
servers are read from `mcp_servers` in config.yaml
(`tools/mcp_tool.py::_load_mcp_config`).

`TEAMHARNESS_RUNTIME_CONFIG` is written as a `${VAR}` reference — the
variable name, never a value. hermes resolves `${ENV_VAR}` from `os.environ`
(including `~/.hermes/.env`) and keeps the placeholder verbatim when unset.

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
