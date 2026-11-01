/**
 * Unit tests for the inbound dispatch wiring of the agentteams-matrix
 * OpenClaw channel plugin.
 *
 * Verified against the installed OpenClaw 2026.10.9 SDK (public subpaths
 * only): the entry injects the runtime via `setRuntime`, and every sidecar
 * `inbound` frame is dispatched through `runtime.channel.routing
 * .resolveAgentRoute` and `runtime.channel.inbound.run` (the core-managed
 * delivery overload) with an adapter whose `ingest` takes the raw frame and
 * whose `resolveTurn` takes (ingested, decision, extra). These tests cast a
 * fake to `PluginRuntime` and exercise that contract end to end at the unit
 * level: route/ctx/turn shapes, `delivery.deliver` reaching the sidecar as
 * `outbound {room_id, text, reply_to}`, the empty-text no-send rule, the
 * log-and-drop failure path (no requeue), the no-runtime fallback, and the
 * module-level `setRuntime`/`clearRuntime` switch on the default instance
 * (the latter over a real socket, via the in-process mock sidecar).
 *
 * Run with the package `test` script (compile with tsc, then `node --test`).
 * Node's built-in test runner + assert/strict only; no test framework.
 */

import { describe, it } from "node:test";
import * as assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import type { OpenClawConfig, PluginRuntime } from "openclaw/plugin-sdk/channel-core";
import {
  clearRuntime,
  createOpenClawMatrixPlugin,
  normalizeInboundFrame,
  openClawMatrixPlugin,
  setRuntime,
  type OpenClawMatrixInboundMessage,
  type OpenClawMatrixLogger,
  type ResolvedOpenClawMatrixAccount,
} from "../src/plugin.js";
import type { SidecarClient, SidecarInboundFrame } from "../src/sidecar-client.js";
import { startMockSidecar } from "./mock-sidecar.js";

const CFG: OpenClawConfig = {
  channels: {
    "agentteams-matrix": { socketPath: "/tmp/never-used.sock", agent: "test-agent" },
  },
} as OpenClawConfig;

const waitTick = (ms = 100): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

async function waitFor(cond: () => boolean, timeoutMs = 5_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!cond()) {
    if (Date.now() > deadline) throw new Error("waitFor timed out");
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
}

function makeFrame(over: Record<string, unknown> = {}): SidecarInboundFrame {
  return {
    type: "inbound",
    event_id: "$evt-1",
    room_id: "!room:example.org",
    sender: "@alice:example.org",
    text: "hello",
    media: [],
    mentions: [],
    ts: 1_761_000_000_000,
    ...over,
  } as SidecarInboundFrame;
}

/** In-memory stand-in for SidecarClient (no socket involved). */
class FakeSidecarClient extends EventEmitter {
  readonly sent: Record<string, unknown>[] = [];
  connectCalls = 0;
  closeCalls = 0;
  connected = true;

  connect(_path: string, _options: unknown): Promise<void> {
    this.connectCalls += 1;
    return Promise.resolve();
  }

  sendOutbound(request: Record<string, unknown>): Promise<{ event_id: string }> {
    this.sent.push({ ...request });
    return Promise.resolve({ event_id: `$sent-${this.sent.length}` });
  }

  close(): void {
    this.closeCalls += 1;
  }

  // The SidecarClient registration surface. The plugin attaches its frame
  // handlers through these methods, so the fake maps them onto the
  // EventEmitter it already extends (and `emit` drives them in the tests).
  onInbound(handler: (frame: SidecarInboundFrame) => void): this {
    this.on("inbound", handler);
    return this;
  }

  onTyping(handler: (frame: { room_id: string; state: string }) => void): this {
    this.on("typing", handler);
    return this;
  }

  onError(handler: (err: Error) => void): this {
    this.on("error", handler);
    return this;
  }

  onClose(handler: (err?: Error) => void): this {
    this.on("close", handler);
    return this;
  }
}

type FakeCall = { op: string; args?: unknown; params?: unknown };

/**
 * Fake `PluginRuntime` implementing the two seams the plugin uses
 * (`channel.routing.resolveAgentRoute`, `channel.inbound.{buildContext,run}`).
 * `run` simulates the core-managed delivery path: ingest(raw) ->
 * resolveTurn(ingested, decision, extra) -> delivery.deliver(payload).
 */
