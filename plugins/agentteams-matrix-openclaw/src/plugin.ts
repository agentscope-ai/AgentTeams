/**
 * OpenClaw channel plugin for AgentTeams Matrix, bridged by the Python sidecar.
 *
 * The sidecar (`plugins/agentteams-matrix/core/sidecar.py`) owns ALL Matrix
 * logic: credentials, sync, send, media, history. This plugin is deliberately
 * thin: it maps OpenClaw's channel contract onto the sidecar's JSON-lines
 * protocol and contains no Matrix logic of its own.
 *
 * Wiring into OpenClaw (verified against the public plugin SDK, v2026.10.6):
 *   - `openclaw/plugin-sdk/channel-core` — `ChannelPlugin`, `OpenClawConfig`.
 *   - `openclaw/plugin-sdk/channel-config-schema` —
 *     `buildJsonChannelConfigSchema` (ZOD-free config schema with a runtime
 *     `safeParse`, so this package does not depend on zod).
 *
 * Lifecycle: the gateway calls `gateway.startAccount` / `stopAccount`, which
 * own the {@link SidecarClient} connection for the account. Inbound frames are
 * normalized to {@link OpenClawMatrixInboundMessage} and handed to the
 * `onInboundMessage` hook; outbound text goes through `outbound.sendText`.
 *
 * KNOWN LIMITATION (documented, not guessed): the remaining core integration
 * is dispatching inbound messages into OpenClaw's agent-turn pipeline
 * (`runChannelInboundEvent` from `openclaw/plugin-sdk/channel-inbound`). Its
 * `ChannelTurnAdapter.resolveTurn` requires core-internal dispatch context
 * (`ChannelTurnContext`, store paths, finalized msg context), which a thin
 * external plugin cannot synthesize. Until that binding lands, the default
 * inbound handler only logs, and the sidecar still delivers normally.
 */

import type { ChannelPlugin, OpenClawConfig } from "openclaw/plugin-sdk/channel-core";
import { buildJsonChannelConfigSchema } from "openclaw/plugin-sdk/channel-config-schema";
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

export type OpenClawMatrixPluginDeps = {
  /** Test seam: replace the SidecarClient implementation. */
  createSidecarClient?: () => SidecarClient;
  /**
   * Receives every normalized inbound message. The default handler logs only.
   *
   * This is the seam where the OpenClaw core agent-turn dispatch
   * (`runChannelInboundEvent` / `ChannelTurnAdapter.resolveTurn`) will be
   * bound; see the module docstring for why the scaffold cannot call it yet.
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

  const onInboundMessage = deps.onInboundMessage ?? ((message: OpenClawMatrixInboundMessage) => {
    // Documented seam: bind the OpenClaw agent-turn dispatch here.
    log.info?.(
      `inbound ${message.eventId} from ${message.sender} in ${message.roomId}: ${message.text}`,
    );
  });

  function clientFor(account: ResolvedOpenClawMatrixAccount): SidecarClient {
    let client = clients.get(account.accountId);
    if (!client) {
      client = createClient();
      clients.set(account.accountId, client);
    }
    return client;
  }

  function wireClient(client: SidecarClient, account: ResolvedOpenClawMatrixAccount): void {
    client.onInbound((frame: SidecarInboundFrame) => {
      void onInboundMessage(normalizeInboundFrame(frame, account.accountId));
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
        if (client.connected) return { connected: true };
        wireClient(client, account);
        await client.connect(account.socketPath, {
          agent: account.agent,
          protocol: account.protocolVersion,
        });
        log.info?.(`sidecar ${account.accountId} connected at ${account.socketPath}`);
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
