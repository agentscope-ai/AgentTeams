#!/usr/bin/env node
"use strict";

const fs = require("fs");
const { spawnSync } = require("child_process");

const CONTEXT_TEAMHARNESS_KEYS = new Set([
  "TEAMHARNESS_TEAM_NAME",
  "TEAMHARNESS_MEMBER_NAME",
  "TEAMHARNESS_ROLE",
  "TEAMHARNESS_RUNTIME_CONFIG",
  "TEAMHARNESS_SHARED_DIR"
]);

const SHELL_ENV_TIMEOUT_MS = 3000;
const SHELL_ENV_MARKER = "__TEAMHARNESS_NATIVE_ENV_BEGIN__";

function cleanManagedEnv(source) {
  const env = {};
  for (const [key, value] of Object.entries(source || {})) {
    if (key.startsWith("HICLAW_")) continue;
    if (key.startsWith("AGENTTEAM_")) continue;
    if (key.startsWith("AGENTTEAMS_")) continue;
    if (CONTEXT_TEAMHARNESS_KEYS.has(key)) continue;
    env[key] = value;
  }
  return env;
}

function agentteamsEnv(options) {
  const opts = options || {};
  const edge = opts.edge || {};
  const runtimeState = opts.runtimeState || {};
  const member = runtimeState.runtime?.member || {};
  return {
    AGENTTEAMS_WORKER_NAME: String(edge.workerName || member.runtimeName || "").trim()
  };
}

function buildManagedRuntimeEnv(baseEnv, options) {
  const opts = options || {};
  const env = cleanManagedEnv(baseEnv);
  env.TEAMHARNESS_NODE_BIN = env.TEAMHARNESS_NODE_BIN || process.execPath;
  if (Object.prototype.hasOwnProperty.call(opts, "brokerDescriptor")) {
    env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR = String(opts.brokerDescriptor || "");
  }
  Object.assign(env, agentteamsEnv(opts));
  return env;
}

function sourceScriptForShell(shellPath) {
  const shell = String(shellPath || "").split(/[\\/]/).pop() || "";
  if (shell.includes("zsh")) {
    return "for f in \"$HOME/.zprofile\" \"$HOME/.zshrc\" \"$HOME/.profile\"; do [ -f \"$f\" ] && . \"$f\" >/dev/null 2>/dev/null || true; done";
  }
  if (shell.includes("bash")) {
    return "for f in \"$HOME/.bash_profile\" \"$HOME/.bashrc\" \"$HOME/.profile\"; do [ -f \"$f\" ] && . \"$f\" >/dev/null 2>/dev/null || true; done";
  }
  return "[ -f \"$HOME/.profile\" ] && . \"$HOME/.profile\" >/dev/null 2>/dev/null || true";
}

function resolveShell(env) {
  const explicit = String(env?.SHELL || "").trim();
  if (explicit && fs.existsSync(explicit)) return explicit;
  for (const candidate of ["/bin/bash", "/usr/bin/bash", "/bin/zsh", "/bin/sh"]) {
    if (fs.existsSync(candidate)) return candidate;
  }
  return "";
}

function readShellProfileEnv(baseEnv) {
  const env = { ...(baseEnv || {}) };
  const shell = resolveShell(env);
  if (!shell) return {};

  const script = [
    "set +e",
    sourceScriptForShell(shell),
    `printf '${SHELL_ENV_MARKER}\\0'`,
    "env -0"
  ].join("\n");

  const result = spawnSync(shell, ["-lc", script], {
    env,
    timeout: SHELL_ENV_TIMEOUT_MS,
    maxBuffer: 1024 * 1024
  });
  if (result.error || result.status !== 0 || !result.stdout) return {};

  const stdout = Buffer.isBuffer(result.stdout) ? result.stdout : Buffer.from(String(result.stdout));
  const marker = Buffer.from(`${SHELL_ENV_MARKER}\0`);
  const markerOffset = stdout.indexOf(marker);
  if (markerOffset < 0) return {};

  const payload = stdout.subarray(markerOffset + marker.length).toString("utf8");
  const out = {};
  for (const entry of payload.split("\0")) {
    if (!entry) continue;
    const index = entry.indexOf("=");
    if (index <= 0) continue;
    const key = entry.slice(0, index);
    if (!key) continue;
    out[key] = entry.slice(index + 1);
  }
  return out;
}

function buildNativeConfigRuntimeEnv(baseEnv, options) {
  const opts = options || {};
  const env = buildManagedRuntimeEnv(baseEnv, opts);
  const nodeBin = env.TEAMHARNESS_NODE_BIN || process.execPath;
  Object.assign(env, cleanManagedEnv(readShellProfileEnv(env)));
  env.TEAMHARNESS_NODE_BIN = nodeBin;
  if (Object.prototype.hasOwnProperty.call(opts, "brokerDescriptor")) {
    env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR = String(opts.brokerDescriptor || "");
  }
  Object.assign(env, agentteamsEnv(opts));
  return env;
}

module.exports = {
  agentteamsEnv,
  buildNativeConfigRuntimeEnv,
  buildManagedRuntimeEnv,
  cleanManagedEnv
};
