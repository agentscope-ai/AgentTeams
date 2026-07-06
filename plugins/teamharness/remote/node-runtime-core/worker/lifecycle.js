#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");
const { spawnSync } = require("child_process");
const { decodeBootstrapToken, readBootstrapTokenFile } = require("./bootstrap");
const { exchangeEdgeToken, requestSts, reportHeartbeat } = require("./controller");
const { loadRuntimeConfig } = require("./runtime-config");
const { logEvent } = require("./log");
const { cleanupLegacySensitiveState, mkdirp, writeStatus } = require("./status");

function commandExists(command) {
  if (!command) return false;
  if (command.includes(path.sep)) {
    return fs.existsSync(command);
  }
  const result = spawnSync(process.platform === "win32" ? "where" : "command", process.platform === "win32" ? [command] : ["-v", command], {
    shell: process.platform !== "win32",
    stdio: "ignore"
  });
  return result.status === 0;
}

function wait(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

function closeServer(server) {
  return new Promise(resolve => {
    if (!server || typeof server.close !== "function") {
      resolve();
      return;
    }
    const timer = setTimeout(resolve, 1000);
    try {
      server.close(() => {
        clearTimeout(timer);
        resolve();
      });
    } catch {
      clearTimeout(timer);
      resolve();
    }
  });
}

function installSignalCleanup(cleanup) {
  let running = false;
  const handler = () => {
    if (running) return;
    running = true;
    Promise.resolve()
      .then(cleanup)
      .finally(() => process.exit(0));
  };
  process.once("SIGINT", handler);
  process.once("SIGTERM", handler);
  return () => {
    process.removeListener("SIGINT", handler);
    process.removeListener("SIGTERM", handler);
  };
}

function workerOnce(args) {
  return Boolean(args.once || process.env.TEAMHARNESS_WORKER_ONCE === "1");
}

async function runWorkerMain(argv, adapter) {
  const args = adapter.parseArgs(argv);
  mkdirp(args.stateDir);
  cleanupLegacySensitiveState(args);
  logEvent("info", "worker_starting", {
    runtime: args.runtime,
    stateDir: args.stateDir,
    workDir: args.workDir,
    modelConfigMode: args.modelConfigMode,
    maxConcurrentTasks: args.maxConcurrentTasks
  });

  let bootstrap;
  try {
    bootstrap = decodeBootstrapToken(readBootstrapTokenFile(args));
  } catch (error) {
    writeStatus(args, "Failed", error.reason || "InvalidBootstrapToken", error.message);
    logEvent("error", "bootstrap_token_invalid", { runtime: args.runtime, error });
    throw error;
  }
  Object.defineProperty(args, "edgeBootstrap", {
    value: bootstrap,
    writable: true,
    configurable: true,
    enumerable: false
  });

  if (!args.workDir) {
    writeStatus(args, "Failed", "MissingWorkDir", "work dir is required; pass --work-dir");
    logEvent("error", "worker_workdir_missing", { runtime: args.runtime });
    throw new Error("work dir is required; pass --work-dir");
  }
  mkdirp(args.workDir);

  let commandMissingLogged = false;
  while (!commandExists(adapter.command(args))) {
    writeStatus(args, "Waiting", adapter.commandMissingReason, adapter.commandMissingMessage(args));
    if (!commandMissingLogged) {
      commandMissingLogged = true;
      logEvent("warn", "worker_command_missing", {
        runtime: args.runtime,
        command: adapter.command(args),
        reason: adapter.commandMissingReason
      });
    }
    if (workerOnce(args)) {
      return;
    }
    await wait(Math.max(1, args.intervalSeconds) * 1000);
  }
  logEvent("info", "worker_command_ready", {
    runtime: args.runtime,
    command: adapter.command(args)
  });

  const edge = await exchangeEdgeToken(args, bootstrap);
  logEvent("info", "edge_token_exchanged", {
    runtime: args.runtime,
    workerName: edge.workerName,
    workerResourceName: edge.workerResourceName,
    controllerUrl: args.controllerUrl
  });
  const sts = await requestSts(args, edge);
  logEvent("info", "sts_credentials_ready", {
    runtime: args.runtime,
    bucket: sts.oss_bucket,
    endpoint: sts.oss_endpoint
  });
  const runtimeState = await loadRuntimeConfig(args, edge, sts);
  runtimeState.sts = sts;
  runtimeState.args = args;
  runtimeState.edge = edge;
  args.runtimeState = runtimeState;
  if (typeof adapter.afterRuntimeLoaded === "function") {
    await adapter.afterRuntimeLoaded(args, edge, runtimeState);
  }
  runtimeState.lastRuntimeRefreshAt = Date.now();
  await adapter.applyAgentPackage(args, runtimeState, runtimeState.sts);
  const modelProxy = await adapter.startModelProxy(args, edge, runtimeState);
  logEvent("info", modelProxy ? "model_proxy_started" : "model_proxy_disabled", {
    runtime: args.runtime,
    endpoint: modelProxy?.endpoint || "",
    modelConfigMode: args.modelConfigMode
  });
  const broker = await adapter.startBroker(args, edge, sts, runtimeState);
  if (typeof adapter.afterBrokerStarted === "function") {
    await adapter.afterBrokerStarted(args, edge, runtimeState);
  }
  adapter.applyGlobalIntegrations(args, edge, runtimeState);

  const cleanupRuntime = async () => {
    logEvent("info", "worker_cleanup_started", { runtime: args.runtime });
    if (args.modelProxy?.server) {
      await closeServer(args.modelProxy.server);
    }
    await closeServer(broker);
    adapter.cleanupBrokerFiles(args);
    adapter.cleanupGlobalIntegrations(args);
    logEvent("info", "worker_cleanup_finished", { runtime: args.runtime });
  };
  const removeSignalCleanup = installSignalCleanup(cleanupRuntime);
  await reportHeartbeat(args, edge);

  if (workerOnce(args)) {
    removeSignalCleanup();
    await cleanupRuntime();
    return;
  }

  writeStatus(args, "Running", "WorkerReady", adapter.readyMessage, {
    workerName: edge.workerName,
    workerResourceName: edge.workerResourceName
  });
  logEvent("info", "worker_ready", {
    runtime: args.runtime,
    workerName: edge.workerName,
    workerResourceName: edge.workerResourceName,
    matrixHomeserver: edge.matrixHomeserver
  });
  const stopPeriodicTasks = adapter.startRemotePeriodicTasks(args, edge, runtimeState);
  try {
    await adapter.matrixLoop(args, edge, runtimeState);
  } finally {
    stopPeriodicTasks();
    removeSignalCleanup();
    await cleanupRuntime();
  }
}

module.exports = {
  commandExists,
  wait,
  closeServer,
  installSignalCleanup,
  runWorkerMain
};
