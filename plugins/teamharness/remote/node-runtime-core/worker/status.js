#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");

function mkdirp(dir) {
  fs.mkdirSync(dir, { recursive: true });
}

function writeJson(file, payload, mode) {
  mkdirp(path.dirname(file));
  const targetMode = mode || 0o600;
  const tmp = path.join(path.dirname(file), `.${path.basename(file)}.${process.pid}.${Date.now()}.tmp`);
  fs.writeFileSync(tmp, `${JSON.stringify(payload, null, 2)}\n`, { mode: targetMode });
  fs.renameSync(tmp, file);
  try {
    fs.chmodSync(file, targetMode);
  } catch {
    // Best effort; the write already succeeded.
  }
}

function readJson(file, fallback) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch {
    return fallback;
  }
}

function runtimeId(args) {
  return String(args.runtime || args.runtimeId || "remote-node");
}

function writeStatus(args, phase, reason, message, extra) {
  const payload = {
    phase,
    reason,
    message,
    updatedAt: new Date().toISOString(),
    runtime: runtimeId(args),
    instanceId: args.instanceId || "",
    ...(extra || {})
  };
  writeJson(path.join(args.stateDir, "status.json"), payload);
}

function removeFileQuietly(file) {
  try {
    fs.rmSync(file, { force: true });
  } catch {
    // Best-effort cleanup only.
  }
}

function cleanupLegacySensitiveState(args) {
  for (const name of ["edge-token.json", "sts.json", "runtime-state.json", "credential-token", "credential-broker.json"]) {
    removeFileQuietly(path.join(args.stateDir, name));
  }
}

function positiveIntervalSeconds(value, fallback) {
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 ? number : fallback;
}

function stsRefreshRequired(sts, nowSeconds) {
  if (!sts || typeof sts !== "object") return true;
  const issuedAt = Number(sts.issuedAt || 0);
  const expiresIn = Number(sts.expires_in_sec || sts.expiresInSeconds || 0);
  if (!issuedAt || !expiresIn) return true;
  const now = nowSeconds || Math.floor(Date.now() / 1000);
  const refreshAt = Math.min(issuedAt + expiresIn * 0.8, issuedAt + Math.max(expiresIn - 300, 0));
  return now >= refreshAt;
}

module.exports = {
  mkdirp,
  writeJson,
  readJson,
  writeStatus,
  removeFileQuietly,
  cleanupLegacySensitiveState,
  positiveIntervalSeconds,
  stsRefreshRequired
};
