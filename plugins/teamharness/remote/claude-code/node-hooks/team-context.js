#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");

function findDescriptor() {
  const explicit = (process.env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR || "").trim();
  if (explicit && fs.existsSync(explicit)) {
    return explicit;
  }
  let current = __dirname;
  for (;;) {
    const candidate = path.join(current, ".teamharness", "credential-broker.json");
    if (fs.existsSync(candidate)) {
      return candidate;
    }
    const parent = path.dirname(current);
    if (parent === current) {
      return "";
    }
    current = parent;
  }
}

async function brokerContext() {
  const descriptorPath = findDescriptor();
  if (!descriptorPath) {
    return {};
  }
  try {
    const descriptor = JSON.parse(fs.readFileSync(descriptorPath, "utf8"));
    const endpoint = String(descriptor.endpoint || "").replace(/\/+$/, "");
    const token = fs.readFileSync(String(descriptor.tokenFile || ""), "utf8").trim();
    if (!endpoint || !token) {
      return {};
    }
    const response = await fetch(`${endpoint}/v1/runtime/context`, {
      headers: { Authorization: `Bearer ${token}`, Accept: "application/json" }
    });
    if (!response.ok) {
      return {};
    }
    return await response.json();
  } catch {
    return {};
  }
}

function runtimeContextFiles() {
  const pluginRoot = path.resolve(__dirname, "..");
  const contextRoot = path.join(pluginRoot, ".teamharness", "runtime-context");
  const sections = [];
  for (const [file, title] of [["AGENTS.md", "Agent instructions"], ["SOUL.md", "Soul"]]) {
    const target = path.join(contextRoot, file);
    if (!fs.existsSync(target)) {
      continue;
    }
    const text = fs.readFileSync(target, "utf8").trim();
    if (text) {
      sections.push(`${title}:\n${text}`);
    }
  }
  return sections;
}

async function main() {
  const context = await brokerContext();
  const storage = context.storage && typeof context.storage === "object" ? context.storage : {};
  const skillRegistry = context.skillRegistry && typeof context.skillRegistry === "object" ? context.skillRegistry : {};
  const lines = [
    "TeamHarness local Claude runtime is active.",
    "Use TeamHarness MCP for explicit shared file and task operations.",
    "Do not treat low-information acknowledgements as task completion."
  ];
  for (const [label, value] of [
    ["Team", context.teamName || process.env.AGENTTEAMS_TEAM_NAME],
    ["Member", context.memberName || process.env.AGENTTEAMS_WORKER_NAME],
    ["Runtime", context.runtimeName],
    ["Role", context.role || process.env.AGENTTEAMS_ROLE || "remote-member"],
    ["Team room", context.teamRoomId],
    ["Personal room", context.personalRoomId],
    ["Shared prefix", storage.sharedPrefix],
    ["Member prefix", storage.memberPrefix],
    ["Global shared prefix", storage.globalSharedPrefix],
    ["Skill registry", skillRegistry.url]
  ]) {
    if (value) {
      lines.push(`${label}: ${value}`);
    }
  }
  lines.push(...runtimeContextFiles());
  process.stdout.write(JSON.stringify({
    hookSpecificOutput: {
      hookEventName: "SessionStart",
      additionalContext: lines.join("\n")
    }
  }));
}

main().catch(() => {});
