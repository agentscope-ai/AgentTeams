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
//   PROBE_BUILD_ID       Exact Control UI build id (e.g. details.gatewayBuildId
//                        from a PROTOCOL_MISMATCH); highest precedence
//   PROBE_RAW_BUILD_ID   Raw value of the served
//                        data-openclaw-control-ui-build-id attribute. That
//                        attribute is an artifact stamp, NOT the runtime build
//                        id the gateway compares against: it is only a last
//                        resort (normalized like the UI) when the gateway's own
//                        dist/build-info.json cannot be read
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
const exactBuildId = (process.env.PROBE_BUILD_ID || "").trim();
const rawBuildId = (process.env.PROBE_RAW_BUILD_ID || "").trim();

// The gateway compares client.buildId against resolveRuntimeServiceBuildId(),
// which reads build-info.json next to the built module (src/version.ts:
// BUILD_INFO_CANDIDATES relative to dist/index.js). Mirror that resolution so
// the probe presents the exact value the gateway expects, instead of the
// 112-character artifact stamp the HTML attribute carries.
function readRuntimeBuildIdFromBuildInfo() {
  const candidates = [
    "/opt/openclaw/build-info.json",
    "/opt/openclaw/../build-info.json",
    "/opt/openclaw/dist/build-info.json",
  ];
  for (const candidate of candidates) {
    try {
      if (!existsSync(candidate)) continue;
      const parsed = JSON.parse(readFileSync(candidate, "utf8"));
      const buildId = typeof parsed.buildId === "string" ? parsed.buildId.trim() : "";
      if (buildId && buildId.length <= 96) {
        return buildId;
      }
    } catch {
      // ignore unreadable candidates, same as the runtime resolver
    }
  }
  return "";
}

// Mirrors ui/src/build-info-normalizers.ts::normalizeControlUiBuildId: the
// served attribute is an artifact stamp (observed 112 chars) while the connect
// schema caps client.buildId at 96 characters, so the UI strips characters
// outside [A-Za-z0-9._-] and truncates before sending it.
function normalizeControlUiBuildId(value) {
  const normalized = String(value || "").trim().replace(/[^a-zA-Z0-9._-]+/g, "-");
  return normalized.slice(0, 96) || "dev";
}

const buildInfoBuildId = exactBuildId ? "" : readRuntimeBuildIdFromBuildInfo();
const buildId =
  exactBuildId || buildInfoBuildId || (rawBuildId ? normalizeControlUiBuildId(rawBuildId) : "");
const buildIdSource = exactBuildId
  ? "explicit"
  : buildInfoBuildId
    ? "build-info.json"
    : rawBuildId
      ? "normalized-html-attribute"
      : "none";
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
  onHelloOk: () => finish({ ok: true, result: "hello-ok", buildId, buildIdSource }, 0),
  onConnectError: (err) =>
    finish(
      {
        ok: false,
        error: err && err.message ? String(err.message) : String(err),
        code: detailCode(err),
        details: detailFields(err),
        buildId,
        buildIdSource,
      },
      0,
    ),
  onClose: (code, reason) =>
    finish({ ok: false, error: `closed ${code} ${reason || ""}`.trim(), code: null }, 0),
});

client.start();
