/**
 * SidecarClient — TypeScript client for the AgentTeams Matrix sidecar.
 *
 * The sidecar (Python, `plugins/agentteams-matrix/core/sidecar.py`) owns all
 * Matrix logic. This client is the thin, runtime-neutral transport used by the
 * OpenClaw channel plugin (`./plugin.ts`) to talk to the sidecar over a local
 * Unix socket.
 *
 * Protocol (authoritative: the sidecar module docstring, protocol version 1):
 *   - One JSON object per line, UTF-8, `\n`-terminated.
 *   - Frames: hello / ack / inbound / outbound / typing / error.
 *   - Handshake: client sends `hello`, server replies `ack` with an EMPTY
 *     `event_id`. Any other first response (an `error` frame, a close, or a
 *     timeout) is a handshake failure.
 *   - Fail-closed: on a protocol mismatch or bad frame the server closes the
 *     connection and the client surfaces the failure. There is deliberately NO
 *     native-Matrix fallback.
 *   - One agent => one sidecar => at most one connection; a second concurrent
 *     connection is rejected by the server (`connection_limit`).
 *
 * This module has no Matrix logic and no dependency on OpenClaw.
 */

import { EventEmitter } from "node:events";
import * as net from "node:net";

/** Sidecar protocol version implemented by this client. */
export const PROTOCOL_VERSION = 1;

/** Default handshake timeout in milliseconds. */
export const DEFAULT_HANDSHAKE_TIMEOUT_MS = 5_000;

/** Maximum accepted line size in bytes (1 MiB), mirroring the server. */
export const MAX_LINE_BYTES = 1024 * 1024;

export const INBOUND_KINDS = ["text", "media", "edit", "redaction"] as const;
export type InboundKind = (typeof INBOUND_KINDS)[number];

export const OUTBOUND_KINDS = ["final", "tool", "notice"] as const;
export type OutboundKind = (typeof OUTBOUND_KINDS)[number];

export const TYPING_STATES = ["start", "stop"] as const;
export type TypingState = (typeof TYPING_STATES)[number];

/** A Matrix media reference as carried inside inbound/outbound frames. */
export type SidecarMediaRef = {
  mxc_uri: string;
  mime_type?: string;
  file_name?: string;
  size?: number;
};

/** `inbound` frame: a Matrix event the sidecar wants the agent to handle. */
export type SidecarInboundFrame = {
  type: "inbound";
  event_id: string;
  room_id: string;
  sender: string;
  text: string;
  media: SidecarMediaRef[];
  mentions: string[];
  thread_root?: string | null;
  /** Unix timestamp in milliseconds. */
  ts: number;
  kind?: InboundKind;
};

/** `typing` frame: typing indicator state for a room. */
export type SidecarTypingFrame = {
  type: "typing";
  room_id: string;
  state: TypingState;
};

/** `error` frame: a server-side protocol/connection error. */
export type SidecarErrorFrame = {
  type: "error";
  code: string;
  message: string;
};

/** `ack` frame: handshake ack has an EMPTY `event_id`; outbound acks do not. */
export type SidecarAckFrame = {
  type: "ack";
  event_id: string;
};

export type SidecarFrame =
  | SidecarInboundFrame
  | SidecarTypingFrame
  | SidecarErrorFrame
  | SidecarAckFrame;

/**
 * Client-side request for an `outbound` frame.
 *
 * The wire frame always carries all five validated fields: absent
 * `reply_to`/`kind`/`media` are sent as `null` / `"final"` / `[]` (the core
 * sidecar rejects an outbound frame missing any of the five).
 */
export type SidecarOutboundRequest = {
  room_id: string;
  text: string;
  reply_to?: string | null;
  kind?: OutboundKind;
  media?: SidecarMediaRef[];
};

/** Resolved from the sidecar's `ack` for an outbound send. */
export type SidecarOutboundAck = {
  event_id: string;
};

export type ConnectOptions = {
  /** Non-empty agent name advertised in `hello`. */
  agent: string;
  /** Protocol version to advertise. Defaults to {@link PROTOCOL_VERSION}. */
  protocol?: number;
  /** Capability list advertised in `hello`. Defaults to `[]`. */
  capabilities?: string[];
  /** Handshake timeout in ms. Defaults to {@link DEFAULT_HANDSHAKE_TIMEOUT_MS}. */
  handshakeTimeoutMs?: number;
};

