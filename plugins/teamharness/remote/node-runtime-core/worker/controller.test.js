"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");
const { decodeBootstrapToken, encodeBootstrapToken, writeBootstrapTokenFile } = require("./bootstrap");
const { edgeTokenRefreshRequired, exchangeEdgeToken, heartbeatPayload, reportHeartbeat, requestSts } = require("./controller");

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => resolve(server.address().port));
  });
}

async function readBody(req) {
  const chunks = [];
  for await (const chunk of req) chunks.push(Buffer.from(chunk));
  return Buffer.concat(chunks).toString("utf8");
}

test("edge token exchange uses jwt and persists refreshed bootstrap token", async () => {
  const requests = [];
  const server = http.createServer(async (req, res) => {
    const body = await readBody(req);
    requests.push({ method: req.method, url: req.url, body });
    if (req.method === "POST" && req.url === "/api/v1/edge/token") {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({
        token: "edge-token",
        expiresAt: "2026-06-24T12:00:00Z",
        jwtToken: "jwt-refreshed",
        jwtExpiresAt: "2026-07-01T12:00:00Z",
        workerName: "worker-01",
        workerResourceName: "worker-resource",
        runtimeName: "worker-01",
        teamName: "team-a"
      }));
      return;
    }
    res.writeHead(404);
    res.end("not found");
  });

  const port = await listen(server);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-controller-"));
  try {
    const args = {
      bootstrapTokenFile: path.join(dir, "credentials", "bootstrap-token"),
      stateDir: dir,
      runtime: "core-test"
    };
    const initialBootstrap = {
      jwtToken: "jwt-initial",
      matrixUrl: `http://127.0.0.1:${port}/matrix`,
      controllerUrl: `http://127.0.0.1:${port}`,
      modelGatewayUrl: `http://127.0.0.1:${port}/gateway`
    };
    writeBootstrapTokenFile(args, encodeBootstrapToken(initialBootstrap));

    const edge = await exchangeEdgeToken(args, decodeBootstrapToken(fs.readFileSync(args.bootstrapTokenFile, "utf8")));

    assert.deepEqual(JSON.parse(requests[0].body), { jwtToken: "jwt-initial" });
    assert.equal(edge.workerName, "worker-01");
    assert.equal(edge.workerResourceName, "worker-resource");
    assert.equal(edge.teamName, "team-a");
    assert.equal(edge.token, "edge-token");
    assert.equal(edge.expiresAt, "2026-06-24T12:00:00Z");
    assert.equal(edge.jwtExpiresAt, "2026-07-01T12:00:00Z");

    const persisted = decodeBootstrapToken(fs.readFileSync(args.bootstrapTokenFile, "utf8"));
    assert.deepEqual(persisted, {
      jwtToken: "jwt-refreshed",
      matrixUrl: initialBootstrap.matrixUrl,
      controllerUrl: initialBootstrap.controllerUrl,
      modelGatewayUrl: initialBootstrap.modelGatewayUrl
    });
    assert(!fs.readFileSync(path.join(dir, "edge-state.json"), "utf8").includes("jwt-refreshed"));
  } finally {
    await new Promise(resolve => server.close(resolve));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("edge token refresh checks token expiry window", () => {
  assert.equal(edgeTokenRefreshRequired({ token: "edge-token" }, 1000), false);
  assert.equal(edgeTokenRefreshRequired({
    token: "edge-token",
    updatedAt: 1000,
    expiresAt: new Date(4600 * 1000).toISOString()
  }, 3900), true);
  assert.equal(edgeTokenRefreshRequired({
    token: "edge-token",
    updatedAt: 1000,
    expiresAt: new Date(4600 * 1000).toISOString()
  }, 2000), false);
});

test("heartbeat reports runtime and local IP in request body", async () => {
  const requests = [];
  const server = http.createServer(async (req, res) => {
    const body = await readBody(req);
    requests.push({ method: req.method, url: req.url, body, authorization: req.headers.authorization || "" });
    if (req.method === "POST" && req.url === "/api/v1/workers/worker-resource/heartbeat") {
      res.writeHead(204);
      res.end();
      return;
    }
    res.writeHead(404);
    res.end("not found");
  });

  const oldNetworkInterfaces = os.networkInterfaces;
  const port = await listen(server);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-controller-"));
  try {
    os.networkInterfaces = () => ({
      lo0: [{ address: "127.0.0.1", family: "IPv4", internal: true }],
      en0: [{ address: "192.168.1.10", family: "IPv4", internal: false }]
    });
    const args = { stateDir: dir, runtime: "openclaw" };
    const edge = {
      controllerUrl: `http://127.0.0.1:${port}`,
      token: "edge-token",
      workerName: "worker-01",
      workerResourceName: "worker-resource"
    };

    assert.deepEqual(heartbeatPayload(args), {
      localIP: "192.168.1.10",
      runtime: "openclaw"
    });

    await reportHeartbeat(args, edge);

    assert.equal(requests[0].authorization, "Bearer edge-token");
    assert.deepEqual(JSON.parse(requests[0].body), {
      localIP: "192.168.1.10",
      runtime: "openclaw"
    });
  } finally {
    os.networkInterfaces = oldNetworkInterfaces;
    await new Promise(resolve => server.close(resolve));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("STS request refreshes expired edge token before controller calls", async () => {
  const requests = [];
  const server = http.createServer(async (req, res) => {
    const body = await readBody(req);
    requests.push({ method: req.method, url: req.url, body, authorization: req.headers.authorization || "" });
    if (req.method === "POST" && req.url === "/api/v1/edge/token") {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({
        token: "edge-token-refreshed",
        expiresAt: "2099-06-24T12:00:00Z",
        jwtToken: "jwt-refreshed",
        jwtExpiresAt: "2099-07-01T12:00:00Z",
        workerName: "worker-01",
        workerResourceName: "worker-resource"
      }));
      return;
    }
    if (req.method === "POST" && req.url === "/api/v1/credentials/sts") {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({
        access_key_id: "ak",
        access_key_secret: "sk",
        security_token: "sts-token",
        oss_endpoint: "https://oss.example.com",
        oss_bucket: "bucket",
        expires_in_sec: 3600
      }));
      return;
    }
    res.writeHead(404);
    res.end("not found");
  });

  const port = await listen(server);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-controller-"));
  try {
    const args = {
      bootstrapTokenFile: path.join(dir, "credentials", "bootstrap-token"),
      stateDir: dir,
      runtime: "core-test"
    };
    const bootstrap = {
      jwtToken: "jwt-initial",
      matrixUrl: `http://127.0.0.1:${port}/matrix`,
      controllerUrl: `http://127.0.0.1:${port}`,
      modelGatewayUrl: `http://127.0.0.1:${port}/gateway`
    };
    args.edgeBootstrap = bootstrap;
    writeBootstrapTokenFile(args, encodeBootstrapToken(bootstrap));

    const edge = {
      controllerUrl: bootstrap.controllerUrl,
      modelGatewayUrl: bootstrap.modelGatewayUrl,
      matrixHomeserver: bootstrap.matrixUrl,
      token: "edge-token-expired",
      workerName: "worker-01",
      workerResourceName: "worker-resource",
      updatedAt: 1,
      expiresAt: "2000-01-01T00:00:00Z"
    };

    const sts = await requestSts(args, edge);

    assert.equal(sts.security_token, "sts-token");
    assert.equal(edge.token, "edge-token-refreshed");
    assert.equal(requests[0].url, "/api/v1/edge/token");
    assert.deepEqual(JSON.parse(requests[0].body), { jwtToken: "jwt-initial" });
    assert.equal(requests[1].url, "/api/v1/credentials/sts");
    assert.equal(requests[1].authorization, "Bearer edge-token-refreshed");
    const persisted = decodeBootstrapToken(fs.readFileSync(args.bootstrapTokenFile, "utf8"));
    assert.equal(persisted.jwtToken, "jwt-refreshed");
  } finally {
    await new Promise(resolve => server.close(resolve));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
