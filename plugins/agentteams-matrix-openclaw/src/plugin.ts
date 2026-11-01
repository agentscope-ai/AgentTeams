/**
 * OpenClaw channel plugin for AgentTeams Matrix, bridged by the Python sidecar.
 *
 * The sidecar (`plugins/agentteams-matrix/core/sidecar.py`) owns ALL Matrix
 * logic: credentials, sync, send, media, history. This plugin is deliberately
 * thin: it maps OpenClaw's channel contract onto the sidecar's JSON-lines
 * protocol and contains no Matrix logic of its own.
 *
 * Inbound dispatch (verified against the installed OpenClaw 2026.10.9 SDK):
 * the entry (`src/index.ts`) injects the runtime via {@link setRuntime}
 * (the public `PluginRuntime` from `openclaw/plugin-sdk/channel-core`), and
 * every sidecar `inbound` frame is dispatched through
 *   1. `runtime.channel.routing.resolveAgentRoute` — agent + session routing,
 *   2. `runtime.channel.inbound.run` (the core-managed delivery overload,
 *      typed as `ChannelInboundEventRunnerParams` from
 *      `openclaw/plugin-sdk/channel-inbound`) with a turn adapter whose
 *      `resolveTurn` builds the turn context via
 *      `runtime.channel.inbound.buildContext` and wires `delivery.deliver`
 *      back to the sidecar (`sendOutbound`, kind `final`, `reply_to` = the
 *      inbound event id),
 *   3. on `run` failure: log with the event id and drop. There is no
 *      requeue — the sidecar delivers each event exactly once, and
 *      re-delivery from here would risk a double reply.
 * DM vs group: the sidecar `inbound` frame carries no directness field
 * (no `is_direct` / `members`; protocol version 1), so v1 routes every room
 * as a group (`peer.kind = "group"`). DM semantics need a core protocol
 * field.
 *
 * Without an injected runtime (unit tests, or an entry that never ran
 * `setRuntime`), inbound frames fall back to the `onInboundMessage` hook
 * (default: log only).
 *
 * Wiring into OpenClaw (verified against the public plugin SDK):
 *   - `openclaw/plugin-sdk/channel-core` — `ChannelPlugin`, `OpenClawConfig`,
 *     `PluginRuntime`, `defineChannelPluginEntry`.
 *   - `openclaw/plugin-sdk/channel-config-schema` —
 *     `buildJsonChannelConfigSchema` (ZOD-free config schema with a runtime
 *     `safeParse`, so this package does not depend on zod).
 *   - `openclaw/plugin-sdk/channel-inbound` — `ChannelInboundEventRunnerParams`
 *     (type-only import; pins the core-managed delivery overload of
 *     `runtime.channel.inbound.run`).
 *
 * Lifecycle: the gateway calls `gateway.startAccount` / `stopAccount`, which
 * own the {@link SidecarClient} connection for the account. Outbound text
 * goes through `outbound.sendText` (channel-level) or the turn's
 * `delivery.deliver` (dispatch-level); both land on the sidecar socket.
 */

import type {
  ChannelPlugin,
  OpenClawConfig,
  PluginRuntime,
} from "openclaw/plugin-sdk/channel-core";
import { buildJsonChannelConfigSchema } from "openclaw/plugin-sdk/channel-config-schema";
import type { ChannelInboundEventRunnerParams } from "openclaw/plugin-sdk/channel-inbound";
import {
  PROTOCOL_VERSION,
  SidecarClient,
  type SidecarInboundFrame,
  type SidecarMediaRef,
  type TypingState,
} from "./sidecar-client.js";

/** OpenClaw channel id owned by this plugin. */
export const CHANNEL_ID = "agentteams-matrix";

const DEFAULT_ACCOUNT_ID = "default";

/**
 * The OpenClaw runtime injected by the entry's `setRuntime`. This is the
 * public `PluginRuntime` from `openclaw/plugin-sdk/channel-core`; the alias
 * keeps the plugin's public API stable across SDK versions.
 */
