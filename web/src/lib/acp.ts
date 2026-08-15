/**
 * ACP frame shapes and the reducer that turns a stream of them into a
 * transcript.
 *
 * Two rails arrive here and both are handled by the same code:
 *
 *   - `session/update` — the stable ACP rail. Correctness is keyed off this.
 *   - `x.ai/session_notification` — grok's own rail, ~60 internal variants
 *     carrying the streaming deltas that make a transcript readable.
 *
 * They share the `{sessionId, update: {sessionUpdate, ...}}` envelope, which is
 * why one reducer covers them. The xAI rail is *not* a stable interface: its
 * variants drift with every upstream sync, so an unrecognised `sessionUpdate`
 * is dropped from the rendering rather than treated as an error. Nothing
 * load-bearing is keyed off it.
 */

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

export interface Frame {
  jsonrpc?: string;
  id?: unknown;
  method?: string;
  params?: unknown;
  result?: unknown;
  error?: { code: number; message: string };
}

export interface ContentBlock {
  type: string;
  text?: string;
  uri?: string;
  name?: string;
  mimeType?: string;
  data?: string;
  annotations?: unknown;
  resource?: { text?: string; uri?: string; mimeType?: string };
}

export interface ToolCallContent {
  type: "content" | "diff" | "terminal" | string;
  content?: ContentBlock;
  path?: string;
  oldText?: string | null;
  newText?: string;
  terminalId?: string;
}

export type ToolCallStatus = "pending" | "in_progress" | "completed" | "failed";

export interface ToolCallLocation {
  path: string;
  line?: number;
}

export interface ToolCallPayload {
  toolCallId?: string;
  title?: string;
  kind?: string;
  status?: ToolCallStatus;
  content?: ToolCallContent[];
  locations?: ToolCallLocation[];
  rawInput?: unknown;
  rawOutput?: unknown;
}

export interface PlanEntry {
  content: string;
  priority?: string;
  status?: "pending" | "in_progress" | "completed" | string;
}

/** The `{sessionId, update}` envelope both rails share. */
export interface UpdateParams {
  sessionId?: string;
  update?: Record<string, unknown>;
  _meta?: Record<string, unknown>;
}

// ---------------------------------------------------------------------------
// Interaction methods
// ---------------------------------------------------------------------------

export const METHOD_REQUEST_PERMISSION = "session/request_permission";
export const METHOD_ASK_USER_QUESTION = "x.ai/ask_user_question";
export const METHOD_EXIT_PLAN_MODE = "x.ai/exit_plan_mode";

export interface PermissionOption {
  optionId: string;
  name: string;
  /** `allow_once` | `allow_always` | `reject_once` | `reject_always` */
  kind?: string;
}

export interface RequestPermissionParams {
  sessionId?: string;
  toolCall?: ToolCallPayload;
  options?: PermissionOption[];
}

export interface QuestionOption {
  label: string;
  description: string;
  preview?: string;
  id?: string;
}

export interface Question {
  question: string;
  options: QuestionOption[];
  multiSelect?: boolean;
  multi_select?: boolean;
  id?: string;
}

export interface AskUserQuestionParams {
  sessionId?: string;
  toolCallId?: string;
  questions?: Question[];
  /** `default` | `plan` — plan mode unlocks two extra outcomes. */
  mode?: string;
}

export interface ExitPlanModeParams {
  sessionId?: string;
  toolCallId?: string;
  planContent?: string | null;
}

// ---------------------------------------------------------------------------
// Turn tracking
// ---------------------------------------------------------------------------

/**
 * Whether an update starts, continues or ends a turn.
 *
 * This mirrors `classifyUpdate` in internal/hub/agent.go deliberately. The
 * server sends `turnActive` only in the snapshot, so a browser that did not
 * derive it from the frame stream would show a stale spinner for the rest of
 * the session. Keeping the two in step matters: if upstream renames
 * `turn_completed`, both sides go wrong the same way rather than disagreeing.
 */
export function turnEffect(kind: string | undefined): "start" | "end" | null {
  switch (kind) {
    case "turn_completed":
      return "end";
    case "agent_message_chunk":
    case "agent_thought_chunk":
    case "tool_call":
    case "tool_call_update":
    case "user_message_chunk":
    case "plan":
    case "pending_interaction":
      return "start";
    default:
      return null;
  }
}

// ---------------------------------------------------------------------------
// Transcript model
// ---------------------------------------------------------------------------

export type TranscriptItem =
  | { kind: "user"; id: string; text: string }
  | { kind: "assistant"; id: string; text: string }
  | { kind: "thought"; id: string; text: string }
  | { kind: "tool"; id: string; call: ToolCallPayload }
  | { kind: "plan"; id: string; entries: PlanEntry[] }
  | { kind: "notice"; id: string; text: string }
  | { kind: "turn-end"; id: string };

export interface Transcript {
  items: TranscriptItem[];
  /** Frames the server's ring buffer discarded before this browser attached. */
  dropped: number;
  turnActive: boolean;
}

export function emptyTranscript(dropped = 0, turnActive = false): Transcript {
  return { items: [], dropped, turnActive };
}

function textOf(content: unknown): string {
  if (typeof content === "string") return content;
  if (!content || typeof content !== "object") return "";
  if (Array.isArray(content)) return content.map(textOf).join("");
  const block = content as ContentBlock;
  if (typeof block.text === "string") return block.text;
  if (block.resource && typeof block.resource.text === "string") return block.resource.text;
  // Images and audio have no textual form; naming them beats rendering nothing
  // and leaving a silent gap in the transcript.
  if (block.type === "image") return "[image]";
  if (block.type === "audio") return "[audio]";
  if (block.type === "resource_link") return `[${block.name ?? block.uri ?? "resource"}]`;
  return "";
}

