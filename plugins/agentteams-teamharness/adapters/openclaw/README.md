# TeamHarness OpenClaw Adapter

Projects the bundle contract (`../../contract.yaml`) onto the OpenClaw
runtime, statically. After `install.sh`, the TeamHarness MCP server is
declared in the OpenClaw runtime configuration and a **self-contained plugin
root is staged next to that configuration** — `<config dir>/plugins/
agentteams-teamharness/` — then registered via `plugins.load.paths`; no
runtime code executes at install time. With the default config that is
`~/.openclaw/plugins/agentteams-teamharness/`; with `--config <path>` it is
`<path's parent>/plugins/agentteams-teamharness/`. The bundle's adapter
directory itself is **not** registered: its `skills/` live outside it, and
OpenClaw rejects a manifest skill path that escapes the plugin root.

The staged root contains:

- `index.mjs` — the loadable plugin entry that native discovery loads
  (copied from the adapter directory).
- `openclaw.plugin.json` — plugin manifest (id, name, version, configSchema)
  with `skills` rewritten to **in-root** paths (`skills/<root>/<skill>`).
- `skills/` — the bundle's skills tree, copied in, so every manifest entry
  stays inside the plugin root.
- `install.sh` / `uninstall.sh` — black-box scripts that stage/remove that
  root and project the runtime configuration.

### Loadable entry (`index.mjs`)

At the pinned release, plugin discovery (`src/plugins/discovery.ts`) adds a
directory as a plugin candidate only when it carries a supported bundle
manifest (`.codex-plugin/`, `.cursor-plugin/`, `.claude-plugin/`, or
`plugin.json`) **or** a default entry file — `index.ts` / `index.js` /
`index.mjs` / `index.cjs` (`DEFAULT_PLUGIN_ENTRY_CANDIDATES`,
`src/plugins/manifest.ts`). A directory holding only `openclaw.plugin.json`
is skipped (`detectBundleManifestFormat` returns null for it), which is why
`index.mjs` exists: it makes this directory discoverable. The loader unwraps
the module's default export (`src/plugins/loader.ts`,
`unwrapDefaultModuleExport`), so the plugin is a plain default-exported
object. The plugin is declarative — it imports nothing from the `openclaw`
package and registers nothing at runtime; the TeamHarness functionality is
the `mcp.servers.teamharness` entry and the manifest skill roots, both
projected by `install.sh`.

## What `install.sh` writes

Target: `~/.openclaw/openclaw.json` (override with `--config <path>` or
`OPENCLAW_CONFIG`).

| Key | Value |
|-----|-------|
| `mcp.servers.teamharness` | `{command: python3, args: ["-m", "mcp.server"], cwd: <bundle root>, env: {TEAMHARNESS_RUNTIME_CONFIG: "${TEAMHARNESS_RUNTIME_CONFIG}"}}` |
| `plugins.allow` | `+ "agentteams-teamharness"` (deduplicated) |
| `plugins.load.paths` | `+ <config dir>/plugins/agentteams-teamharness` (the staged root; deduplicated, and the bare adapter directory is removed if an older install registered it) |
| `plugins.entries.agentteams-teamharness.enabled` | `true` |
| `openclaw.plugin.json` `skills` (staged copy) | the bundle's actual skill roots as **in-root** paths (`skills/<root>/<skill>`, each a directory containing `SKILL.md`); `[]` when the bundle ships no skills |

`install.sh` pre-checks that `index.mjs` exists in the adapter directory,
stages the root, and only then registers it. `--workspace <path>` is accepted
for isolated runs (the directory is created if absent); the registration is
absolute-path based and does not depend on the workspace.

The MCP entry is a *static projection*: `python3 -m mcp.server` with
`cwd` = bundle root. The `mcp/` directory now carries an explicit
`__init__.py` (a regular package, so `import mcp.server` can never resolve to
the pip-installed MCP SDK instead of this bundle). `TEAMHARNESS_RUNTIME_CONFIG` is written
as a `${VAR}` reference — the variable name, never a value. OpenClaw expands
`${VAR}` over the whole parsed configuration; an unset variable degrades to a
warning and keeps the placeholder, it does not crash the worker.

## Usage

```sh
install.sh                              # merge into ~/.openclaw/openclaw.json
install.sh --dry-run                    # print resulting config on stdout, change nothing
install.sh --config /path               # target a different config file
install.sh --config /path --workspace /tmp/ws   # isolated run (workspace created if absent)
uninstall.sh                            # remove exactly the keys above, keep everything else
uninstall.sh --dry-run
```

