#!/usr/bin/env node
"use strict";

const fs = require("fs");
const http = require("http");
const path = require("path");
const { logEvent } = require("./log");
const { brokerOssEndpoint } = require("./storage-oss");
const { teamRoomId, personalRoomId, matrixRooms } = require("./runtime-config");

function sendJson(res, status, payload) {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(payload));
}

function defaultDescriptorWriter(args, descriptor) {
  const payload = { ...descriptor, updatedAt: new Date().toISOString() };
  const descriptorFile = path.join(args.stateDir, "credential-broker.json");
  fs.mkdirSync(path.dirname(descriptorFile), { recursive: true });
  fs.writeFileSync(descriptorFile, `${JSON.stringify(payload, null, 2)}\n`, { mode: 0o600 });
  return descriptorFile;
}

function teamNameFromStorage(runtime) {
  const storage = runtime?.storage || {};
  for (const raw of [storage.teamPrefix, storage.sharedPrefix]) {
    const match = String(raw || "").match(/^teams\/([^/]+)/);
    if (match) return match[1];
  }
  return "";
}

async function startBroker(args, edge, sts, runtimeState, options) {
  const opts = options || {};
  const runtimeId = String(opts.runtime || args.runtime || "remote-node");
  const modelConfig = typeof opts.modelConfig === "function"
    ? opts.modelConfig
    : () => ({});
  const modelCredentialsEnabled = opts.modelCredentialsEnabled !== false;
  const writeDescriptor = typeof opts.writeDescriptor === "function"
    ? opts.writeDescriptor
    : defaultDescriptorWriter;
  const refreshMcpConfig = typeof opts.refreshMcpConfig === "function" ? opts.refreshMcpConfig : () => {};
  const clearMcpNeedsAuthCache = typeof opts.clearMcpNeedsAuthCache === "function" ? opts.clearMcpNeedsAuthCache : () => {};

  const token = Buffer.from(`${process.pid}:${Date.now()}:${Math.random()}`).toString("base64url");
  const server = http.createServer((req, res) => {
    const auth = String(req.headers.authorization || "");
    if (auth !== `Bearer ${token}`) {
      sendJson(res, 401, { ok: false, error: "unauthorized" });
      return;
    }
    if (req.method === "GET" && req.url === "/v1/runtime/context") {
      const runtime = runtimeState && runtimeState.runtime ? runtimeState.runtime : {};
      const member = runtime.member || {};
      const desired = runtime.desired || {};
      sendJson(res, 200, {
        ok: true,
        runtime: runtimeId,
        teamName: edge.teamName || runtime.team?.name || runtime.team?.teamName || teamNameFromStorage(runtime),
        memberName: member.name || edge.workerName || "",
        runtimeName: member.runtimeName || edge.runtimeName || edge.workerName || "",
        role: member.role || "remote-member",
        matrixUserId: member.matrixUserId || "",
        teamRoomId: teamRoomId(runtime),
        personalRoomId: member.personalRoomId || "",
        storage: runtime.storage || {},
        skillRegistry: desired.skillRegistry || {},
        runtimeGeneration: runtime.metadata?.generation,
        runtimeDigest: runtimeState.digest || ""
      });
      return;
    }
    if (req.method === "GET" && req.url === "/v1/credentials/matrix") {
      const runtime = runtimeState && runtimeState.runtime ? runtimeState.runtime : {};
      const matrix = runtime.matrix || {};
      const member = runtime.member || {};
      sendJson(res, 200, {
        homeserver: matrix.homeserver || edge.matrixHomeserver || "",
        accessToken: matrix.accessToken || "",
        userId: member.matrixUserId || "",
        teamRoomId: teamRoomId(runtime),
        personalRoomId: personalRoomId(runtime),
        rooms: matrixRooms(runtime)
      });
      return;
    }
    if (req.method === "GET" && req.url === "/v1/credentials/model") {
      if (!modelCredentialsEnabled) {
        sendJson(res, 404, { ok: false, error: "model_credentials_disabled" });
        return;
      }
      const llm = modelConfig(edge, runtimeState) || {};
      sendJson(res, 200, {
        provider: "anthropic-compatible",
        model: llm.model || "",
        baseUrl: llm.baseUrl || "",
        apiKey: llm.apiKey || "",
        runtimeGeneration: runtimeState.runtime?.metadata?.generation,
        runtimeDigest: runtimeState.digest || ""
      });
      return;
    }
    if (req.method === "GET" && req.url === "/v1/credentials/skill-registry") {
      const runtime = runtimeState && runtimeState.runtime ? runtimeState.runtime : {};
      const desired = runtime.desired || {};
      const skillRegistry = desired.skillRegistry || {};
      const currentSts = runtimeState.sts || sts;
      const payload = { ...skillRegistry };
      if (payload.authType === "sts-hiclaw") {
        payload.accessKeyId = currentSts.access_key_id;
        payload.accessKeySecret = currentSts.access_key_secret;
        payload.securityToken = currentSts.security_token;
        payload.expiration = currentSts.expiration || "";
      }
      sendJson(res, 200, payload);
      return;
    }
    if (req.method === "GET" && req.url === "/v1/credentials/storage") {
      const currentSts = runtimeState.sts || sts;
      sendJson(res, 200, {
        provider: "oss",
        accessKeyId: currentSts.access_key_id,
        accessKeySecret: currentSts.access_key_secret,
        securityToken: currentSts.security_token,
        endpoint: brokerOssEndpoint(currentSts),
        bucket: currentSts.oss_bucket,
        expiration: currentSts.expiration || "",
        expiresInSec: currentSts.expires_in_sec
      });
      return;
    }
    sendJson(res, 404, { ok: false, error: "not_found" });
  });

  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  server.unref();
  const address = server.address();
  if (!address || typeof address === "string") {
    throw new Error("failed to start credential broker");
  }
  const endpoint = `http://127.0.0.1:${address.port}`;
  const tokenFile = path.join(args.stateDir, "credential-token");
  fs.mkdirSync(path.dirname(tokenFile), { recursive: true });
  fs.writeFileSync(tokenFile, `${token}\n`, { mode: 0o600 });
  runtimeState.brokerDescriptor = { endpoint, tokenFile, pid: process.pid };
  const descriptorFile = writeDescriptor(args, runtimeState.brokerDescriptor);
  process.env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR = descriptorFile;
  process.env.TEAMHARNESS_NODE_BIN = process.env.TEAMHARNESS_NODE_BIN || process.execPath;
  refreshMcpConfig(args);
  clearMcpNeedsAuthCache();
  logEvent("info", "credential_broker_started", {
    runtime: runtimeId,
    endpoint,
    descriptorFile,
    modelCredentialsEnabled
  });
  return server;
}

module.exports = { startBroker };