/**
 * Merge one tool-call update into an existing call.
 *
 * `tool_call_update` is a partial: fields it omits keep their previous value,
 * and `content` arrives as replacement snapshots rather than appends. Treating
 * an update as a whole new call — the obvious shortcut — makes a long edit
 * flicker between titled and untitled as deltas arrive.
 */
function mergeToolCall(previous: ToolCallPayload, next: ToolCallPayload): ToolCallPayload {
  return {
    ...previous,
    ...Object.fromEntries(Object.entries(next).filter(([, v]) => v !== undefined && v !== null)),
    content: next.content ?? previous.content,
  };
}

let syntheticId = 0;

/**
 * Fold one mirrored frame into the transcript, returning a new value.
 *
 * Unknown methods and unknown update kinds are no-ops: this is the code path
 * that has to survive an upstream sync it was not written against.
 */
export function applyFrame(transcript: Transcript, raw: unknown): Transcript {
  const frame = raw as Frame;
  if (!frame || typeof frame !== "object" || typeof frame.method !== "string") {
    return transcript;
  }

  const params = frame.params as UpdateParams | undefined;
  const update = params?.update;
  if (!update || typeof update !== "object") return transcript;

  const kind = typeof update.sessionUpdate === "string" ? update.sessionUpdate : undefined;
  const effect = turnEffect(kind);
  const turnActive =
    effect === "start" ? true : effect === "end" ? false : transcript.turnActive;

  const items = transcript.items;
  const last = items[items.length - 1];

  switch (kind) {
    case "user_message_chunk": {
      const text = textOf(update.content);
      if (!text) break;
      // Chunks of the same kind coalesce into one bubble; without this a
      // streamed reply renders as one paragraph per token.
      if (last?.kind === "user") {
        return {
          ...transcript,
          turnActive,
          items: [...items.slice(0, -1), { ...last, text: last.text + text }],
        };
      }
      return {
        ...transcript,
        turnActive,
        items: [...items, { kind: "user", id: `u${syntheticId++}`, text }],
      };
    }

    case "agent_message_chunk": {
      const text = textOf(update.content);
      if (!text) break;
      if (last?.kind === "assistant") {
        return {
          ...transcript,
          turnActive,
          items: [...items.slice(0, -1), { ...last, text: last.text + text }],
        };
      }
      return {
        ...transcript,
        turnActive,
        items: [...items, { kind: "assistant", id: `a${syntheticId++}`, text }],
      };
    }

    case "agent_thought_chunk": {
      const text = textOf(update.content);
      if (!text) break;
      if (last?.kind === "thought") {
        return {
          ...transcript,
          turnActive,
          items: [...items.slice(0, -1), { ...last, text: last.text + text }],
        };
      }
      return {
        ...transcript,
        turnActive,
        items: [...items, { kind: "thought", id: `t${syntheticId++}`, text }],
      };
    }

    case "tool_call":
    case "tool_call_update": {
      const call = update as ToolCallPayload;
      const id = call.toolCallId;
      if (!id) break;
      const index = items.findIndex((item) => item.kind === "tool" && item.call.toolCallId === id);
      if (index === -1) {
        // An update for a call whose `tool_call` was dropped from the ring is
        // normal on reattach, so it opens a new entry rather than being lost.
        return {
          ...transcript,
          turnActive,
          items: [...items, { kind: "tool", id: `tc:${id}`, call }],
        };
      }
      const existing = items[index] as Extract<TranscriptItem, { kind: "tool" }>;
      const merged = [...items];
      merged[index] = { ...existing, call: mergeToolCall(existing.call, call) };
      return { ...transcript, turnActive, items: merged };
    }

    case "plan": {
      const entries = (update.entries ?? update.plan) as PlanEntry[] | undefined;
      if (!Array.isArray(entries)) break;
      // The plan is a running snapshot, not an append: replacing the previous
      // one keeps the checklist a checklist instead of a pile of revisions.
      const index = items.findIndex((item) => item.kind === "plan");
      const entry: TranscriptItem = { kind: "plan", id: "plan", entries };
      if (index === -1) return { ...transcript, turnActive, items: [...items, entry] };
      const merged = [...items];
      merged[index] = entry;
      return { ...transcript, turnActive, items: merged };
    }

    case "turn_completed": {
      if (last?.kind === "turn-end") return { ...transcript, turnActive };
      return {
        ...transcript,
        turnActive,
        items: [...items, { kind: "turn-end", id: `end${syntheticId++}` }],
      };
    }

    default:
      // An unrecognised variant still counts toward turn state if
      // `turnEffect` claimed it; otherwise it is deliberately invisible.
      break;
  }

  return turnActive === transcript.turnActive ? transcript : { ...transcript, turnActive };
}

/** Replay a snapshot's worth of frames in one pass. */
export function applyFrames(transcript: Transcript, frames: unknown[]): Transcript {
  return frames.reduce<Transcript>((acc, frame) => applyFrame(acc, frame), transcript);
}

export function appendNotice(transcript: Transcript, text: string): Transcript {
  return {
    ...transcript,
    items: [...transcript.items, { kind: "notice", id: `n${syntheticId++}`, text }],
  };
}