export type OpenClawMatrixRuntime = PluginRuntime;

/** Shape of `channels.agentteams-matrix` in the OpenClaw config. */
export type OpenClawMatrixChannelConfig = {
  enabled?: boolean;
  /** Path of the sidecar's Unix socket (e.g. `/run/agentteams/matrix.sock`). */
  socketPath: string;
  /** Agent name advertised in the sidecar `hello` handshake. */
  agent: string;
  /** Sidecar protocol version to advertise. Defaults to 1. */
  protocolVersion?: number;
};

/** Account as resolved from config (single default account for now). */
export type ResolvedOpenClawMatrixAccount = {
  accountId: string;
  enabled: boolean;
  /** True when socketPath and agent are both present. */
  configured: boolean;
  agent: string;
  socketPath: string;
  protocolVersion: number;
};

/** Normalized inbound Matrix message, derived from a sidecar `inbound` frame. */
export type OpenClawMatrixInboundMessage = {
  accountId: string;
  eventId: string;
  roomId: string;
  sender: string;
  text: string;
  /** `text` | `media` | `edit` | `redaction` (sidecar `kind`). */
  kind?: string;
  media: SidecarMediaRef[];
  mentions: string[];
  threadRoot: string | null;
  /** Unix timestamp in milliseconds. */
  timestamp: number;
};

export type OpenClawMatrixTypingEvent = {
  accountId: string;
  roomId: string;
  state: TypingState;
};

export type OpenClawMatrixLogger = {
  info?: (message: string) => void;
  warn?: (message: string) => void;
  error?: (message: string) => void;
};

let moduleRuntime: OpenClawMatrixRuntime | null = null;

/**
 * Inject the OpenClaw runtime (called by the entry's `setRuntime` in
 * `src/index.ts`). The default instance then dispatches inbound frames
 * through the real agent-turn pipeline instead of the log-only fallback.
 */
export function setRuntime(runtime: OpenClawMatrixRuntime): void {
  moduleRuntime = runtime;
}

/** Remove the injected runtime (test seam for the module-level state). */
export function clearRuntime(): void {
  moduleRuntime = null;
}

export type OpenClawMatrixPluginDeps = {
  /** Test seam: replace the SidecarClient implementation. */
  createSidecarClient?: () => SidecarClient;
  /**
   * Override the runtime used for inbound dispatch. When omitted, the
   * runtime injected via {@link setRuntime} is used; when neither is
   * available, the `onInboundMessage` fallback applies.
   */
  runtime?: OpenClawMatrixRuntime;
  /**
   * Receives every normalized inbound message when NO runtime is available
   * (no `deps.runtime` and no {@link setRuntime} injection — e.g. unit
   * tests without a gateway). The default handler logs only.
   */
  onInboundMessage?: (message: OpenClawMatrixInboundMessage) => void | Promise<void>;
  /** Receives sidecar `typing` frames. No default behavior. */
  onTyping?: (event: OpenClawMatrixTypingEvent) => void;
  /** Logger sink; defaults to console. */
  log?: OpenClawMatrixLogger;
};

function readSection(cfg: OpenClawConfig): Record<string, unknown> | null {
  const section = cfg.channels?.[CHANNEL_ID];
  if (section && typeof section === "object" && !Array.isArray(section)) {
    return section as Record<string, unknown>;
  }
  return null;
}

function resolveAccount(
  cfg: OpenClawConfig,
  accountId?: string | null,
): ResolvedOpenClawMatrixAccount {
  const section = readSection(cfg);
  const socketPath = typeof section?.socketPath === "string" ? section.socketPath : "";
  const agent = typeof section?.agent === "string" ? section.agent : "";
  const protocolVersion =
    typeof section?.protocolVersion === "number" ? section.protocolVersion : PROTOCOL_VERSION;
  const enabled = typeof section?.enabled === "boolean" ? section.enabled : true;
  return {
    accountId: accountId ?? DEFAULT_ACCOUNT_ID,
    enabled,
    configured: socketPath.length > 0 && agent.length > 0,
    agent,
    socketPath,
    protocolVersion,
  };
}

