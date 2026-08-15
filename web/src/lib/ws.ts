/**
 * The browser's socket to glance.
 *
 * This is deliberately *not* an ACP peer. glance translates the ACP link into a
 * small envelope so the frontend never has to correlate JSON-RPC ids — with one
 * exception: an interaction's `id` is the agent's own JSON-RPC id, echoed back
 * verbatim, because that is what the answer has to be addressed to.
 */

import type { AgentSummary, SessionMeta } from "./api";

export interface Interaction {
  /** grok's JSON-RPC id, as a JSON value. Echoed back untouched when answering. */
  id: unknown;
  method: string;
  params: unknown;
  toolCallId?: string;
  openedAt: string;
}

export type ServerEvent =
  | {
      type: "snapshot";
      agent: string;
      agents?: AgentSummary[];
      frames?: unknown[];
      dropped?: number;
      open?: Interaction[];
      session?: SessionMeta;
      turnActive?: boolean;
    }
  | { type: "agents"; agents?: AgentSummary[] }
  | { type: "frame"; agent: string; frame: unknown }
  | { type: "interaction"; agent: string; interaction: Interaction }
  | {
      type: "interaction_resolved";
      agent: string;
      id?: string;
      toolCallId?: string;
      by?: string;
      message?: string;
    }
  | { type: "notice"; agent?: string; message: string }
  | { type: "error"; agent?: string; message: string };

export type ClientCommand =
  | { type: "list" }
  | { type: "subscribe"; agent: string }
  | { type: "prompt"; agent: string; text: string }
  | { type: "cancel"; agent: string }
  | { type: "answer"; agent: string; id: string; result: unknown }
  | { type: "decline"; agent: string; id: string; reason?: string };

export type ConnectionState = "connecting" | "open" | "closed";

/**
 * `Interaction.id` arrives as a parsed JSON value but is *keyed* server-side by
 * its raw JSON text (`string(frame.ID)` in internal/hub/agent.go). Re-encoding
 * it is what makes an answer land on the right pending entry: an id of `7`
 * becomes `"7"` and an id of `"abc"` becomes `"\"abc\""`, matching Go's
 * `json.RawMessage` bytes on both sides.
 */
export function interactionKey(id: unknown): string {
  return JSON.stringify(id ?? null);
}

export interface GlanceSocketHandlers {
  onEvent: (event: ServerEvent) => void;
  /**
   * Connection state changes.
   *
   * A rejected upgrade (401, cookie expired) and an unreachable server both
   * surface as an abnormal close with no close frame, so this reports "closed"
   * either way and leaves the app to ask `/api/status` which one it was.
   * Guessing from the close code would send people to a login page whenever the
   * server restarted.
   */
  onState: (state: ConnectionState) => void;
}

/**
 * A reconnecting socket.
 *
 * Reconnection is not optional here: laptops sleep, phones switch networks, and
 * a viewer that silently stops updating is worse than one that says it is
 * offline — it looks like an idle agent.
 */
export class GlanceSocket {
  private socket: WebSocket | null = null;
  private handlers: GlanceSocketHandlers;
  private attempt = 0;
  private timer: number | null = null;
  private stopped = false;
  /** Re-sent on every reconnect so a resumed socket refills its transcript. */
  private subscription: string | null = null;

  constructor(handlers: GlanceSocketHandlers) {
    this.handlers = handlers;
  }

  start(): void {
    this.stopped = false;
    this.open();
  }

  stop(): void {
    this.stopped = true;
    if (this.timer !== null) {
      window.clearTimeout(this.timer);
      this.timer = null;
    }
    this.socket?.close();
    this.socket = null;
  }

  send(command: ClientCommand): boolean {
    if (this.socket?.readyState !== WebSocket.OPEN) return false;
    this.socket.send(JSON.stringify(command));
    return true;
  }

  /** Subscribe, and remember it so a reconnect restores the same view. */
  subscribe(agent: string): void {
    this.subscription = agent;
    this.send({ type: "subscribe", agent });
  }

  clearSubscription(): void {
    this.subscription = null;
  }

  private open(): void {
    if (this.stopped) return;
    this.handlers.onState("connecting");

    const url = new URL("/api/ws", window.location.href);
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(url);
    this.socket = socket;

    socket.onopen = () => {
      this.attempt = 0;
      this.handlers.onState("open");
      if (this.subscription) this.send({ type: "subscribe", agent: this.subscription });
    };

    socket.onmessage = (event) => {
      if (typeof event.data !== "string") return;
      let parsed: ServerEvent;
      try {
        parsed = JSON.parse(event.data) as ServerEvent;
      } catch {
        return;
      }
      this.handlers.onEvent(parsed);
    };

    socket.onclose = () => {
      this.socket = null;
      this.handlers.onState("closed");
      if (this.stopped) return;
      this.scheduleReconnect();
    };

    socket.onerror = () => {
      // `onclose` always follows; handling both would double the backoff.
    };
  }

  private scheduleReconnect(): void {
    // Capped exponential backoff with jitter. The cap is low because this is a
    // local-network tool and a viewer that takes 30s to come back after a
    // laptop wakes up reads as broken.
    const base = Math.min(500 * 2 ** this.attempt, 5000);
    const delay = base + Math.random() * 250;
    this.attempt += 1;
    this.timer = window.setTimeout(() => {
      this.timer = null;
      this.open();
    }, delay);
  }
}
