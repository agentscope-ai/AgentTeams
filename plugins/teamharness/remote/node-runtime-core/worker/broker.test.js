"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");
const { startBroker } = require("./broker");

function tempStateDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-broker-"));
}

function readDescriptor(stateDir) {
  const descriptor = JSON.parse(fs.readFileSync(path.join(stateDir, "credential-broker.json"), "utf8"));
  const token = fs.readFileSync(descriptor.tokenFile, "utf8").trim();
  return { ...descriptor, token };
}

async function brokerGet(descriptor, pathname) {
  const response = await fetch(`${descriptor.endpoint}${pathname}`, {
    headers: { Authorization: `Bearer ${descriptor.token}`, Accept: "application/json" }
  });
  const text = await response.text();
  return {
    status: response.status,
    body: text ? JSON.parse(text) : {}
  };
}

function fixture() {
  return {
    args: { stateDir: tempStateDir(), runtime: "openclaw" },
    edge: { workerName: "worker-01", runtimeName: "worker-01", teamName: "team-a" },
    sts: {
      access_key_id: "ak",
      access_key_secret: "sk",
      security_token: "token",
      oss_endpoint: "https://oss.example.com",
      oss_bucket: "bucket"
    },
    runtimeState: {
      digest: "digest-1",
      runtime: {
        metadata: { generation: 3 },
        member: {
          name: "worker-cr",
          runtimeName: "worker-01",
          role: "worker",
          matrixUserId: "@worker-01:matrix.example"
        },
        team: { name: "team-a" },
        desired: {
          skillRegistry: { provider: "nacos" }
        },
        matrix: {
          homeserver: "https://matrix.example",
          accessToken: "mx-token",
          teamRoomId: "!team:matrix.example",
          personalRoomId: "!dm:matrix.example"
        },
        storage: {
          memberPrefix: "agents/worker-01"
        }
      }
    }
  };
}

test("broker exposes model credentials by default", async () => {
  const { args, edge, sts, runtimeState } = fixture();
  const server = await startBroker(args, edge, sts, runtimeState, {
    runtime: "openclaw",
    modelConfig: () => ({
      model: "glm-5.2",
      baseUrl: "https://gateway.example/v1",
      apiKey: "model-token"
    })
  });

  try {
    const response = await brokerGet(readDescriptor(args.stateDir), "/v1/credentials/model");
    assert.equal(response.status, 200);
    assert.equal(response.body.model, "glm-5.2");
    assert.equal(response.body.baseUrl, "https://gateway.example/v1");
    assert.equal(response.body.apiKey, "model-token");
  } finally {
    server.close();
    fs.rmSync(args.stateDir, { recursive: true, force: true });
  }
});

test("broker can disable only the model credential endpoint", async () => {
  const { args, edge, sts, runtimeState } = fixture();
  const server = await startBroker(args, edge, sts, runtimeState, {
    runtime: "openclaw",
    modelCredentialsEnabled: false,
    modelConfig: () => ({
      model: "glm-5.2",
      baseUrl: "https://gateway.example/v1",
      apiKey: "model-token"
    })
  });

  try {
    const descriptor = readDescriptor(args.stateDir);
    const model = await brokerGet(descriptor, "/v1/credentials/model");
    assert.equal(model.status, 404);
    assert.equal(model.body.error, "model_credentials_disabled");

    const context = await brokerGet(descriptor, "/v1/runtime/context");
    assert.equal(context.status, 200);
    assert.equal(context.body.runtime, "openclaw");
    assert.equal(context.body.teamName, "team-a");

    const storage = await brokerGet(descriptor, "/v1/credentials/storage");
    assert.equal(storage.status, 200);
    assert.equal(storage.body.provider, "oss");
    assert.equal(storage.body.bucket, "bucket");
  } finally {
    server.close();
    fs.rmSync(args.stateDir, { recursive: true, force: true });
  }
});