/** Map a sidecar `inbound` frame onto the plugin's normalized inbound message. */
export function normalizeInboundFrame(
  frame: SidecarInboundFrame,
  accountId: string,
): OpenClawMatrixInboundMessage {
  return {
    accountId,
    eventId: frame.event_id,
    roomId: frame.room_id,
    sender: frame.sender,
    text: frame.text,
    ...(frame.kind !== undefined ? { kind: frame.kind } : {}),
    media: frame.media ?? [],
    mentions: frame.mentions ?? [],
    threadRoot: frame.thread_root ?? null,
    timestamp: frame.ts,
  };
}

/**
 * Build the channel plugin. Call once and register the result (e.g. through
 * `defineChannelPluginEntry` in an entry module); the factory is pure and the
 * returned object is a plain `ChannelPlugin` literal.
 */
export function createOpenClawMatrixPlugin(
  deps: OpenClawMatrixPluginDeps = {},
): ChannelPlugin<ResolvedOpenClawMatrixAccount> {
  const log: OpenClawMatrixLogger = deps.log ?? {
    info: (m) => console.log(`[agentteams-matrix] ${m}`),
    warn: (m) => console.warn(`[agentteams-matrix] ${m}`),
    error: (m) => console.error(`[agentteams-matrix] ${m}`),
  };
  const createClient = deps.createSidecarClient ?? (() => new SidecarClient());
  const clients = new Map<string, SidecarClient>();
  // One wiring pass per client instance (see wireClientOnce).
  const wiredClients = new WeakSet<SidecarClient>();

  const onInboundMessage = deps.onInboundMessage ?? ((message: OpenClawMatrixInboundMessage) => {
    // No-runtime fallback (see the module docstring).
    log.info?.(
      `inbound ${message.eventId} from ${message.sender} in ${message.roomId}: ${message.text}`,
    );
  });

  function activeRuntime(): OpenClawMatrixRuntime | null {
    return deps.runtime ?? moduleRuntime;
  }

  function clientFor(account: ResolvedOpenClawMatrixAccount): SidecarClient {
    let client = clients.get(account.accountId);
    if (!client) {
      client = createClient();
      clients.set(account.accountId, client);
    }
    return client;
  }

  /**
   * Dispatch one sidecar `inbound` frame into the OpenClaw agent-turn
   * pipeline. Never throws: a failed dispatch is logged with the event id
   * and dropped (no requeue — dedupe is the sidecar's job, and a retry here
   * could produce a duplicate reply).
   */
  async function dispatchInbound(
    frame: SidecarInboundFrame,
    account: ResolvedOpenClawMatrixAccount,
    client: SidecarClient,
    cfg: OpenClawConfig,
  ): Promise<void> {
    const rt = activeRuntime();
    if (rt === null) {
      await onInboundMessage(normalizeInboundFrame(frame, account.accountId));
      return;
    }

    // DM/group: the sidecar `inbound` frame carries no directness field, so
    // v1 routes every room as a group (see the module docstring).
    const route = rt.channel.routing.resolveAgentRoute({
      cfg,
      channel: CHANNEL_ID,
      accountId: account.accountId,
      peer: { kind: "group", id: frame.room_id },
    });

    const params: ChannelInboundEventRunnerParams<SidecarInboundFrame> = {
      channel: CHANNEL_ID,
      accountId: account.accountId,
      raw: frame,
      adapter: {
        ingest: (raw) => ({
          id: raw.event_id,
          timestamp: raw.ts,
          rawText: raw.text,
          textForAgent: raw.text,
          textForCommands: raw.text,
          raw,
        }),
        resolveTurn: (input) => {
          const ctxPayload = rt.channel.inbound.buildContext({
            channel: CHANNEL_ID,
            accountId: account.accountId,
            messageId: input.id,
            timestamp: input.timestamp,
            from: `${CHANNEL_ID}:${frame.sender}`,
            sender: { id: frame.sender, name: frame.sender },
            conversation: { kind: "group", id: frame.room_id, label: frame.room_id },
            route: {
              agentId: route.agentId,
              dmScope: route.dmScope,
              accountId: account.accountId,
              routeSessionKey: route.sessionKey,
              dispatchSessionKey: route.sessionKey,
            },
            reply: { to: frame.room_id },
            message: {
              rawBody: input.rawText,
              commandBody: input.textForCommands,
              bodyForAgent: input.textForAgent,
            },
            extra: {
              ThreadRoot: frame.thread_root ?? undefined,
              Mentions: frame.mentions,
            },
            channelIngress: "unsupported",
          });
          return {
            cfg,
            channel: CHANNEL_ID,
            accountId: account.accountId,
            route: {
              agentId: route.agentId,
              dmScope: route.dmScope,
              sessionKey: route.sessionKey,
            },
            ctxPayload,
            delivery: {
              durable: () => ({ to: frame.room_id }),
              deliver: async (payload) => {
                const text = payload.text;
                if (!text) return;
                await client.sendOutbound({
                  room_id: frame.room_id,
                  text,
                  reply_to: frame.event_id,
                });
              },
            },
          };
        },
      },
    };

    try {
      await rt.channel.inbound.run(params);
    } catch (err) {
      log.error?.(
        `inbound dispatch failed for ${frame.event_id} in ${frame.room_id} ` +
          `(account ${account.accountId}): ${err instanceof Error ? err.message : String(err)}`,
      );
    }
  }

  /**
   * Attach the frame handlers exactly once per client instance. The
   * SidecarClient registration helpers append to an EventEmitter, so a
   * second `wireClient` on the same instance would dispatch every inbound
   * frame twice (two agent turns, two replies).
   */
  function wireClientOnce(
    client: SidecarClient,
    account: ResolvedOpenClawMatrixAccount,
    cfg: OpenClawConfig,
  ): void {
    if (wiredClients.has(client)) return;
    wiredClients.add(client);
    wireClient(client, account, cfg);
  }

  function wireClient(
    client: SidecarClient,
    account: ResolvedOpenClawMatrixAccount,
    cfg: OpenClawConfig,
  ): void {
    client.onInbound((frame: SidecarInboundFrame) => {
      void dispatchInbound(frame, account, client, cfg);
    });
    client.onTyping((frame) => {
      deps.onTyping?.({
        accountId: account.accountId,
        roomId: frame.room_id,
        state: frame.state,
      });
    });
    client.onError((err: Error) => {
      log.error?.(`sidecar ${account.accountId}: ${err.message}`);
    });
    client.onClose((err?: Error) => {
      if (err) log.warn?.(`sidecar ${account.accountId} closed: ${err.message}`);
      clients.delete(account.accountId);
    });
  }

  return {
    id: CHANNEL_ID,
    meta: {
      id: CHANNEL_ID,
      label: "AgentTeams Matrix",
      selectionLabel: "AgentTeams Matrix (sidecar)",
      docsPath: "/plugins/agentteams-matrix-openclaw",
      blurb: "Matrix channel bridged by the AgentTeams Python sidecar.",
    },
    capabilities: {
      chatTypes: ["direct", "group"],
      media: true,
    },
    configSchema: buildJsonChannelConfigSchema(
      {
        type: "object",
        additionalProperties: false,
        properties: {
          enabled: { type: "boolean" },
          socketPath: { type: "string", minLength: 1 },
          agent: { type: "string", minLength: 1 },
          protocolVersion: { type: "integer", minimum: 1, default: PROTOCOL_VERSION },
        },
        required: ["socketPath", "agent"],
      },
      {
        uiHints: {
          socketPath: {
            label: "Sidecar socket",
            help: "Path of the AgentTeams Matrix sidecar Unix socket.",
            placeholder: "/run/agentteams/matrix.sock",
          },
          agent: {
            label: "Agent name",
            help: "Agent name advertised in the sidecar hello handshake.",
          },
          protocolVersion: {
            label: "Protocol version",
            help: "Sidecar protocol version (1).",
            advanced: true,
          },
        },
      },
    ),
    config: {
      listAccountIds: (cfg: OpenClawConfig) => (readSection(cfg) ? [DEFAULT_ACCOUNT_ID] : []),
      resolveAccount: (cfg: OpenClawConfig, accountId?: string | null) =>
        resolveAccount(cfg, accountId),
      isConfigured: (account: ResolvedOpenClawMatrixAccount) => account.configured,
      // Contractually only queried when the account is NOT configured; the
      // configured path returns an empty string because the return type is
      // `string` (not `string | undefined`).
      unconfiguredReason: (account: ResolvedOpenClawMatrixAccount) =>
        account.configured
          ? ""
          : "channels.agentteams-matrix requires socketPath and agent",
    },
    outbound: {
      deliveryMode: "direct",
      sendText: async (ctx) => {
        const account = resolveAccount(ctx.cfg, ctx.accountId ?? null);
        if (!account.configured) {
          throw new Error("agentteams-matrix is not configured (socketPath and agent required)");
        }
        const client = clientFor(account);
        // Fail closed: there is deliberately no native-Matrix fallback.
        if (!client.connected) {
          throw new Error(
            `agentteams-matrix sidecar is not connected (account ${account.accountId})`,
          );
        }
        const ack = await client.sendOutbound({
          room_id: ctx.to,
          text: ctx.text,
          ...(ctx.replyToId ? { reply_to: ctx.replyToId } : {}),
        });
        return { channel: CHANNEL_ID, messageId: ack.event_id };
      },
    },
    gateway: {
      startAccount: async (ctx) => {
        const account = ctx.account;
        if (!account.configured) {
          throw new Error(
            "agentteams-matrix is not configured (socketPath and agent required)",
          );
        }
        const client = clientFor(account);
        // Wire first, then connect: a client that is already connected
        // (a restarted account reusing the cached instance, or a test fake)
        // still needs its inbound/typing handlers attached. `wireClientOnce`
        // is idempotent per client instance, so a repeated startAccount never
        // double-dispatches an inbound frame.
        wireClientOnce(client, account, ctx.cfg);
        if (!client.connected) {
          await client.connect(account.socketPath, {
            agent: account.agent,
            protocol: account.protocolVersion,
          });
          log.info?.(`sidecar ${account.accountId} connected at ${account.socketPath}`);
        }
        // Keep the account running until the gateway aborts it. A resolved
        // `startAccount` is treated as "channel exited" and auto-restarted
        // (the Pi run logged `channel exited without an error` every few
        // seconds): each restart tore the sidecar wiring down between the
        // agent turn and its delivery, and the reply never reached the room.
        await new Promise<void>((resolve) => {
          const signal = ctx.abortSignal as AbortSignal | undefined;
          if (signal?.aborted) {
            resolve();
            return;
          }
          signal?.addEventListener("abort", () => resolve(), { once: true });
          client.onClose(() => resolve());
        });
        return { connected: true };
      },
      stopAccount: async (ctx) => {
        const client = clients.get(ctx.accountId);
        clients.delete(ctx.accountId);
        client?.close();
      },
    },
  };
}

/** Ready-to-register default instance (uses the real SidecarClient). */
export const openClawMatrixPlugin: ChannelPlugin<ResolvedOpenClawMatrixAccount> =
  createOpenClawMatrixPlugin();