export type SidecarClientState = "disconnected" | "connecting" | "connected" | "closed";

/**
 * Thrown when the sidecar handshake fails: timeout, protocol mismatch,
 * connection-limit rejection, or the server closing before the handshake ack.
 */
export class SidecarHandshakeError extends Error {
  readonly code?: string;

  constructor(message: string, code?: string, cause?: unknown) {
    super(message, cause !== undefined ? { cause } : undefined);
    this.name = "SidecarHandshakeError";
    this.code = code;
  }
}

/** A protocol-level framing violation (invalid JSON/line, unknown shape). */
export class SidecarProtocolError extends Error {
  readonly code?: string;

  constructor(message: string, code?: string) {
    super(message);
    this.name = "SidecarProtocolError";
    this.code = code;
  }
}

/** Raised when an operation requires an established connection. */
export class SidecarNotConnectedError extends Error {
  constructor(message = "sidecar client is not connected") {
    super(message);
    this.name = "SidecarNotConnectedError";
  }
}

type PendingOutbound = {
  resolve: (ack: SidecarOutboundAck) => void;
  reject: (err: Error) => void;
};

type HandshakeSettlers = {
  resolve: () => void;
  reject: (err: Error) => void;
};

export type InboundHandler = (frame: SidecarInboundFrame) => void;
export type TypingHandler = (frame: SidecarTypingFrame) => void;
export type ErrorHandler = (err: Error) => void;
export type CloseHandler = (err?: Error) => void;

/**
 * A single-connection client for one sidecar.
 *
 * Events:
 *   - `ready`  — handshake completed; the connection is usable.
 *   - `inbound` — a typed {@link SidecarInboundFrame}.
 *   - `typing`  — a typed {@link SidecarTypingFrame}.
 *   - `error`   — an {@link Error} (protocol/socket). Attach a listener; see
 *                 {@link SidecarClient.onError}.
 *   - `close`  — terminal close; argument is the {@link Error} if the close was
 *                the result of a failure, otherwise `undefined`.
 */
export class SidecarClient extends EventEmitter {
  private socket: net.Socket | null = null;
  private lineBuffer = "";
  private state: SidecarClientState = "disconnected";
  private pendingOutbounds: PendingOutbound[] = [];
  private handshake: HandshakeSettlers | null = null;
  private handshakeTimer: NodeJS.Timeout | null = null;
  private lastErrorValue: Error | null = null;

  /** True only after a successful handshake and before the terminal close. */
  get connected(): boolean {
    return this.state === "connected";
  }

  get stateValue(): SidecarClientState {
    return this.state;
  }

  /** Most recent error surfaced on the `error` event, if any. */
  get lastError(): Error | null {
    return this.lastErrorValue;
  }

  /**
   * Open the Unix socket, perform the hello/ack handshake, and resolve once the
   * connection is usable.
   *
   * @throws {Error} Synchronously if a connection is already active
   *   (single-connection invariant). Reconnection after a terminal close is
   *   permitted and starts a fresh handshake.
   * @rejects {SidecarHandshakeError} On timeout, protocol mismatch,
   *   connection-limit rejection, or a close before the handshake ack.
   */
  connect(socketPath: string, options: ConnectOptions): Promise<void> {
    if (this.state === "connecting" || this.state === "connected") {
      throw new Error("connect() called while a connection is already active");
    }

    const protocol = options.protocol ?? PROTOCOL_VERSION;
    const timeoutMs = options.handshakeTimeoutMs ?? DEFAULT_HANDSHAKE_TIMEOUT_MS;

    this.state = "connecting";
    this.lineBuffer = "";
    this.pendingOutbounds = [];

    return new Promise<void>((resolve, reject) => {
      const socket = net.createConnection({ path: socketPath });
      this.socket = socket;
      this.handshake = { resolve, reject };

      socket.setEncoding("utf8");

      this.handshakeTimer = setTimeout(() => {
        this.teardown(new SidecarHandshakeError("handshake timed out", "handshake_timeout"));
      }, timeoutMs);
      // The timer must not keep the process alive on its own.
      this.handshakeTimer.unref?.();

      socket.on("connect", () => {
        // The sidecar can speak first: its reject path sends an error frame
        // at connection time, before reading our hello. Drain whatever
        // already arrived before writing — a `hello` write to a peer that
        // just closed raises EPIPE, and Node hard-destroys the socket on
        // EPIPE (no opt-out), dropping the queued error frame and reducing
        // a connection_limit rejection to a bare close.
        const pending = socket.read();
        if (pending) this.handleData(pending);
        if (this.state !== "connecting") return;
        const hello = {
          type: "hello",
          protocol,
          agent: options.agent,
          capabilities: options.capabilities ?? [],
        };
        socket.write(`${JSON.stringify(hello)}\n`);
      });
      socket.on("data", (chunk: string) => this.handleData(chunk));
      socket.on("error", (err: Error) => {
        // While connecting, a socket-level error (EPIPE/ECONNRESET) is usually
        // the aftermath of the server's fail-closed teardown: the protocol
        // error frame (e.g. connection_limit) is still in the receive buffer.
        // Tearing down here with the raw errno would mask the protocol reason;
        // the follow-up `close` event finalizes the rejection instead.
        if (this.state === "connecting") return;
        this.teardown(err);
      });
      socket.on("close", () => this.teardown());
    });
  }

