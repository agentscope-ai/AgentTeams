#!/usr/bin/env node
// Control UI device-identity probe (#1460).
//
// Connects to a real OpenClaw gateway as the Control UI would and prints one
// JSON line on stdout:
//
//   {"ok":true,"result":"hello-ok"}
//   {"ok":false,"error":"...","code":"PAIRING_REQUIRED","details":{"requestId":"..."}}
//
// The standalone @openclaw/gateway-client package requires the host to supply
// the device-signing dependencies (hostDeps): signDevicePayload and
// publicKeyRawBase64UrlFromPem. We provide them with Node's built-in Ed25519,
// which is the same key material the SDK/browser provisions; the identity is
// persisted to PROBE_IDENTITY_FILE so a reconnect presents the same device.
//
// Env:
//   PROBE_URL            ws(s)://host:port             (required)
//   PROBE_ORIGIN         Origin header value           (default: URL with http scheme)
//   PROBE_TOKEN          shared gateway token          (required)
//   PROBE_DEVICE         1 = present a device identity, 0 = device-less
//   PROBE_IDENTITY_FILE  path to persist/reuse the identity (required when PROBE_DEVICE=1)
//   PROBE_BUILD_ID       Control UI build id (the gateway's
//                        data-openclaw-control-ui-build-id); omit when the
//                        gateway serves none
//   PROBE_TIMEOUT_MS     handshake budget (default 15000)
//   PROBE_CLIENT_NAME    default openclaw-control-ui
//   PROBE_MODE           default webchat

import crypto from "node:crypto";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { GatewayClient } from "/opt/openclaw/packages/gateway-client/dist/index.mjs";

const url = process.env.PROBE_URL;
const token = process.env.PROBE_TOKEN;
if (!url || !token) {
  console.log(JSON.stringify({ ok: false, error: "PROBE_URL and PROBE_TOKEN are required" }));
  process.exit(2);
}
const origin = process.env.PROBE_ORIGIN || url.replace(/^ws(s?):\/\//, "http$1://");
const withDevice = process.env.PROBE_DEVICE === "1";
const identityFile = process.env.PROBE_IDENTITY_FILE || "";
const buildId = (process.env.PROBE_BUILD_ID || "").trim();
const timeoutMs = Number(process.env.PROBE_TIMEOUT_MS || "15000");

function loadOrCreateIdentity() {
  if (!identityFile) {
    throw new Error("PROBE_IDENTITY_FILE is required when PROBE_DEVICE=1");
  }
  if (existsSync(identityFile)) {
    return JSON.parse(readFileSync(identityFile, "utf8"));
  }
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  const spki = publicKey.export({ type: "spki", format: "der" });
  // Ed25519 SubjectPublicKeyInfo: fixed prefix + the 32-byte raw key.
  const raw = spki.subarray(spki.length - 32);
  const identity = {
    deviceId: crypto.createHash("sha256").update(raw).digest("hex"),
    publicKeyPem: publicKey.export({ type: "spki", format: "pem" }),
    privateKeyPem: privateKey.export({ type: "pkcs8", format: "pem" }),
  };
  writeFileSync(identityFile, JSON.stringify(identity, null, 2), { mode: 0o600 });
  return identity;
}

// The standalone gateway-client resolves these from hostDeps; without them the
// handshake fails with "device signature dependency is not configured".
const hostDeps = {
  signDevicePayload: (privateKeyPem, payload) =>
    crypto
      .sign(null, Buffer.from(payload, "utf8"), crypto.createPrivateKey(privateKeyPem))
      .toString("base64url"),
  publicKeyRawBase64UrlFromPem: (publicKeyPem) => {
    const der = crypto.createPublicKey(publicKeyPem).export({ type: "spki", format: "der" });
    return der.subarray(der.length - 32).toString("base64url");
  },
  loadDeviceAuthToken: () => null,
  storeDeviceAuthToken: () => {},
  clearDeviceAuthToken: () => {},
};

function detailCode(err) {
  const details = err && typeof err === "object" ? err.details : undefined;
  if (details && typeof details === "object" && typeof details.code === "string") {
    return details.code;
  }
  if (err && typeof err === "object" && typeof err.code === "string") {
    return err.code;
  }
  return null;
}

function detailFields(err) {
  const details = err && typeof err === "object" ? err.details : undefined;
  if (!details || typeof details !== "object") {
    return null;
  }
  const out = {};
  for (const key of ["requestId", "gatewayBuildId", "reason", "deviceId"]) {
    if (details[key] !== undefined && details[key] !== null) {
      out[key] = details[key];
    }
  }
  return Object.keys(out).length > 0 ? out : null;
}

let settled = false;
function finish(payload, exitCode) {
  if (settled) return;
  settled = true;
  clearTimeout(timer);
  console.log(JSON.stringify(payload));
  process.exit(exitCode);
}

const timer = setTimeout(
  () => finish({ ok: false, error: "handshake timed out", code: "PROBE_TIMEOUT" }, 1),
  timeoutMs,
);

const client = new GatewayClient({
  url,
  origin,
  token,
  clientName: process.env.PROBE_CLIENT_NAME || "openclaw-control-ui",
  mode: process.env.PROBE_MODE || "webchat",
  role: "operator",
  clientBuildId: buildId || undefined,
  deviceIdentity: withDevice ? loadOrCreateIdentity() : null,
  hostDeps,
  requestTimeoutMs: timeoutMs,
  onHelloOk: () => finish({ ok: true, result: "hello-ok" }, 0),
  onConnectError: (err) =>
    finish(
      {
        ok: false,
        error: err && err.message ? String(err.message) : String(err),
        code: detailCode(err),
        details: detailFields(err),
      },
      0,
    ),
  onClose: (code, reason) =>
    finish({ ok: false, error: `closed ${code} ${reason || ""}`.trim(), code: null }, 0),
});

client.start();