Both scripts are idempotent: canonical serialization (sorted keys, 2-space
indent) makes two consecutive runs byte-identical, and files are only
rewritten when the bytes would change. A freshly created config file is
written `0600`; an existing file's mode is preserved. No credentials are ever
written. Dry-run previews redact existing credential values (string values
under keys ending in `token`, `secret`, `password`, `passwd`, `apikey`, or
`credentials`) as `<redacted>`; the written file itself is never redacted.

## Upstream key provenance (higress-group/openclaw @ 2f35b6fa, pinned in `openclaw-base/Dockerfile`)

- `mcp.servers.<name>` — `src/config/types.mcp.ts` (`McpServerConfig`:
  `command`, `args`, `env`, `cwd`, `workingDirectory`, `url`, `transport`,
  `headers`, ...). This is the upstream-equivalent of the task's
  `mcpServers` shape; there is **no top-level `mcpServers` key** in
  openclaw.json.
- `~/.openclaw/openclaw.json` — `docs/gateway/configuration-reference.md`.
- `${VAR}` substitution — `src/config/env-substitution.ts` (recursive over
  the parsed config, uppercase names, `$${` escape; missing variable warns
  and keeps the placeholder).
- Manifest format — `docs/plugins/manifest.md`: `openclaw.plugin.json`
  requires `id` + `configSchema`; `skills: string[]` are "skill directories
  to load, relative to the plugin root". Only documented manifest fields are
  read; **there is no `mcpServers` manifest key** — MCP servers are runtime
  configuration, declared in openclaw.json instead.
- Plugin registration keys (`plugins.allow` / `plugins.load.paths` /
  `plugins.entries`) — same shape the repo already uses for the
  openclaw-cms-plugin (`manager/scripts/init/start-manager-agent.sh`,
  `manager/agent/skills/worker-management/scripts/generate-worker-config.sh`).

## Deviations & limitations (disclosed)

1. **No `mcpServers` in the manifest.** The task's wording suggested an
   `mcpServers` key on `openclaw.plugin.json`; upstream ignores undocumented
   manifest keys ("Avoid adding custom top-level keys here" —
   `docs/plugins/manifest.md`). The MCP server is therefore declared where
   OpenClaw actually reads MCP declarations: `mcp.servers` in openclaw.json.
2. **Plugin entry, not a bundle manifest.** This directory ships
   `index.mjs` (a default-exported plugin object) rather than a
   `.codex-plugin/`/`.cursor-plugin/`/`.claude-plugin/` bundle manifest.
   That is the loadable route at the pinned commit: discovery
   (`src/plugins/discovery.ts`) requires a supported bundle manifest or a
   default entry file (`DEFAULT_PLUGIN_ENTRY_CANDIDATES`), and
   `openclaw.plugin.json` alone makes a directory a non-candidate
   (`detectBundleManifestFormat` returns null for it). The loader unwraps
   the default export (`src/plugins/loader.ts`,
   `unwrapDefaultModuleExport`); both the directory scan and
   `plugins.load.paths` resolve this same directory, so both register the
   same plugin id.
3. **The plugin root is the staged directory, not the adapter directory.**
   OpenClaw rejects a manifest skill path that escapes the plugin root, so
   `install.sh` copies the skills into `<config dir>/plugins/
   agentteams-teamharness/skills/` and rewrites the manifest to in-root
   paths.
4. **`commands.mcp` is not set** — it only enables the chat `/mcp` command;
   server availability does not depend on it.

## Native discovery acceptance (pinned runtime)

Reproduce the maintainer's P1 check on the pinned release
(2026.4.14 / `2f35b6fa`), offline:

```sh
export HOME=<scratch-home>          # empty directory; no network
bash install.sh --config $HOME/.openclaw/openclaw.json --workspace <tmp>/ws
openclaw plugins list --json        # agentteams-teamharness present, no
                                    # "plugin not found" warnings
bash uninstall.sh --config $HOME/.openclaw/openclaw.json
openclaw plugins list --json        # id gone, no warnings return
```

The regression drives these steps against the real CLI when `OPENCLAW_BIN`
is available (or `openclaw` is on PATH) and is wired into CI
(`.github/workflows/test-controller.yml`, `openclaw-native-discovery` job,
`npm install -g openclaw@2026.4.14`); it skips with a clear reason
otherwise.

## Lifecycle

- **Install** — run `install.sh`; safe to re-run (byte-identical).
- **Upgrade** — update the bundle, re-run `install.sh`; the MCP entry and
  manifest skills are re-projected, plugin registration is a no-op.
- **Uninstall** — `uninstall.sh` removes this adapter's configuration keys
  (MCP entry, `plugins.allow` / `plugins.load.paths` / `plugins.entries`) and
  deletes the staged plugin root (`<config dir>/plugins/
  agentteams-teamharness/`); other entries in openclaw.json are preserved,
  and the bundle source files are never touched.
- **Reload** — the OpenClaw worker picks up the configuration on its next
  start.