  /**
   * Send an `outbound` frame and resolve with the sidecar's `ack` `event_id`.
   * Outbound acks are correlated FIFO: the server acks in the order it receives
   * outbounds on the single connection.
   *
   * @throws {SidecarNotConnectedError} If the handshake has not completed.
   */
  sendOutbound(request: SidecarOutboundRequest): Promise<SidecarOutboundAck> {
    if (this.state !== "connected" || this.socket === null) {
      return Promise.reject(new SidecarNotConnectedError());
    }
    const socket = this.socket;

    // The core sidecar validates EVERY outbound frame for all five fields
    // (`_validate_frame` in `plugins/agentteams-matrix/core/sidecar.py`,
    // protocol version 1), so absent values must be sent as their explicit
    // defaults rather than omitted: `reply_to: null`, `kind: "final"`,
    // `media: []`.
    const frame: Record<string, unknown> = {
      type: "outbound",
      room_id: request.room_id,
      text: request.text,
      reply_to: request.reply_to ?? null,
      kind: request.kind ?? "final",
      media: request.media ?? [],
    };

    return new Promise<SidecarOutboundAck>((resolve, reject) => {
      const pending: PendingOutbound = { resolve, reject };
      this.pendingOutbounds.push(pending);
      socket.write(`${JSON.stringify(frame)}\n`, (err) => {
        if (err) {
          const index = this.pendingOutbounds.indexOf(pending);
          if (index !== -1) this.pendingOutbounds.splice(index, 1);
          reject(err);
        }
      });
    });
  }

  /** Register an `inbound` frame handler. */
  onInbound(handler: InboundHandler): this {
    return this.on("inbound", handler);
  }

  /** Register a `typing` frame handler. */
  onTyping(handler: TypingHandler): this {
    return this.on("typing", handler);
  }

  /**
   * Register an `error` handler. Attaching at least one `error` listener is
   * expected; without one, errors are still recorded in {@link lastError} but
   * not thrown (unlike a bare EventEmitter).
   */
  onError(handler: ErrorHandler): this {
    return this.on("error", handler);
  }

  /** Register a terminal `close` handler. */
  onClose(handler: CloseHandler): this {
    return this.on("close", handler);
  }

  /**
   * Close the connection. Idempotent: safe to call multiple times and after a
   * server-initiated close. Emits a single terminal `close` event.
   */
  close(): void {
    if (this.state === "closed") return;
    this.teardown();
  }

  // ---------------------------------------------------------------------------
  // Internal
  // ---------------------------------------------------------------------------

  private handleData(chunk: string): void {
    this.lineBuffer += chunk;

    let newlineIndex: number;
    while ((newlineIndex = this.lineBuffer.indexOf("\n")) !== -1) {
      const line = this.lineBuffer.slice(0, newlineIndex);
      this.lineBuffer = this.lineBuffer.slice(newlineIndex + 1);
      if (Buffer.byteLength(line, "utf8") > MAX_LINE_BYTES) {
        this.teardown(new SidecarProtocolError("line exceeds limit", "line_exceeds_limit"));
        return;
      }
      this.handleLine(line);
      if (this.state === "closed") return;
    }

    // A partially-received line that is already over the limit can only grow.
    if (Buffer.byteLength(this.lineBuffer, "utf8") > MAX_LINE_BYTES) {
      this.teardown(new SidecarProtocolError("line exceeds limit", "line_exceeds_limit"));
    }
  }

