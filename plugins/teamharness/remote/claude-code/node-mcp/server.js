#!/usr/bin/env node
"use strict";

const path = require("path");

function loadCoreServer() {
  const explicit = process.env.TEAMHARNESS_NODE_RUNTIME_CORE_DIR;
  const candidates = [
    explicit ? path.join(explicit, "mcp", "server.js") : "",
    path.join(__dirname, "../../node-runtime-core/mcp/server.js"),
    path.join(__dirname, "../../../node-runtime-core/mcp/server.js")
  ].filter(Boolean);
  for (const candidate of candidates) {
    try {
      return require(candidate);
    } catch (error) {
      if (error.code !== "MODULE_NOT_FOUND" || !String(error.message || "").includes(candidate)) {
        throw error;
      }
    }
  }
  throw new Error(`TeamHarness node-runtime-core MCP server not found from ${__dirname}`);
}

loadCoreServer().runMcpServer({
  runtime: "claude-code",
  serverName: "teamharness-claude-code",
  healthDescription: "Check TeamHarness MCP server availability and remote-managed Claude Code wiring."
});
