#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");
const { writeJson, writeStatus } = require("./status");
const { ossPut, ossDelete, ossList } = require("./storage-oss");

function packageViewBasePrefixes(runtime) {
  const storage = runtime.storage && typeof runtime.storage === "object" ? runtime.storage : {};
  const member = runtime.member && typeof runtime.member === "object" ? runtime.member : {};
  let memberPrefix = String(storage.memberPrefix || "").trim().replace(/^\/+|\/+$/g, "");
  if (!memberPrefix) {
    const runtimeName = String(member.runtimeName || member.name || "").trim();
    if (runtimeName) {
      memberPrefix = `agents/${runtimeName}`;
    }
  }
  if (!memberPrefix) return [];

  const prefixes = [memberPrefix];
  const memberRuntime = String(member.runtime || "").trim().toLowerCase();
  if (["qwenpaw", "remote-managed-local"].includes(memberRuntime) && !memberPrefix.includes("/.qwenpaw/workspaces/default")) {
    prefixes.push(`${memberPrefix}/.qwenpaw/workspaces/default`);
  }
  return Array.from(new Set(prefixes));
}

async function syncPackageFileToView(sts, source, objectKey) {
  if (fs.existsSync(source) && fs.statSync(source).isFile()) {
    await ossPut(sts, objectKey, fs.readFileSync(source));
    return true;
  }
  await ossDelete(sts, objectKey).catch(() => {});
  return false;
}

async function syncPackageSkillsToView(sts, packageDir, skillsPrefix) {
  const existing = await ossList(sts, skillsPrefix).catch(() => []);
  await Promise.all(existing.map(key => ossDelete(sts, key).catch(() => {})));

  const skillsDir = path.join(packageDir, "skills");
  if (!fs.existsSync(skillsDir) || !fs.statSync(skillsDir).isDirectory()) {
    return 0;
  }
  const writes = [];
  const walk = dir => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        walk(full);
      } else if (entry.isFile()) {
        const rel = path.relative(skillsDir, full).split(path.sep).join("/");
        writes.push(ossPut(sts, `${skillsPrefix}${rel}`, fs.readFileSync(full)));
      }
    }
  };
  walk(skillsDir);
  await Promise.all(writes);
  return writes.length;
}

async function syncAgentPackageView(args, runtimeState, packageState, packageDir, sts) {
  const runtime = runtimeState.runtime || {};
  const prefixes = packageViewBasePrefixes(runtime);
  if (!prefixes.length) {
    throw new Error("runtime storage.memberPrefix is required to sync AgentSpec package view");
  }

  const configDir = path.join(packageDir, "config");
  let totalSkillFiles = 0;
  const writtenContext = [];
  for (const prefix of prefixes) {
    for (const name of ["SOUL.md", "AGENTS.md"]) {
      if (await syncPackageFileToView(sts, path.join(configDir, name), `${prefix}/${name}`)) {
        writtenContext.push(`${prefix}/${name}`);
      }
    }
    totalSkillFiles += await syncPackageSkillsToView(sts, packageDir, `${prefix}/skills/`);
  }

  const marker = {
    status: "synced",
    ref: packageState.ref || "",
    digest: packageState.digest || "",
    objectKey: packageState.objectKey || "",
    prefixes,
    contextFiles: writtenContext,
    skillFiles: totalSkillFiles,
    syncedAt: Math.floor(Date.now() / 1000)
  };
  for (const prefix of prefixes) {
    await ossPut(sts, `${prefix}/.agent-package.json`, `${JSON.stringify(marker, null, 2)}\n`);
  }
  return marker;
}

async function syncAgentPackageViewIfNeeded(args, runtimeState, state, packageDir, sts) {
  if (state.viewSync?.status === "synced" && state.viewSync.digest === state.digest) {
    return state;
  }
  try {
    state.viewSync = await syncAgentPackageView(args, runtimeState, state, packageDir, sts);
    writeStatus(args, "Running", "AgentPackageViewSynced", "AgentSpec package view synced");
  } catch (error) {
    state.viewSync = {
      status: "failed",
      digest: state.digest || "",
      message: String(error.message || error).slice(0, 500),
      failedAt: Math.floor(Date.now() / 1000)
    };
  }
  writeJson(path.join(args.stateDir, "agent-package-state.json"), state);
  runtimeState.agentPackage = state;
  return state;
}

module.exports = {
  packageViewBasePrefixes,
  syncPackageFileToView,
  syncPackageSkillsToView,
  syncAgentPackageView,
  syncAgentPackageViewIfNeeded
};
