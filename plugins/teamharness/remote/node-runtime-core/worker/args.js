#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");

function firstEnv(envNames) {
  for (const name of envNames || []) {
    const value = process.env[name];
    if (value !== undefined && value !== "") {
      return value;
    }
  }
  return "";
}

function isExecutable(file) {
  try {
    const stat = fs.statSync(file);
    if (!stat.isFile()) return false;
    if (process.platform === "win32") return true;
    return (stat.mode & 0o111) !== 0;
  } catch {
    return false;
  }
}

function commandInPath(command) {
  if (!command || command.includes(path.sep)) return "";
  for (const dir of String(process.env.PATH || "").split(path.delimiter)) {
    if (!dir) continue;
    const candidate = path.join(dir, command);
    if (isExecutable(candidate)) return candidate;
  }
  return "";
}

function resolveDefaultCommand(command, searchPaths) {
  if (!command || command.includes(path.sep) || commandInPath(command)) return command;
  for (const candidate of searchPaths || []) {
    if (candidate && isExecutable(candidate)) return candidate;
  }
  return command;
}

function normalizeModelConfigMode(value) {
  const normalized = String(value || "native-config").trim().toLowerCase();
  if (["managed-runtime", "managed-global", "native-config"].includes(normalized)) return normalized;
  throw new Error(`model config mode must be managed-runtime, managed-global, or native-config, got ${value}`);
}

function normalizeMaxConcurrentTasks(value, fallback) {
  const number = Number(value);
  const base = Number.isFinite(number) && number >= 1 ? Math.floor(number) : fallback;
  return Math.min(Math.max(Number(base) || 2, 1), 8);
}

function parseRuntimeArgs(argv, options) {
  const opts = options || {};
  const commandName = opts.commandName || "runtimeCommand";
  const commandArg = opts.commandArg || "--runtime-command";
  const pluginAliases = opts.pluginArgAliases || ["--plugin-dir"];
  const maxConcurrentEnv = process.env.TEAMHARNESS_MAX_CONCURRENT_TASKS;
  const args = {
    runtime: opts.runtime || "remote-node",
    bootstrapTokenFile: process.env.TEAMHARNESS_BOOTSTRAP_TOKEN_FILE || "",
    pluginInstallScope: "local",
    modelConfigMode: "native-config",
    stateDir: process.env.TEAMHARNESS_STATE_DIR || opts.defaultStateDir || ".agentteams/runtime/remote-node",
    pluginDir: opts.pluginEnvVar ? process.env[opts.pluginEnvVar] || "" : "",
    workDir: "",
    instanceId: "",
    intervalSeconds: Number(process.env.TEAMHARNESS_CONFIG_POLL_INTERVAL_SECONDS || "5"),
    runtimeRefreshIntervalSeconds: Number(process.env.TEAMHARNESS_RUNTIME_REFRESH_INTERVAL_SECONDS || "60"),
    heartbeatIntervalSeconds: Number(process.env.TEAMHARNESS_HEARTBEAT_INTERVAL_SECONDS || "30"),
    maxConcurrentTasks: maxConcurrentEnv !== undefined && maxConcurrentEnv !== "" ? normalizeMaxConcurrentTasks(maxConcurrentEnv, 2) : undefined,
    once: false
  };
  const envCommand = firstEnv(opts.commandEnvVars);
  let commandSource = envCommand ? "env" : "default";
  args[commandName] = envCommand || opts.commandDefault || commandName;

  for (let i = 0; i < argv.length; i += 1) {
    const key = argv[i];
    const value = argv[i + 1];
    switch (key) {
      case "--bootstrap-token-file":
        args.bootstrapTokenFile = value || "";
        i += 1;
        break;
      case "--plugin-install-scope":
        args.pluginInstallScope = value || "local";
        i += 1;
        break;
      case "--model-config-mode":
        args.modelConfigMode = normalizeModelConfigMode(value);
        i += 1;
        break;
      case "--state-dir":
        args.stateDir = value || args.stateDir;
        i += 1;
        break;
      case "--work-dir":
        args.workDir = value || "";
        i += 1;
        break;
      case "--instance-id":
        args.instanceId = value || "";
        i += 1;
        break;
      case "--interval":
        args.intervalSeconds = Number(value || "5");
        i += 1;
        break;
      case "--runtime-refresh-interval":
        args.runtimeRefreshIntervalSeconds = Number(value || "60");
        i += 1;
        break;
      case "--heartbeat-interval":
        args.heartbeatIntervalSeconds = Number(value || "30");
        i += 1;
        break;
      case "--max-concurrent-tasks":
        args.maxConcurrentTasks = normalizeMaxConcurrentTasks(value, args.maxConcurrentTasks);
        i += 1;
        break;
      case "--once":
        args.once = true;
        break;
      default:
        if (pluginAliases.includes(key)) {
          args.pluginDir = value || "";
          i += 1;
        } else if (key === commandArg) {
          if (value) {
            args[commandName] = value;
            commandSource = "arg";
          }
          i += 1;
        } else if (key.startsWith("--")) {
          throw new Error(`unknown argument: ${key}`);
        }
    }
  }
  if (commandSource === "default") {
    args[commandName] = resolveDefaultCommand(args[commandName], opts.commandSearchPaths);
  }
  return args;
}

module.exports = {
  isExecutable,
  normalizeModelConfigMode,
  normalizeMaxConcurrentTasks,
  parseRuntimeArgs
};
