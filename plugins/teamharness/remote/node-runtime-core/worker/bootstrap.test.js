"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");
const { parseRuntimeArgs } = require("./args");
const {
  decodeBootstrapToken,
  encodeBootstrapToken,
  readBootstrapTokenFile,
  writeBootstrapTokenFile
} = require("./bootstrap");

function tempDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-bootstrap-"));
}

function token(overrides = {}) {
  return Buffer.from(JSON.stringify({
    jwtToken: "jwt-initial",
    matrixUrl: "https://matrix.example.com",
    controllerUrl: "https://controller.example.com",
    modelGatewayUrl: "https://model.example.com",
    ...overrides
  })).toString("base64");
}

test("runtime args require bootstrap token file instead of inline token", () => {
  assert.equal(parseRuntimeArgs(["--bootstrap-token-file", "/tmp/bootstrap-token"]).bootstrapTokenFile, "/tmp/bootstrap-token");
  assert.throws(
    () => parseRuntimeArgs(["--bootstrap-token", token()]),
    /unknown argument: --bootstrap-token/
  );
});

test("bootstrap token can be read from the token file", () => {
  const dir = tempDir();
  try {
    const tokenFile = path.join(dir, "credentials", "bootstrap-token");
    fs.mkdirSync(path.dirname(tokenFile), { recursive: true });
    fs.writeFileSync(tokenFile, `${token()}\n`, "utf8");

    const readToken = readBootstrapTokenFile({ bootstrapTokenFile: tokenFile });
    assert.equal(readToken, token());
    assert.equal(decodeBootstrapToken(readToken).jwtToken, "jwt-initial");
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("bootstrap token requires jwt token and can be re-encoded", () => {
  assert.throws(
    () => decodeBootstrapToken(token({ jwtToken: "" })),
    /jwtToken is required/
  );
  assert.throws(
    () => decodeBootstrapToken(token({ workerUuid: "legacy-uuid", jwtToken: undefined })),
    /jwtToken is required/
  );

  const decoded = decodeBootstrapToken(token({ matrixUrl: "https://matrix.example.com/" }));
  assert.equal(decoded.matrixUrl, "https://matrix.example.com");
  decoded.jwtToken = "jwt-refreshed";
  assert.deepEqual(decodeBootstrapToken(encodeBootstrapToken(decoded)), {
    jwtToken: "jwt-refreshed",
    matrixUrl: "https://matrix.example.com",
    controllerUrl: "https://controller.example.com",
    modelGatewayUrl: "https://model.example.com"
  });
});

test("bootstrap token file writes replace the file for future rotation", () => {
  const dir = tempDir();
  try {
    const tokenFile = path.join(dir, "credentials", "bootstrap-token");
    const nextToken = token({ matrixUrl: "https://matrix-2.example.com" });

    writeBootstrapTokenFile({ bootstrapTokenFile: tokenFile }, token());
    writeBootstrapTokenFile({ bootstrapTokenFile: tokenFile }, nextToken);

    assert.equal(fs.readFileSync(tokenFile, "utf8").trim(), nextToken);
    if (process.platform !== "win32") {
      assert.equal((fs.statSync(tokenFile).mode & 0o777), 0o600);
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
