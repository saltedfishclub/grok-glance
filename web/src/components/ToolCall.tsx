import { Chip, Disclosure, Spinner } from "@heroui/react";
import type { ToolCallContent, ToolCallPayload, ToolCallStatus } from "../lib/acp";

const STATUS_COLOR: Record<string, "default" | "accent" | "success" | "warning" | "danger"> = {
  pending: "default",
  in_progress: "accent",
  completed: "success",
  failed: "danger",
};

/**
 * Tool kinds get a glyph rather than a colour: status already owns colour in
 * this row, and two colour dimensions on one line stop reading as either.
 */
const KIND_GLYPH: Record<string, string> = {
  read: "▤",
  edit: "✎",
  delete: "␡",
  move: "→",
  search: "⌕",
  execute: "❯",
  think: "◇",
  fetch: "↓",
  switch_mode: "⇄",
  other: "•",
};

function DiffBlock({ item }: { item: ToolCallContent }) {
  const oldText = item.oldText ?? "";
  const newText = item.newText ?? "";
  return (
    <div className="rounded-md border border-border overflow-hidden">
      <div className="px-3 py-1.5 text-xs font-medium bg-surface-secondary truncate">
        {item.path ?? "(unnamed file)"}
      </div>
      <div className="grid md:grid-cols-2 divide-y md:divide-y-0 md:divide-x divide-border">
        {oldText ? (
          <pre className="glance-pre p-3 overflow-x-auto bg-danger-soft text-danger-soft-foreground">
            {oldText}
          </pre>
        ) : (
          <div className="p-3 text-xs text-muted">new file</div>
        )}
        <pre className="glance-pre p-3 overflow-x-auto bg-success-soft text-success-soft-foreground">
          {newText}
        </pre>
      </div>
    </div>
  );
}

function ContentItem({ item }: { item: ToolCallContent }) {
  if (item.type === "diff") return <DiffBlock item={item} />;

  if (item.type === "terminal") {
    // Terminal content is a live handle, not data: the bridge does not mirror
    // the pty, so claiming to show output would be a lie.
    return (
      <div className="text-xs text-muted italic">
        live terminal {item.terminalId ?? ""} — output stays in the session
      </div>
    );
  }

  const block = item.content;
  const text =
    block?.text ??
    block?.resource?.text ??
    (block?.type === "image"
      ? "[image]"
      : block?.type === "resource_link"
        ? `[${block.name ?? block.uri ?? "resource"}]`
        : "");
  if (!text) return null;
  return (
    <pre className="glance-pre p-3 rounded-md bg-surface-secondary overflow-auto max-h-96">
      {text}
    </pre>
  );
}

export function ToolCall({ call }: { call: ToolCallPayload }) {
  const status: ToolCallStatus = call.status ?? "pending";
  const running = status === "pending" || status === "in_progress";
  const content = call.content ?? [];
  const glyph = KIND_GLYPH[call.kind ?? "other"] ?? KIND_GLYPH.other;

  const header = (
    <div className="flex items-center gap-2 min-w-0 w-full text-left">
      <span aria-hidden className="text-muted shrink-0 w-4 text-center">
        {glyph}
      </span>
      <span className="truncate text-sm font-medium">{call.title ?? call.kind ?? "tool call"}</span>
      <span className="grow" />
      {running ? (
        <Spinner size="sm" color="accent" aria-label="running" />
      ) : (
        <Chip size="sm" color={STATUS_COLOR[status] ?? "default"} variant="soft">
          <Chip.Label>{status.replace("_", " ")}</Chip.Label>
        </Chip>
      )}
    </div>
  );

  const hasDetail = content.length > 0 || (call.locations?.length ?? 0) > 0;

  // A tool call with nothing to show should not pretend to be expandable: an
  // empty disclosure opening onto blank space reads as a loading bug.
  if (!hasDetail) {
    return (
      <div className="px-3 py-2 rounded-lg border border-border bg-surface">{header}</div>
    );
  }

  return (
    <Disclosure className="rounded-lg border border-border bg-surface">
      <Disclosure.Heading>
        <Disclosure.Trigger className="px-3 py-2 w-full flex items-center gap-2">
          {header}
          <Disclosure.Indicator className="shrink-0" />
        </Disclosure.Trigger>
      </Disclosure.Heading>
      <Disclosure.Content>
        <Disclosure.Body className="px-3 pb-3 space-y-2">
          {call.locations?.length ? (
            <div className="text-xs text-muted truncate">
              {call.locations.map((l) => (l.line ? `${l.path}:${l.line}` : l.path)).join("  ·  ")}
            </div>
          ) : null}
          {content.map((item, i) => (
            <ContentItem key={i} item={item} />
          ))}
        </Disclosure.Body>
      </Disclosure.Content>
    </Disclosure>
  );
}
