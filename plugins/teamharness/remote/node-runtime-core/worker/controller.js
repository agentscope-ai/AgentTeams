#!/usr/bin/env node
"use strict";

const path = require("path");
const os = require("os");
const { encodeBootstrapToken, writeBootstrapTokenFile } = require("./bootstrap");
const { writeJson, writeStatus } = require("./status");

async function postJson(baseUrl, requestPath, payload, headers) {
  const url = new URL(requestPath, `${baseUrl}/`);
  const response = await fetch(url, {
    method: "POST",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json; charset=utf-8",
      ...(headers || {})
    },
    body: JSON.stringify(payload || {})
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`POST ${url} failed: ${response.status} ${text}`);
  }
  if (!text.trim()) {
    return {};
  }
  return JSON.parse(text);
}

async function postEmpty(baseUrl, requestPath, headers) {
  const url = new URL(requestPath, `${baseUrl}/`);
  const response = await fetch(url, {
    method: "POST",
    headers: {
      Accept: "application/json",
      ...(headers || {})
    }
  });
  if (!response.ok) {
    throw new Error(`POST ${url} failed: ${response.status} ${await response.text()}`);
  }
}

function localIPAddress() {
  const fallback = [];
  for (const entries of Object.values(os.networkInterfaces())) {
    for (const entry of entries || []) {
      if (!entry || entry.internal || !entry.address) continue;
      if (entry.family === "IPv4" || entry.family === 4) return entry.address;
      fallback.push(entry.address);
    }
  }
  return fallback[0] || "";
}

function heartbeatPayload(args) {
  return {
    localIP: localIPAddress(),
    runtime: String(args?.runtime || "").trim()
  };
}

async function exchangeEdgeToken(args, bootstrap) {
  const response = await postJson(bootstrap.controllerUrl, "/api/v1/edge/token", {
    jwtToken: bootstrap.jwtToken
  });
  const token = String(response.token || "").trim();
  const workerName = String(response.workerName || "").trim();
  if (!token || !workerName) {
    throw new Error("edge token response missing token or workerName");
  }
  const refreshedJWT = String(response.jwtToken || "").trim();
  if (refreshedJWT) {
    bootstrap.jwtToken = refreshedJWT;
    writeBootstrapTokenFile(args, encodeBootstrapToken(bootstrap));
  }
  const edge = {
    controllerUrl: bootstrap.controllerUrl,
    modelGatewayUrl: bootstrap.modelGatewayUrl,
    matrixHomeserver: bootstrap.matrixUrl,
    token,
    workerName,
    workerResourceName: String(response.workerResourceName || workerName).trim(),
    runtimeName: String(response.runtimeName || workerName).trim(),
    teamName: String(response.teamName || "").trim(),
    expiresAt: String(response.expiresAt || "").trim(),
    jwtExpiresAt: String(response.jwtExpiresAt || "").trim(),
    updatedAt: Math.floor(Date.now() / 1000)
  };
  writeJson(path.join(args.stateDir, "edge-state.json"), {
    controllerUrl: edge.controllerUrl,
    modelGatewayUrl: edge.modelGatewayUrl,
    matrixHomeserver: edge.matrixHomeserver,
    workerName: edge.workerName,
    workerResourceName: edge.workerResourceName,
    runtimeName: edge.runtimeName,
    teamName: edge.teamName,
    expiresAt: edge.expiresAt,
    jwtExpiresAt: edge.jwtExpiresAt,
    updatedAt: edge.updatedAt
  });
  writeStatus(args, "Running", "EdgeTokenReady", "edge token exchanged", {
    workerName: edge.workerName,
    workerResourceName: edge.workerResourceName
  });
  return edge;
}

function edgeTokenRefreshRequired(edge, nowSeconds) {
  if (!edge || typeof edge !== "object") return true;
  if (!String(edge.token || "").trim()) return true;
  const expiresAt = Date.parse(String(edge.expiresAt || "")) / 1000;
  if (!Number.isFinite(expiresAt) || expiresAt <= 0) return false;
  const now = nowSeconds === undefined ? Math.floor(Date.now() / 1000) : nowSeconds;
  if (now >= expiresAt) return true;
  const issuedAt = Number(edge.updatedAt || 0);
  if (!issuedAt || issuedAt >= expiresAt) {
    return now >= expiresAt - 300;
  }
  const lifetime = expiresAt - issuedAt;
  const refreshAt = Math.min(issuedAt + lifetime * 0.8, issuedAt + Math.max(lifetime - 300, 0));
  return now >= refreshAt;
}

function setHidden(target, key, value) {
  Object.defineProperty(target, key, {
    value,
    writable: true,
    configurable: true,
    enumerable: false
  });
}

async function refreshEdgeToken(args, edge, bootstrap) {
  if (!bootstrap || !bootstrap.jwtToken) {
    throw new Error("edge bootstrap token is unavailable; cannot refresh edge token");
  }
  if (args.edgeTokenRefreshPromise) {
    return args.edgeTokenRefreshPromise;
  }
  const promise = exchangeEdgeToken(args, bootstrap).then(nextEdge => {
    Object.assign(edge, nextEdge);
    return edge;
  });
  setHidden(args, "edgeTokenRefreshPromise", promise);
  try {
    return await promise;
  } finally {
    setHidden(args, "edgeTokenRefreshPromise", null);
  }
}

async function ensureEdgeTokenFresh(args, edge, options) {
  const force = Boolean(options && options.force);
  if (!force && !edgeTokenRefreshRequired(edge, options && options.nowSeconds)) {
    return edge;
  }
  return refreshEdgeToken(args, edge, args.edgeBootstrap);
}

async function requestSts(args, edge) {
  await ensureEdgeTokenFresh(args, edge);
  const sts = await postJson(edge.controllerUrl, "/api/v1/credentials/sts", {}, {
    Authorization: `Bearer ${edge.token}`
  });
  const required = ["access_key_id", "access_key_secret", "security_token", "oss_endpoint", "oss_bucket"];
  const missing = required.filter(key => !String(sts[key] || "").trim());
  if (missing.length) {
    throw new Error(`STS response missing required fields: ${missing.join(", ")}`);
  }
  const payload = {
    ...sts,
    controllerUrl: edge.controllerUrl,
    modelGatewayUrl: edge.modelGatewayUrl,
    workerName: edge.workerName,
    workerResourceName: edge.workerResourceName,
    issuedAt: Math.floor(Date.now() / 1000)
  };
  writeStatus(args, "Running", "StsReady", "STS credential refreshed", {
    workerName: edge.workerName
  });
  return payload;
}

async function reportHeartbeat(args, edge) {
  await ensureEdgeTokenFresh(args, edge);
  const workerPath = encodeURIComponent(edge.workerResourceName || edge.workerName);
  const payload = heartbeatPayload(args);
  await postJson(edge.controllerUrl, `/api/v1/workers/${workerPath}/heartbeat`, payload, {
    Authorization: `Bearer ${edge.token}`
  });
  writeStatus(args, "Running", "HeartbeatReported", "worker heartbeat reported", {
    workerName: edge.workerName,
    workerResourceName: edge.workerResourceName,
    localIP: payload.localIP,
    runtime: payload.runtime
  });
}

module.exports = {
  postJson,
  postEmpty,
  localIPAddress,
  heartbeatPayload,
  exchangeEdgeToken,
  edgeTokenRefreshRequired,
  ensureEdgeTokenFresh,
  requestSts,
  reportHeartbeat
};
