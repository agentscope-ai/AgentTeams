#!/usr/bin/env node
// Control UI device-identity probe (#1460).
//
// Connects to a real OpenClaw gateway as the Control UI would, and reports the
// handshake outcome as one JSON line on stdout:
//
//   {"ok":true,"result":"hello-ok"}
//   {"ok":false,"error":"...","code":"CONTROL_UI_DEVICE_IDENTITY_REQUIRED"}
//
// The device identity, when enabled, is generated exactly the way the SDK's
// generateStoredDeviceIdentity() does (Ed25519 keypair, device id =
// sha256(raw public key)); it is persisted to PROBE_IDENTITY_FILE so a
// reconnect (after approval) presents the same device.
//
// Env:
//   PROBE_URL            ws(s)://host:port           (required)
//   PROBE_ORIGIN         Origin header value         (default: URL with http scheme)
//   PROBE_TOKEN          shared gateway token        (required)
//   PROBE_DEVICE         1 = present a device identity, 0 = device-less
//   PROBE_IDENTITY_FILE  path to persist/reuse the identity (optional)
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
const timeoutMs = Number(process.env.PROBE_TIMEOUT_MS || "15000");

function loadOrCreateIdentity() {
  if (identityFile && existsSync(identityFile)) {
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
  if (identityFile) {
    writeFileSync(identityFile, JSON.stringify(identity, null, 2), { mode: 0o600 });
  }
  return identity;
}

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
  deviceIdentity: withDevice ? loadOrCreateIdentity() : null,
  requestTimeoutMs: timeoutMs,
  onHelloOk: () => finish({ ok: true, result: "hello-ok" }, 0),
  onConnectError: (err) =>
    finish(
      {
        ok: false,
        error: err && err.message ? String(err.message) : String(err),
        code: detailCode(err),
      },
      0,
    ),
  onClose: (code, reason) =>
    finish({ ok: false, error: `closed ${code} ${reason || ""}`.trim(), code: null }, 0),
});

client.start();
