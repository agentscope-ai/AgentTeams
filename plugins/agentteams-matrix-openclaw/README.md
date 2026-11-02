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
| `src/index.ts` | Plugin entry: `defineChannelPluginEntry` + runtime injection (`setRuntime`). |
| `src/sidecar-client.ts` | Runtime-neutral sidecar protocol client (no OpenClaw dependency). |
| `src/plugin.ts` | `ChannelPlugin` factory wiring OpenClaw config/inbound dispatch/outbound/gateway onto the client. |
| `openclaw.plugin.json` | OpenClaw plugin manifest (identity, activation, channel config schema + UI hints). |
| `install.sh` / `upgrade.sh` / `rollback.sh` / `uninstall.sh` | Release lifecycle: self-contained release dirs, `active`/`previous` markers, channel config. Uninstall is fail-closed on an unreadable inventory. |
| `scripts/lib-lifecycle.sh` | Shared lifecycle helpers (state layout, channel config, three-valued inventory check). |
| `scripts/verify-sdk.mjs` | Runtime assertion that the installed OpenClaw SDK actually loads the built entry. |
| `test/*.test.ts` | `node:test` suites (no framework): protocol client, inbound dispatch, lifecycle config, member preflight, install order, e2e bootstrap. |

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

Verified against the public plugin SDK of the installed OpenClaw
**v2026.10.9** (typecheck against the real peer, `npm run verify:sdk`
runtime import assertions, and a native Matrix text roundtrip on a real
OpenClaw install — see Gate status):

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
  `OpenClawMatrixInboundMessage` and dispatched into the agent turn through
  the public runtime API: `runtime.channel.routing.resolveAgentRoute` →
  `runtime.channel.inbound.run` (core-managed delivery overload) with a turn
  adapter whose `resolveTurn` builds the turn context via
  `runtime.channel.inbound.buildContext` and wires `delivery.deliver` back to
  the sidecar (`kind: final`, `reply_to` = the inbound event id). Without an
  injected runtime (unit tests), frames fall back to the `onInboundMessage`
  hook (default: log).
- **Outbound**: `outbound.sendText` sends an `outbound` frame and returns the
  sidecar's ack `event_id` as the delivery `messageId`. Fail-closed: if the
  sidecar is not connected, the send throws.

The entry module is `src/index.ts`: it wraps the plugin in
`defineChannelPluginEntry` (`openclaw/plugin-sdk/channel-core`) and injects
the runtime through `setRuntime`. The manifest is `openclaw.plugin.json`
(channel id `agentteams-matrix`, activation on startup), and `package.json`
exposes the extension via `openclaw.extensions` / `runtimeExtensions` with
`main: dist/src/index.js`.

## Developing

Node 24 LTS (24.22.x; the `openclaw` peer's install scripts require Node 24).
`openclaw` is a peer dependency (do not vendor it).

```sh
npm install
npm run typecheck   # tsc --noEmit
npm test            # tsc -p tsconfig.json && node --test (6 suites)
npm run verify:sdk  # runtime import assertions against the installed SDK
```

The test script compiles first because `node --test` resolves the ESM `./x.js`
specifiers against the compiled layout (`dist/src` + `dist/test`).

## Limitations / open questions

1. **Single default account**: `accounts.*` multi-account support is not
   implemented; one sidecar per agent matches the protocol's one-connection
   rule.
2. **DM vs group**: the sidecar `inbound` frame carries no directness field
   (no `is_direct` / `members`; protocol version 1), so v1 routes every room
   as a group (`peer.kind = "group"`). DM semantics need a core protocol
   field.
3. **Protocol gaps to settle with the maintainer**: reconnect/backoff
   semantics after a terminal close (client never auto-reconnects), typing
   direction (server→client only today), edit/react frames, media
   upload/download over the sidecar, and a health/ping frame.
4. **Gate status**: `npm install` / `npm run typecheck` / `npm test`
   (6 `node:test` suites) / `npm run verify:sdk` run green on Node 24 LTS
   (v24.22.2) with the installed **OpenClaw v2026.10.9** peer: typecheck
   clean against the real peer, `verify-sdk` runtime import assertions pass,
   and a native Matrix text roundtrip (inbound message → agent turn → routed
   reply through the sidecar) was verified twice on a real OpenClaw +
   Synapse install (`scripts/e2e-openclaw.sh`).
