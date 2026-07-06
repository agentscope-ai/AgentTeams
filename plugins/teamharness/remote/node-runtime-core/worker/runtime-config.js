#!/usr/bin/env node
"use strict";

const crypto = require("crypto");
const path = require("path");
const { logEvent } = require("./log");
const { writeJson, writeStatus, positiveIntervalSeconds, stsRefreshRequired } = require("./status");
const { requestSts } = require("./controller");
const { ossGet } = require("./storage-oss");

function parseScalar(value) {
  const raw = String(value || "").trim();
  if (!raw) {
    return "";
  }
  if ((raw.startsWith('"') && raw.endsWith('"')) || (raw.startsWith("'") && raw.endsWith("'"))) {
    return raw.slice(1, -1);
  }
  if (raw === "true") {
    return true;
  }
  if (raw === "false") {
    return false;
  }
  if (/^-?\d+(\.\d+)?$/.test(raw)) {
    return Number(raw);
  }
  return raw;
}

function parseRuntimeYaml(text) {
  const root = {};
  const stack = [{ indent: -1, value: root }];
  for (const originalLine of String(text || "").split(/\r?\n/)) {
    if (!originalLine.trim() || originalLine.trimStart().startsWith("#")) {
      continue;
    }
    const indent = originalLine.match(/^\s*/)[0].length;
    const line = originalLine.trim();
    if (line.startsWith("- ")) {
      continue;
    }
    const match = line.match(/^([A-Za-z0-9_.-]+):(?:\s*(.*))?$/);
    if (!match) {
      continue;
    }
    while (stack.length > 1 && indent <= stack[stack.length - 1].indent) {
      stack.pop();
    }
    const parent = stack[stack.length - 1].value;
    const key = match[1];
    const rest = match[2] || "";
    if (!rest.trim()) {
      parent[key] = {};
      stack.push({ indent, value: parent[key] });
    } else {
      parent[key] = parseScalar(rest);
    }
  }
  return root;
}

function runtimeObjectKey(workerName) {
  return `shared/runtime/members/${workerName}/runtime.yaml`;
}

function teamRoomId(runtime) {
  return String(runtime?.matrix?.teamRoomId || runtime?.team?.teamRoomId || runtime?.team?.roomId || "").trim();
}

function personalRoomId(runtime) {
  return String(runtime?.member?.personalRoomId || "").trim();
}

function matrixRooms(runtime) {
  const seen = new Set();
  const rooms = [];
  for (const roomId of [teamRoomId(runtime), personalRoomId(runtime)]) {
    if (roomId && !seen.has(roomId)) {
      seen.add(roomId);
      rooms.push(roomId);
    }
  }
  return rooms;
}

function runtimeStateSnapshot(runtimeState) {
  const parsed = runtimeState.runtime || {};
  const desired = parsed.desired || {};
  const model = desired.model || {};
  return {
    objectKey: runtimeState.objectKey,
    digest: runtimeState.digest,
    loadedAt: runtimeState.loadedAt,
    member: parsed.member || {},
    desired: {
      model: {
        model: model.model || "",
        providerId: model.providerId || "",
        gatewayUrl: model.gatewayUrl || "",
        hasGatewayKey: Boolean(String(model.gatewayKey || "").trim())
      },
      agentPackage: desired.agentPackage || {},
      skillRegistry: desired.skillRegistry || {}
    },
    storage: parsed.storage || {}
  };
}

async function loadRuntimeConfig(args, edge, sts) {
  const objectKey = runtimeObjectKey(edge.workerName);
  const yaml = await ossGet(sts, objectKey);
  const parsed = parseRuntimeYaml(yaml);
  const digest = crypto.createHash("sha256").update(yaml).digest("hex");
  const runtimeState = {
    objectKey,
    digest,
    loadedAt: new Date().toISOString(),
    runtime: parsed
  };
  writeJson(path.join(args.stateDir, "runtime-state.json"), runtimeStateSnapshot(runtimeState));
  writeStatus(args, "Running", "RuntimeConfigReady", "runtime.yaml loaded", {
    workerName: edge.workerName,
    runtimeDigest: digest
  });
  logEvent("info", "runtime_config_loaded", {
    runtime: args.runtime,
    workerName: edge.workerName,
    objectKey,
    digest,
    teamRoomId: teamRoomId(parsed),
    personalRoomId: personalRoomId(parsed)
  });
  return runtimeState;
}

function updateRuntimeState(target, source) {
  target.objectKey = source.objectKey;
  target.digest = source.digest;
  target.loadedAt = source.loadedAt;
  target.runtime = source.runtime;
}

function runtimeRefreshDue(args, runtimeState) {
  const interval = positiveIntervalSeconds(args.runtimeRefreshIntervalSeconds, 60);
  if (interval === 0) return true;
  const last = Number(runtimeState.lastRuntimeRefreshAt || 0);
  return Date.now() - last >= interval * 1000;
}

async function refreshRemoteRuntime(args, edge, runtimeState, options) {
  if (runtimeState.refreshPromise) {
    return runtimeState.refreshPromise;
  }
  const force = Boolean(options && options.force);
  runtimeState.refreshPromise = (async () => {
    if (stsRefreshRequired(runtimeState.sts)) {
      runtimeState.sts = await requestSts(args, edge);
    }
    if (force || runtimeRefreshDue(args, runtimeState)) {
      const next = await loadRuntimeConfig(args, edge, runtimeState.sts);
      updateRuntimeState(runtimeState, next);
      runtimeState.lastRuntimeRefreshAt = Date.now();
      logEvent("info", "runtime_config_refreshed", {
        runtime: args.runtime,
        workerName: edge.workerName,
        force,
        digest: runtimeState.digest
      });
    }
    return runtimeState;
  })().finally(() => {
    runtimeState.refreshPromise = null;
  });
  return runtimeState.refreshPromise;
}

module.exports = {
  parseScalar,
  parseRuntimeYaml,
  runtimeObjectKey,
  teamRoomId,
  personalRoomId,
  matrixRooms,
  runtimeStateSnapshot,
  loadRuntimeConfig,
  updateRuntimeState,
  runtimeRefreshDue,
  refreshRemoteRuntime
};
