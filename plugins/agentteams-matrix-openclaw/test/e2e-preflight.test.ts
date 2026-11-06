/**
 * E2E preflight regressions (#1470).
 *
 * `scripts/e2e-openclaw.sh --preflight-only` validates the Matrix
 * environment before anything starts. It used to read the membership from
 * `/rooms/{roomId}/members/{userId}` — a route that has never existed in
 * the client-server API and that Tuwunel answers with 404 M_UNRECOGNIZED —
 * so every run aborted in preflight with both users valid and joined.
 *
 * These tests drive the real script (`--preflight-only`, shared
 * `scripts/e2e_preflight.py`) against a fake homeserver that implements the
 * documented surface (whoami, member state, joined_members) and 404s the
 * bogus route, exactly like a real server would.
 */

import { describe, it } from "node:test";
import * as assert from "node:assert/strict";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { spawn } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const PLUGIN_ROOT = resolve(HERE, "..", "..");
const SCRIPT = join(PLUGIN_ROOT, "scripts", "e2e-openclaw.sh");

const ROOM_ID = "!e2e:example.org";
const AGENT = "@agent:example.org";
const SENDER = "@sender:example.org";

interface FakeServer {
  url: string;
  requests: string[];
  close(): Promise<void>;
}

type Behaviour = (req: IncomingMessage, res: ServerResponse, path: string) => boolean;

async function fakeHomeserver(behaviour: Behaviour): Promise<FakeServer> {
  const requests: string[] = [];
  const server: Server = createServer((req, res) => {
    const path = req.url ?? "/";
    requests.push(path);
    if (behaviour(req, res, path)) return;
    // Default surface: the documented endpoints only.
    if (path === "/_matrix/client/v3/account/whoami") {
      const token = (req.headers.authorization ?? "").replace("Bearer ", "");
      const user = token === "agent-token" ? AGENT : token === "sender-token" ? SENDER : "@other:example.org";
      respond(res, 200, { user_id: user });
      return;
    }
    const memberState = path.match(
      new RegExp(`^/_matrix/client/v3/rooms/([^/]+)/state/m\\.room\\.member/([^/]+)$`),
    );
    if (memberState) {
      const user = decodeURIComponent(memberState[2]);
      respond(res, 200, { membership: user === AGENT || user === SENDER ? "join" : "leave" });
      return;
    }
    if (path.match(/^\/_matrix\/client\/v3\/rooms\/[^/]+\/joined_members$/)) {
      respond(res, 200, { joined: { [AGENT]: {}, [SENDER]: {} } });
      return;
    }
    // Everything else — including the old /members/{user} route — is a 404,
    // exactly what Tuwunel answers for an unknown client route.
    respond(res, 404, { errcode: "M_UNRECOGNIZED", error: "Unrecognized request" });
  });
  await new Promise<void>((done) => server.listen(0, "127.0.0.1", () => done()));
  const address = server.address();
  if (address === null || typeof address === "string") throw new Error("no port");
  return {
    url: `http://127.0.0.1:${address.port}`,
    requests,
    close: () => new Promise<void>((done) => server.close(() => done())),
  };
}

function respond(res: ServerResponse, status: number, body: unknown): void {
  const payload = JSON.stringify(body);
  res.writeHead(status, { "content-type": "application/json" });
  res.end(payload);
}

/**
 * Run the preflight asynchronously: the fake homeserver lives in THIS
 * process, so a spawnSync would block the event loop and every request would
 * time out.
 */
function runPreflight(baseUrl: string): Promise<{ status: number | null; stdout: string; stderr: string }> {
  return new Promise((done, fail) => {
    const child = spawn("bash", [SCRIPT, "--preflight-only"], {
      env: {
        ...process.env,
        AGENTTEAMS_MATRIX_URL: baseUrl,
        E2E_MATRIX_USER: AGENT,
        E2E_MATRIX_TOKEN: "agent-token",
        E2E_SENDER_USER: SENDER,
        E2E_SENDER_TOKEN: "sender-token",
        E2E_ROOM_ID: ROOM_ID,
      },
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => {
      stdout += String(chunk);
    });
    child.stderr.on("data", (chunk) => {
      stderr += String(chunk);
    });
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      fail(new Error(`preflight did not finish in 60s; stderr=${stderr}`));
    }, 60_000);
    child.on("error", (err) => {
      clearTimeout(timer);
      fail(err);
    });
    child.on("close", (code) => {
      clearTimeout(timer);
      done({ status: code, stdout, stderr });
    });
  });
}

describe("e2e preflight", () => {
  it("passes against a spec-shaped homeserver and never calls /members/{user}", async () => {
    const server = await fakeHomeserver(() => false);
    try {
      const result = await runPreflight(server.url);
      assert.equal(result.status, 0, `stderr: ${result.stderr}`);
      assert.match(result.stdout, /preflight ok: both users valid and joined/);
      // The member state route was used, with percent-encoded segments.
      // (Python's quote(..., safe="") escapes '!' and ':' and '@' — note
      // that JS encodeURIComponent leaves '!' alone, so the expected string
      // is written literally rather than via encodeURIComponent.)
      assert.ok(
        server.requests.includes(
          "/_matrix/client/v3/rooms/%21e2e%3Aexample.org/state/m.room.member/%40agent%3Aexample.org",
        ),
        `requests: ${server.requests.join(", ")}`,
      );
      // The route that does not exist in the client-server API is never called.
      assert.equal(
        server.requests.some((path) => /\/members\/%40|\/members\/@/.test(path)),
        false,
        `bogus /members/{user} route was called: ${server.requests.join(", ")}`,
      );
    } finally {
      await server.close();
    }
  });

  it("falls back to joined_members when member state is unavailable", async () => {
    const server = await fakeHomeserver((_req, res, path) => {
      if (path.includes("/state/m.room.member/")) {
        respond(res, 404, { errcode: "M_UNRECOGNIZED", error: "state not available" });
        return true;
      }
      return false;
    });
    try {
      const result = await runPreflight(server.url);
      assert.equal(result.status, 0, `stderr: ${result.stderr}`);
      assert.ok(server.requests.some((path) => path.endsWith("/joined_members")));
    } finally {
      await server.close();
    }
  });

  it("fails with a clear message when a user is not joined", async () => {
    const server = await fakeHomeserver((_req, res, path) => {
      if (path.endsWith(`/state/m.room.member/${encodeURIComponent(AGENT)}`)) {
        respond(res, 200, { membership: "invite" });
        return true;
      }
      return false;
    });
    try {
      const result = await runPreflight(server.url);
      assert.equal(result.status, 2);
      assert.match(result.stderr, /is not joined/);
    } finally {
      await server.close();
    }
  });

  it("fails when a token belongs to another user", async () => {
    const server = await fakeHomeserver((_req, res, path) => {
      if (path === "/_matrix/client/v3/account/whoami") {
        respond(res, 200, { user_id: "@someone-else:example.org" });
        return true;
      }
      return false;
    });
    try {
      const result = await runPreflight(server.url);
      assert.equal(result.status, 2);
      assert.match(result.stderr, /!=/);
    } finally {
      await server.close();
    }
  });
});
