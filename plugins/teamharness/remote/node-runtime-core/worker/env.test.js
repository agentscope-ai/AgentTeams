"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const {
  agentteamsEnv,
  buildManagedRuntimeEnv,
  cleanManagedEnv
} = require("./env");

function hasOwn(object, key) {
  return Object.prototype.hasOwnProperty.call(object, key);
}

test("cleanManagedEnv removes old product context variables", () => {
  const env = cleanManagedEnv({
    PATH: "/bin",
    HICLAW_WORKER_ROLE: "worker",
    HICLAW_MATRIX_URL: "https://matrix.example",
    AGENTTEAM_ROLE: "worker",
    AGENTTEAM_ACTOR: "worker-01",
    AGENTTEAMS_MEMBER_NAME: "old-member",
    TEAMHARNESS_TEAM_NAME: "old-team",
    TEAMHARNESS_MEMBER_NAME: "old-member",
    TEAMHARNESS_ROLE: "worker",
    TEAMHARNESS_NODE_BIN: "/usr/bin/node",
    TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR: "/tmp/broker.json"
  });

  assert.equal(env.PATH, "/bin");
  assert.equal(env.TEAMHARNESS_NODE_BIN, "/usr/bin/node");
  assert.equal(env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR, "/tmp/broker.json");
  assert.equal(hasOwn(env, "HICLAW_WORKER_ROLE"), false);
  assert.equal(hasOwn(env, "AGENTTEAM_ROLE"), false);
  assert.equal(hasOwn(env, "AGENTTEAMS_MEMBER_NAME"), false);
  assert.equal(hasOwn(env, "TEAMHARNESS_TEAM_NAME"), false);
  assert.equal(hasOwn(env, "TEAMHARNESS_MEMBER_NAME"), false);
  assert.equal(hasOwn(env, "TEAMHARNESS_ROLE"), false);
});

test("agentteamsEnv exposes only worker name context", () => {
  const env = agentteamsEnv({
    runtime: "openclaw",
    edge: { workerName: "worker-runtime", workerResourceName: "worker-cr" },
    runtimeState: {
      runtime: {
        team: { name: "team-runtime" },
        member: { name: "worker-cr", runtimeName: "worker-runtime", role: "worker" }
      }
    }
  });

  assert.deepEqual(env, {
    AGENTTEAMS_WORKER_NAME: "worker-runtime"
  });
  assert.equal(hasOwn(env, "AGENTTEAMS_REMOTE_MANAGED"), false);
  assert.equal(hasOwn(env, "AGENTTEAMS_RUNTIME"), false);
  assert.equal(hasOwn(env, "AGENTTEAMS_TEAM_NAME"), false);
  assert.equal(hasOwn(env, "AGENTTEAMS_ROLE"), false);
  assert.equal(hasOwn(env, "AGENTTEAMS_MEMBER_NAME"), false);
  assert.equal(hasOwn(env, "AGENTTEAMS_WORKER_RESOURCE_NAME"), false);
});

test("buildManagedRuntimeEnv combines cleaned env, broker, and agentteams context", () => {
  const env = buildManagedRuntimeEnv(
    {
      PATH: "/bin",
      HICLAW_WORKER_ROLE: "legacy",
      AGENTTEAM_ROLE: "legacy",
      AGENTTEAMS_ROLE: "legacy",
      TEAMHARNESS_NODE_BIN: "/node"
    },
    {
      runtime: "claude-code",
      edge: { workerName: "claude-local" },
      runtimeState: { runtime: { member: { role: "team_leader" } } },
      brokerDescriptor: "/state/credential-broker.json"
    }
  );

  assert.equal(env.PATH, "/bin");
  assert.equal(env.TEAMHARNESS_NODE_BIN, "/node");
  assert.equal(env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR, "/state/credential-broker.json");
  assert.equal(env.AGENTTEAMS_WORKER_NAME, "claude-local");
  assert.equal(hasOwn(env, "AGENTTEAMS_REMOTE_MANAGED"), false);
  assert.equal(hasOwn(env, "AGENTTEAMS_RUNTIME"), false);
  assert.equal(hasOwn(env, "AGENTTEAMS_ROLE"), false);
  assert.equal(hasOwn(env, "AGENTTEAMS_TEAM_NAME"), false);
  assert.equal(hasOwn(env, "HICLAW_WORKER_ROLE"), false);
  assert.equal(hasOwn(env, "AGENTTEAM_ROLE"), false);
});