function makeRuntime(overrides: {
  agentId?: string;
  sessionKey?: string;
  runImpl?: (params: unknown) => Promise<unknown>;
  deliverPayloads?: { text?: string }[];
} = {}) {
  const agentId = overrides.agentId ?? "test-agent";
  const sessionKey = overrides.sessionKey ?? "agentteams-matrix:default:group:!room:example.org";
  const route = { agentId, dmScope: "per-peer", sessionKey };
  const calls: FakeCall[] = [];
  const ings: unknown[] = [];
  const turns: any[] = [];

  const runtime = {
    channel: {
      routing: {
        resolveAgentRoute(args: unknown) {
          calls.push({ op: "resolveAgentRoute", args });
          return route;
        },
      },
      inbound: {
        buildContext(payload: unknown) {
          calls.push({ op: "buildContext", args: payload });
          return { ...(payload as Record<string, unknown>), __built: true };
        },
        async run(params: any) {
          calls.push({ op: "run", params });
          if (overrides.runImpl) return overrides.runImpl(params);
          const ingested = params.adapter.ingest(params.raw);
          ings.push(ingested);
          const turn = params.adapter.resolveTurn(
            ingested,
            { kind: "message", canStartAgentTurn: true },
            {},
          );
          turns.push(turn);
          const payloads = overrides.deliverPayloads;
          const payload = payloads && payloads.length > 0 ? payloads.shift() : { text: "agent says hi" };
          await turn.delivery.deliver(payload);
          return { ok: true };
        },
      },
    },
  };
  return {
    runtime: runtime as unknown as PluginRuntime,
    route,
    calls,
    ings,
    turns,
  };
}

function makePlugin(
  client: FakeSidecarClient,
  runtime: PluginRuntime | null,
  hooks: {
    onInboundMessage?: (m: OpenClawMatrixInboundMessage) => void | Promise<void>;
    log?: OpenClawMatrixLogger;
  } = {},
) {
  return createOpenClawMatrixPlugin({
    createSidecarClient: () => client as unknown as SidecarClient,
    runtime: runtime ?? undefined,
    onInboundMessage: hooks.onInboundMessage,
    log: hooks.log,
  });
}

async function startAccount(
  plugin: ReturnType<typeof createOpenClawMatrixPlugin>,
  cfg: OpenClawConfig,
): Promise<ResolvedOpenClawMatrixAccount> {
  const account = plugin.config!.resolveAccount!(cfg);
  await plugin.gateway!.startAccount!({ account, cfg } as never);
  return account;
}

