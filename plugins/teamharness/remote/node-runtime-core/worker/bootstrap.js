#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");

function readBootstrapTokenFile(args) {
  const tokenFile = args && args.bootstrapTokenFile ? String(args.bootstrapTokenFile).trim() : "";
  if (!tokenFile) {
    throw Object.assign(new Error("bootstrap token file is required; pass --bootstrap-token-file"), {
      reason: "MissingBootstrapToken"
    });
  }
  let token;
  try {
    token = fs.readFileSync(tokenFile, "utf8").trim();
  } catch (error) {
    throw Object.assign(new Error(`failed to read bootstrap token file: ${error.message}`), {
      reason: "MissingBootstrapToken"
    });
  }
  if (!token) {
    throw Object.assign(new Error("bootstrap token file is empty"), {
      reason: "MissingBootstrapToken"
    });
  }
  return token;
}

function writeBootstrapTokenFile(args, token) {
  const tokenFile = args && args.bootstrapTokenFile ? String(args.bootstrapTokenFile).trim() : "";
  if (!tokenFile) {
    throw Object.assign(new Error("bootstrap token file is required; pass --bootstrap-token-file"), {
      reason: "MissingBootstrapToken"
    });
  }
  const value = String(token || "").trim();
  if (!value) {
    throw Object.assign(new Error("bootstrap token is empty"), {
      reason: "MissingBootstrapToken"
    });
  }
  fs.mkdirSync(path.dirname(tokenFile), { recursive: true });
  const tempFile = path.join(path.dirname(tokenFile), `.bootstrap-token.${process.pid}.${Date.now()}.tmp`);
  fs.writeFileSync(tempFile, `${value}\n`, { encoding: "utf8", mode: 0o600 });
  fs.chmodSync(tempFile, 0o600);
  fs.renameSync(tempFile, tokenFile);
  try {
    fs.chmodSync(tokenFile, 0o600);
  } catch {
    // Some filesystems do not support chmod; the private worker directory still scopes access.
  }
}

function decodeBootstrapToken(token) {
  if (!token || !token.trim()) {
    throw Object.assign(new Error("bootstrap token is required"), {
      reason: "MissingBootstrapToken"
    });
  }
  let payload;
  try {
    payload = JSON.parse(Buffer.from(token.trim(), "base64").toString("utf8"));
  } catch (error) {
    throw Object.assign(new Error(`invalid bootstrap token: ${error.message}`), {
      reason: "InvalidBootstrapToken"
    });
  }
  for (const key of ["jwtToken", "matrixUrl", "controllerUrl", "modelGatewayUrl"]) {
    if (!payload || typeof payload[key] !== "string" || !payload[key].trim()) {
      throw Object.assign(new Error(`invalid bootstrap token: ${key} is required`), {
        reason: "InvalidBootstrapToken"
      });
    }
    if (key === "jwtToken") {
      continue;
    }
    const parsed = new URL(payload[key]);
    if (!["http:", "https:"].includes(parsed.protocol)) {
      throw Object.assign(new Error(`invalid bootstrap token: ${key} must be http(s)`), {
        reason: "InvalidBootstrapToken"
      });
    }
  }
  return {
    jwtToken: payload.jwtToken.trim(),
    matrixUrl: payload.matrixUrl.trim().replace(/\/+$/, ""),
    controllerUrl: payload.controllerUrl.trim().replace(/\/+$/, ""),
    modelGatewayUrl: payload.modelGatewayUrl.trim().replace(/\/+$/, "")
  };
}

function encodeBootstrapToken(bootstrap) {
  const payload = {
    jwtToken: String(bootstrap?.jwtToken || "").trim(),
    matrixUrl: String(bootstrap?.matrixUrl || "").trim(),
    controllerUrl: String(bootstrap?.controllerUrl || "").trim(),
    modelGatewayUrl: String(bootstrap?.modelGatewayUrl || "").trim()
  };
  for (const key of Object.keys(payload)) {
    if (!payload[key]) {
      throw Object.assign(new Error(`invalid bootstrap token: ${key} is required`), {
        reason: "InvalidBootstrapToken"
      });
    }
  }
  return Buffer.from(JSON.stringify(payload)).toString("base64");
}

module.exports = {
  decodeBootstrapToken,
  encodeBootstrapToken,
  readBootstrapTokenFile,
  writeBootstrapTokenFile
};
