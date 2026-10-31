/**
 * Unit tests for SidecarClient against the in-process mock sidecar.
 *
 * Run with the package `test` script (compile with tsc, then `node --test`).
 * Node's built-in test runner + assert/strict only; no test framework.
 */

import { describe, it } from "node:test";
import * as assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import {
  SidecarClient,
  SidecarHandshakeError,
  SidecarNotConnectedError,
  SidecarProtocolError,
  type SidecarInboundFrame,
} from "../src/sidecar-client.js";
import { startMockSidecar } from "./mock-sidecar.js";

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function waitEvent<T>(emitter: EventEmitter, event: string): Promise<T> {
  return new Promise((resolve) => {
    emitter.once(event, (arg) => resolve(arg as T));
  });
}

const inboundFixture: Record<string, unknown> = {
  event_id: "$evt-1",
  room_id: "!room:example.org",
  sender: "@alice:example.org",
  text: "hello from matrix",
  media: [],
  mentions: [],
  ts: 1_761_000_000_000,
};

describe("SidecarClient", () => {
  it("handshakes, sends outbound, receives inbound, closes", async () => {
    const mock = await startMockSidecar();
    const client = new SidecarClient();
    try {
      const ready = waitEvent<void>(client, "ready");
      await client.connect(mock.socketPath, { agent: "test-agent" });
      await ready;
      assert.equal(client.connected, true);

      const ack = await client.sendOutbound({ room_id: "!room:example.org", text: "hi" });
      assert.equal(typeof ack.event_id, "string");
      assert.ok(ack.event_id.length > 0);
      assert.equal(mock.lastOutbound?.type, "outbound");
      assert.equal(mock.lastOutbound?.room_id, "!room:example.org");
      assert.equal(mock.lastOutbound?.text, "hi");

      const inboundPromise = waitEvent<SidecarInboundFrame>(client, "inbound");
      mock.pushInbound(inboundFixture);
      const inbound = await inboundPromise;
      assert.equal(inbound.event_id, "$evt-1");
      assert.equal(inbound.room_id, "!room:example.org");
      assert.equal(inbound.text, "hello from matrix");
      assert.deepEqual(inbound.media, []);
      assert.deepEqual(inbound.mentions, []);

      const closed = new Promise<void>((resolve) => client.onClose(() => resolve()));
      client.close();
      client.close(); // idempotent
      await closed;
      assert.equal(client.connected, false);
    } finally {
      client.close();
      await mock.close();
    }
  });

  it("fails the handshake on protocol mismatch", async () => {
    const mock = await startMockSidecar({ protocol: 2 });
    const client = new SidecarClient();
    const errors: SidecarHandshakeError[] = [];
    client.onError((err) => errors.push(err as SidecarHandshakeError));
    try {
      await assert.rejects(
        client.connect(mock.socketPath, { agent: "test-agent" }),
        (err: unknown) => err instanceof SidecarHandshakeError && err.code === "protocol_mismatch",
      );
      assert.equal(client.connected, false);
      assert.equal(errors.length, 1);
      assert.equal(errors[0].code, "protocol_mismatch");
    } finally {
      client.close();
      await mock.close();
    }
  });

  it("times out a silent handshake", async () => {
    const mock = await startMockSidecar({ neverAck: true });
    const client = new SidecarClient();
    client.onError(() => {});
    try {
      await assert.rejects(
        client.connect(mock.socketPath, { agent: "test-agent", handshakeTimeoutMs: 200 }),
        (err: unknown) =>
          err instanceof SidecarHandshakeError && err.code === "handshake_timeout",
      );
      assert.equal(client.connected, false);
    } finally {
      client.close();
      await mock.close();
    }
  });

  it("emits an error for unknown frame types and keeps parsing", async () => {
    const mock = await startMockSidecar();
    const client = new SidecarClient();
    try {
      await client.connect(mock.socketPath, { agent: "test-agent" });

      const errorPromise = waitEvent<Error>(client, "error");
      mock.writeRaw(`${JSON.stringify({ type: "mystery", foo: 1 })}\n`);
      const err = await errorPromise;
      assert.ok(err instanceof SidecarProtocolError);
      assert.equal(err.code, "unknown_type");
      assert.equal(client.connected, true); // connection survives

      // Parsing continues: a following valid frame is still delivered.
      const inboundPromise = waitEvent<SidecarInboundFrame>(client, "inbound");
      mock.pushInbound(inboundFixture);
      const inbound = await inboundPromise;
      assert.equal(inbound.event_id, "$evt-1");
    } finally {
      client.close();
      await mock.close();
    }
  });

  it("emits exactly one terminal close and enforces the single-connection invariant", async () => {
    const mock = await startMockSidecar();
    const client = new SidecarClient();
    try {
      await client.connect(mock.socketPath, { agent: "test-agent" });

      assert.throws(
        () => client.connect(mock.socketPath, { agent: "test-agent" }),
        /already active/,
      );

      let closeCount = 0;
      client.onClose(() => {
        closeCount += 1;
      });
      client.close();
      client.close();
      await sleep(50);
      assert.equal(closeCount, 1);
    } finally {
      client.close();
      await mock.close();
    }
  });

  it("rejects sendOutbound before the handshake completes", async () => {
    const client = new SidecarClient();
    await assert.rejects(
      client.sendOutbound({ room_id: "!room:example.org", text: "hi" }),
      (err: unknown) => err instanceof SidecarNotConnectedError,
    );
  });

  it("rejects a concurrent second connection with connection_limit", async () => {
    const mock = await startMockSidecar();
    const first = new SidecarClient();
    const second = new SidecarClient();
    try {
      await first.connect(mock.socketPath, { agent: "agent-a" });
      await assert.rejects(
        second.connect(mock.socketPath, { agent: "agent-b" }),
        (err: unknown) =>
          // connection_limit is the protocol reason. connection_closed is
          // also valid: the server's reject path (sendall + close) can race
          // the client's hello write — an EPIPE on that write hard-destroys
          // the socket (Node runtime, no opt-out) and the queued error frame
          // is dropped, so the rejection surfaces as a bare close. Either
          // way the second connection must fail closed.
          err instanceof SidecarHandshakeError &&
          (err.code === "connection_limit" || err.code === "connection_closed"),
      );
      assert.equal(mock.connectionCount, 2);
    } finally {
      first.close();
      second.close();
      await mock.close();
    }
  });

  it("buffers partial lines until the newline arrives", async () => {
    const mock = await startMockSidecar();
    const client = new SidecarClient();
    try {
      await client.connect(mock.socketPath, { agent: "test-agent" });

      // `type` is part of the wire protocol — writeRaw sends the bytes as
      // they would appear on the socket, so the frame must carry it (the
      // client rejects any frame without a string `type` as invalid_frame).
      const line = `${JSON.stringify({ type: "inbound", ...inboundFixture, event_id: "$evt-split" })}\n`;
      const half = Math.floor(line.length / 2);
      const inboundPromise = waitEvent<SidecarInboundFrame>(client, "inbound");
      mock.writeRaw(line.slice(0, half));
      await sleep(80); // first chunk alone must not produce a frame
      mock.writeRaw(line.slice(half));
      const inbound = await inboundPromise;
      assert.equal(inbound.event_id, "$evt-split");
      assert.equal(inbound.text, "hello from matrix");
    } finally {
      client.close();
      await mock.close();
    }
  });
});