describe("agentteams-matrix inbound dispatch", () => {
  it("routes inbound frames through resolveAgentRoute + inbound.run and delivers replies to the sidecar", async () => {
    const client = new FakeSidecarClient();
    const { runtime, route, calls, ings, turns } = makeRuntime();
    const plugin = makePlugin(client, runtime);
    await startAccount(plugin, CFG);

    const frame = makeFrame();
    client.emit("inbound", frame);
    await waitTick();

    // 1. Routing: every v1 room is a group (the frame carries no
    //    directness field), single default account.
    const routeCall = calls.find((c) => c.op === "resolveAgentRoute");
    assert.ok(routeCall, "resolveAgentRoute must be called");
    assert.deepEqual(routeCall.args, {
      cfg: CFG,
      channel: "agentteams-matrix",
      accountId: "default",
      peer: { kind: "group", id: "!room:example.org" },
    });

    // 2. run() receives the core-managed delivery params (raw frame + adapter).
    const runCall = calls.find((c) => c.op === "run");
    assert.ok(runCall, "inbound.run must be called");
    const params = runCall.params as {
      channel: string;
      accountId: string;
      raw: unknown;
      adapter: { ingest: unknown; resolveTurn: unknown };
    };
    assert.equal(params.channel, "agentteams-matrix");
    assert.equal(params.accountId, "default");
    assert.equal(params.raw, frame);
    assert.equal(typeof params.adapter.ingest, "function");
    assert.equal(typeof params.adapter.resolveTurn, "function");

    // 3. ingest (one argument: the raw frame) maps onto the inbound identity.
    assert.equal(ings.length, 1);
    const ingested = ings[0] as Record<string, unknown>;
    assert.equal(ingested.id, "$evt-1");
    assert.equal(ingested.textForAgent, "hello");
    assert.equal(ingested.textForCommands, "hello");
    assert.equal(ingested.raw, frame);
    assert.equal(ingested.timestamp, 1_761_000_000_000);

    // 4. resolveTurn (three arguments) returns the turn with route, ctx and
    //    delivery wired back to the sidecar.
    assert.equal(turns.length, 1);
    const turn = turns[0];
    assert.deepEqual(turn.route, {
      agentId: route.agentId,
      dmScope: route.dmScope,
      sessionKey: route.sessionKey,
    });
    assert.equal(turn.cfg, CFG);
    assert.equal(turn.channel, "agentteams-matrix");
    assert.equal(turn.accountId, "default");

    const ctx = turn.ctxPayload as Record<string, any>;
    assert.equal(ctx.__built, true, "ctxPayload must come from buildContext");
    assert.equal(ctx.channel, "agentteams-matrix");
    assert.equal(ctx.accountId, "default");
    assert.equal(ctx.messageId, "$evt-1");
    assert.deepEqual(ctx.conversation, {
      kind: "group",
      id: "!room:example.org",
      label: "!room:example.org",
    });
    assert.deepEqual(ctx.route, {
      agentId: route.agentId,
      dmScope: route.dmScope,
      accountId: "default",
      routeSessionKey: route.sessionKey,
      dispatchSessionKey: route.sessionKey,
    });
    assert.deepEqual(ctx.reply, { to: "!room:example.org" });
    assert.equal(ctx.message.rawBody, "hello");
    assert.equal(ctx.extra.ThreadRoot, undefined);
    assert.deepEqual(ctx.extra.Mentions, []);

    // 5. delivery.deliver lands on the sidecar as an outbound reply.
    assert.equal(client.sent.length, 1);
    assert.equal(client.sent[0].room_id, "!room:example.org");
    assert.equal(client.sent[0].text, "agent says hi");
    assert.equal(client.sent[0].reply_to, "$evt-1");
  });

  it("sends nothing when the turn delivers an empty text", async () => {
    const client = new FakeSidecarClient();
    const { runtime, calls } = makeRuntime({
      deliverPayloads: [{ text: "" }, { text: undefined }],
    });
    const plugin = makePlugin(client, runtime);
    await startAccount(plugin, CFG);

    client.emit("inbound", makeFrame());
    await waitTick(50);
    client.emit("inbound", makeFrame({ event_id: "$evt-2" }));
    await waitTick();

    assert.equal(calls.filter((c) => c.op === "run").length, 2);
    assert.equal(client.sent.length, 0);
  });

  it("logs the event id when run() fails and keeps the connection (no requeue)", async () => {
    const client = new FakeSidecarClient();
    const errors: string[] = [];
    const { runtime, calls } = makeRuntime({
      runImpl: async () => {
        throw new Error("boom");
      },
    });
    const plugin = makePlugin(client, runtime, {
      log: { error: (m) => errors.push(m) },
    });
    await startAccount(plugin, CFG);

    client.emit("inbound", makeFrame());
    await waitTick();

    assert.equal(calls.filter((c) => c.op === "run").length, 1);
    assert.equal(errors.length, 1);
    assert.match(errors[0], /\$evt-1/);
    assert.match(errors[0], /!room:example\.org/);
    assert.match(errors[0], /boom/);
    // No requeue, no teardown: the sidecar delivered the event exactly once
    // and a retry from here would risk a duplicate reply.
    assert.equal(client.sent.length, 0);
    assert.equal(client.connected, true);
    assert.equal(client.closeCalls, 0);
  });

  it("falls back to onInboundMessage when no runtime is available", async () => {
    const client = new FakeSidecarClient();
    const received: OpenClawMatrixInboundMessage[] = [];
    const plugin = makePlugin(client, null, {
      onInboundMessage: (m) => {
        received.push(m);
      },
    });
    await startAccount(plugin, CFG);

    const frame = makeFrame();
    client.emit("inbound", frame);
    await waitTick();

    assert.equal(received.length, 1);
    assert.deepEqual(received[0], normalizeInboundFrame(frame, "default"));
    assert.equal(client.sent.length, 0);
  });

  it("module setRuntime routes the default instance through dispatch; clearRuntime restores the fallback", async () => {
    const mock = await startMockSidecar();
    const { runtime, calls } = makeRuntime();
    const cfg: OpenClawConfig = {
      channels: {
        "agentteams-matrix": { socketPath: mock.socketPath, agent: "test-agent" },
      },
    } as OpenClawConfig;

    setRuntime(runtime);
    try {
      const account = openClawMatrixPlugin.config!.resolveAccount!(cfg);
      await openClawMatrixPlugin.gateway!.startAccount!({ account, cfg } as never);

      const frame = makeFrame();
      mock.pushInbound(frame);
      await waitFor(() => mock.lastOutbound !== null);
      assert.equal(mock.lastOutbound?.type, "outbound");
      assert.equal(mock.lastOutbound?.room_id, "!room:example.org");
      assert.equal(mock.lastOutbound?.text, "agent says hi");
      assert.equal(mock.lastOutbound?.reply_to, "$evt-1");
      assert.ok(calls.some((c) => c.op === "resolveAgentRoute"));
      assert.ok(calls.some((c) => c.op === "run"));

      // With the runtime removed, the next frame takes the log-only
      // fallback: it must not produce an outbound.
      clearRuntime();
      const before = mock.lastOutbound;
      mock.pushInbound(makeFrame({ event_id: "$evt-fallback" }));
      await waitTick();
      assert.equal(mock.lastOutbound, before);
    } finally {
      clearRuntime();
      await mock.close();
    }
  });
});
