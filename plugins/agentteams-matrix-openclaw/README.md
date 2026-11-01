# agentteams-matrix-openclaw

OpenClaw channel plugin for AgentTeams Matrix. The **Python sidecar**
(`plugins/agentteams-matrix/core/sidecar.py`) owns all Matrix logic —
credentials, sync, send, media, history. This package is the thin TypeScript
counterpart: a `SidecarClient` for the sidecar's local-socket protocol plus an
OpenClaw `ChannelPlugin` that maps its contract onto that protocol. There is
deliberately **no Matrix logic in this package**.

## What is in here

| File | Role |
| --- | --- |
| `src/sidecar-client.ts` | Runtime-neutral sidecar protocol client (no OpenClaw dependency). |
| `src/plugin.ts` | `ChannelPlugin` factory wiring OpenClaw config/outbound/gateway onto the client. |
| `test/mock-sidecar.ts` | In-process mock sidecar (Unix socket) for tests. |
| `test/sidecar-client.test.ts` | `node:test` unit tests (no framework). |

## Sidecar protocol (version 1)

Transport: Unix socket (server mode `0600`), one JSON object per line,
UTF-8, `\n`-terminated, lines up to 1 MiB. One agent ⇒ one sidecar ⇒ at most
one connection; a second concurrent connection is rejected.

| Frame | Direction | Fields | Notes |
| --- | --- | --- | --- |
| `hello` | client → server | `protocol` (int), `agent` (non-empty str), `capabilities` (list) | First frame of the connection. |
| `ack` | server → client | `event_id` | Handshake ack has an **empty** `event_id`; outbound acks carry the sent event id (correlated FIFO). |
| `outbound` | client → server | `room_id`, `text`, `reply_to?`, `kind?` ∈ `final\|tool\|notice`, `media?` | One `ack` per outbound. |
| `inbound` | server → client | `event_id`, `room_id`, `sender`, `text`, `media[]`, `mentions[]`, `thread_root?`, `ts` (ms), `kind?` ∈ `text\|media\|edit\|redaction` | Events the agent should handle. |
| `typing` | server → client | `room_id`, `state` ∈ `start\|stop` | Typing indicator state. |
| `error` | server → client | `code`, `message` | `protocol_mismatch`, `connection_limit`, `handshake_timeout`, `connection_closed`, `invalid_json`, `invalid_frame`, `unknown_type`, `line_exceeds_limit`. Server sends an error frame and closes on handshake failure. |

Client behavior: 5 s default handshake timeout; `SidecarHandshakeError` on
timeout / mismatch / limit; FIFO outbound-ack correlation; unknown frame types
emit `error` and parsing continues; invalid JSON / oversized lines / post-handshake
error frames fail closed (connection torn down). **No native-Matrix fallback**,
no auto-reconnect — a new `connect()` is allowed after a terminal `close`.

## Wiring into OpenClaw

Verified against the public plugin SDK in the OpenClaw source
(**v2026.10.6**; the target environment is v2026.10.9 — see Limitations):

- `openclaw/plugin-sdk/channel-core` — `ChannelPlugin` (type),
  `OpenClawConfig` (type). `ChannelPlugin` requires
  `id` / `meta` / `capabilities` / `config` (`ChannelConfigAdapter`);
  `outbound` and `gateway` adapters are optional.
- `openclaw/plugin-sdk/channel-config-schema` — `buildJsonChannelConfigSchema`
  (ZOD-free; provides a runtime `safeParse`, so this package has no zod
  dependency).

The plugin registers as channel id `agentteams-matrix` with a single default
account. Config section:

```json5
{
  channels: {
    "agentteams-matrix": {
      socketPath: "/run/agentteams/matrix.sock", // required
      agent: "team-guide",                        // required
      protocolVersion: 1,                         // optional, default 1
      enabled: true                               // optional, default true
    }
  }
}
```

- **Lifecycle**: `gateway.startAccount` opens the sidecar connection
  (handshake) and wires `inbound` / `typing` / `error` / `close` events;
  `gateway.stopAccount` closes it.
- **Inbound**: sidecar `inbound` frames are normalized to
  `OpenClawMatrixInboundMessage` and handed to the `onInboundMessage` hook
  (default: log). This is the seam for OpenClaw agent-turn dispatch — see
  Limitations.
- **Outbound**: `outbound.sendText` sends an `outbound` frame and returns the
  sidecar's ack `event_id` as the delivery `messageId`. Fail-closed: if the
  sidecar is not connected, the send throws.

To register it as a plugin entry, wrap the exported plugin in an entry module,
e.g. `defineChannelPluginEntry` from `openclaw/plugin-sdk/channel-core`
(see `docs/plugins/sdk-channel-plugins.md` in the OpenClaw tree). That entry
file is intentionally not part of this scaffold.

## Developing

Node 24 LTS (24.22.x; the `openclaw` peer's install scripts require Node 24).
`openclaw` is a peer dependency (do not vendor it).

```sh
npm install
npm run typecheck   # tsc --noEmit
npm test            # tsc -p tsconfig.json && node --test dist/test/sidecar-client.test.js
```

The test script compiles first because `node --test` resolves the ESM `./x.js`
specifiers against the compiled layout (`dist/src` + `dist/test`).

## Limitations / open questions

1. **Version skew**: the plugin was written against the OpenClaw
   **v2026.10.6** source tree (local checkout `fe06181d`); the target runtime
   is **v2026.10.9**, which could not be fetched (shallow clone). Re-run
   `npm run typecheck` against the installed peer to confirm the SDK surface
   (`ChannelPlugin`, `ChannelGatewayAdapter`, `ChannelOutboundAdapter`,
   `buildJsonChannelConfigSchema`) is unchanged.
2. **Inbound dispatch not yet bound**: `runChannelInboundEvent`
   (`openclaw/plugin-sdk/channel-inbound`) needs a
   `ChannelTurnAdapter.resolveTurn` backed by core-internal dispatch context
   (`ChannelTurnContext`: store path, finalized msg context, inbound session
   recorder; `AssembledChannelTurn` additionally needs the runtime config and
   the buffered block dispatcher). A thin external plugin cannot synthesize
   that context, so inbound currently ends at the `onInboundMessage` hook.
   Binding real agent-turn dispatch is the next core-integration step and may
   need an OpenClaw-side helper that accepts a pre-resolved turn.
3. **Single default account**: `accounts.*` multi-account support is not
   implemented; one sidecar per agent matches the protocol's one-connection
   rule.
4. **Protocol gaps to settle with the maintainer**: reconnect/backoff
   semantics after a terminal close (client never auto-reconnects), typing
   direction (server→client only today), edit/react frames, media
   upload/download over the sidecar, and a health/ping frame.
5. **Gate status**: `npm install` / `npm run typecheck` / `npm test`
   (`tsc -p tsconfig.json && node --test`) were run on Node 24 LTS
   (v24.22.2) with a plain `npm install` (the `openclaw` peer's install
   scripts succeed on 24): typecheck clean, 8/8 tests pass. Re-run the
   three commands against the installed peer (v2026.10.9) before merging.