  private handleLine(line: string): void {
    if (line.trim() === "") return;

    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      this.teardown(new SidecarProtocolError("invalid JSON line", "invalid_json"));
      return;
    }
    this.handleFrame(parsed);
  }

  private handleFrame(frame: unknown): void {
    if (typeof frame !== "object" || frame === null || Array.isArray(frame)) {
      this.teardown(new SidecarProtocolError("frame must be a JSON object", "invalid_frame"));
      return;
    }
    const type = (frame as { type?: unknown }).type;
    if (typeof type !== "string") {
      this.teardown(new SidecarProtocolError("frame is missing a string 'type'", "invalid_frame"));
      return;
    }

    switch (type) {
      case "ack":
        this.handleAck(frame as SidecarAckFrame);
        break;
      case "inbound":
        if (this.state === "connected") this.emit("inbound", frame as SidecarInboundFrame);
        break;
      case "typing":
        if (this.state === "connected") this.emit("typing", frame as SidecarTypingFrame);
        break;
      case "error":
        this.handleErrorFrame(frame as SidecarErrorFrame);
        break;
      default:
        // Unknown frame type: surface an error event and keep parsing.
        this.emitError(
          new SidecarProtocolError(`unknown frame type: ${String(type)}`, "unknown_type"),
        );
    }
  }

  private handleAck(frame: SidecarAckFrame): void {
    const eventId = typeof frame.event_id === "string" ? frame.event_id : "";

    if (this.state === "connecting") {
      // The handshake ack must carry an EMPTY event_id.
      if (eventId !== "") {
        this.teardown(
          new SidecarProtocolError("handshake ack must have an empty event_id", "invalid_frame"),
        );
        return;
      }
      this.completeHandshake();
      return;
    }

    // After the handshake, a non-empty event_id acks the next pending outbound.
    if (eventId === "") {
      this.teardown(new SidecarProtocolError("unexpected empty ack after handshake", "invalid_frame"));
      return;
    }
    const pending = this.pendingOutbounds.shift();
    if (pending) {
      pending.resolve({ event_id: eventId });
    } else {
      this.teardown(new SidecarProtocolError("ack without a pending outbound", "invalid_frame"));
    }
  }

  private handleErrorFrame(frame: SidecarErrorFrame): void {
    const code = typeof frame.code === "string" ? frame.code : "unknown";
    const message = typeof frame.message === "string" ? frame.message : "sidecar error";

    if (this.state === "connecting") {
      // Handshake failure (protocol_mismatch, connection_limit, handshake_timeout, ...).
      this.teardown(new SidecarHandshakeError(message, code));
      return;
    }

    // Post-handshake error frames are protocol/connection failures: fail closed.
    this.teardown(new SidecarProtocolError(message, code));
  }

  private completeHandshake(): void {
    this.clearHandshakeTimer();
    this.state = "connected";
    const handshake = this.handshake;
    this.handshake = null;
    this.emit("ready");
    handshake?.resolve();
  }

  private teardown(err?: Error): void {
    if (this.state === "closed") return;
    this.clearHandshakeTimer();

    const closeError = err ?? new SidecarNotConnectedError();
    const pending = this.pendingOutbounds.splice(0);
    for (const p of pending) p.reject(closeError);

    if (this.state === "connecting") {
      const handshake = this.handshake;
      this.handshake = null;
      handshake?.reject(
        err ?? new SidecarHandshakeError("connection closed during handshake", "connection_closed"),
      );
    }

    this.state = "closed";
    this.destroySocket();
    if (err) this.emitError(err);
    this.emit("close", err);
  }

  private emitError(err: Error): void {
    this.lastErrorValue = err;
    if (this.listenerCount("error") > 0) {
      this.emit("error", err);
    }
  }

  private clearHandshakeTimer(): void {
    if (this.handshakeTimer !== null) {
      clearTimeout(this.handshakeTimer);
      this.handshakeTimer = null;
    }
  }

  private destroySocket(): void {
    const socket = this.socket;
    this.socket = null;
    if (socket) {
      socket.removeAllListeners();
      socket.destroy();
    }
  }
}
