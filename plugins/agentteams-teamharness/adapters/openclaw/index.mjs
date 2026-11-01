// TeamHarness runtime plugin entry for OpenClaw (native discovery).
//
// At the pinned OpenClaw (2026.4.14 / 2f35b6fa), src/plugins/discovery.ts
// adds a directory as a plugin candidate only when it carries a supported
// bundle manifest (.codex-plugin/.cursor-plugin/.claude-plugin/plugin.json)
// or a default entry file — index.ts / index.js / index.mjs / index.cjs
// (DEFAULT_PLUGIN_ENTRY_CANDIDATES, src/plugins/manifest.ts). A directory
// with only openclaw.plugin.json is skipped (detectBundleManifestFormat
// returns null for it), which is why this file exists: it is the loadable
// entry that makes this adapter directory discoverable.
//
// The loader unwraps the module's default export (src/plugins/loader.ts,
// unwrapDefaultModuleExport), so the plugin object is a plain default
// export. It deliberately imports nothing from the openclaw package: the
// plugin directory is loaded from an absolute path registered in
// plugins.load.paths, where openclaw's package resolution is not available,
// and the plugin is declarative anyway.
//
// Nothing is registered at runtime: the TeamHarness functionality is the
// MCP server entry (mcp.servers.teamharness in openclaw.json) and the
// manifest skill roots, both projected by install.sh.

export default {
  id: "agentteams-teamharness",
  name: "TeamHarness",
  version: "0.1.0",
  description:
    "AgentTeams TeamHarness runtime plugin for OpenClaw (declarative: MCP server + skills).",
  register() {
    // No runtime registrations: mcp.servers.teamharness and the manifest
    // skills are plain runtime configuration written by install.sh.
  },
};
