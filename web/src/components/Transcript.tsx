import { useEffect, useRef } from "react";
import { Alert } from "@heroui/react";
import type { PlanEntry, Transcript as TranscriptModel, TranscriptItem } from "../lib/acp";
import { ToolCall } from "./ToolCall";

const PLAN_GLYPH: Record<string, string> = {
  pending: "○",
  in_progress: "◐",
  completed: "●",
};

function PlanList({ entries }: { entries: PlanEntry[] }) {
  return (
    <div className="rounded-lg border border-border bg-surface p-3">
      <div className="text-xs font-medium text-muted mb-2">plan</div>
      <ul className="space-y-1">
        {entries.map((entry, i) => {
          const done = entry.status === "completed";
          return (
            <li key={i} className="flex gap-2 text-sm">
              <span aria-hidden className="text-muted shrink-0">
                {PLAN_GLYPH[entry.status ?? "pending"] ?? "○"}
              </span>
              <span className={done ? "line-through text-muted" : undefined}>{entry.content}</span>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function Item({ item }: { item: TranscriptItem }) {
  switch (item.kind) {
    case "user":
      return (
        <div className="flex justify-end">
          <div className="max-w-[85%] rounded-lg bg-accent-soft text-accent-soft-foreground px-3 py-2 glance-prose text-sm">
            {item.text}
          </div>
        </div>
      );

    case "assistant":
      return <div className="glance-prose text-sm">{item.text}</div>;

    case "thought":
      // Thinking is dimmed rather than hidden: on a phone it is often the only
      // sign of life during a long tool-free stretch, but it must never compete
      // with the answer for attention.
      return (
        <div className="border-l-2 border-border pl-3 text-sm text-muted glance-prose italic">
          {item.text}
        </div>
      );

    case "tool":
      return <ToolCall call={item.call} />;

    case "plan":
      return <PlanList entries={item.entries} />;

    case "notice":
      return (
        <Alert status="default" className="text-sm">
          <Alert.Content>
            <Alert.Description>{item.text}</Alert.Description>
          </Alert.Content>
        </Alert>
      );

    case "turn-end":
      return (
        <div className="flex items-center gap-3 py-1" aria-label="turn complete">
          <span className="h-px grow bg-separator" />
          <span className="text-[0.6875rem] uppercase tracking-wider text-muted">turn complete</span>
          <span className="h-px grow bg-separator" />
        </div>
      );
  }
}

/**
 * The mirrored session, rendered.
 *
 * Scrolling sticks to the bottom only while the reader is already there. The
 * alternative — always scrolling — yanks the view away mid-sentence every time
 * a chunk lands, which makes reading back through a long turn impossible on the
 * device this is most likely to be read on.
 */
export function Transcript({
  transcript,
  className,
}: {
  transcript: TranscriptModel;
  className?: string;
}) {
  const viewport = useRef<HTMLDivElement | null>(null);
  const pinned = useRef(true);

  useEffect(() => {
    const el = viewport.current;
    if (el && pinned.current) el.scrollTop = el.scrollHeight;
  }, [transcript]);

  const onScroll = () => {
    const el = viewport.current;
    if (!el) return;
    pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
  };

  return (
    <div ref={viewport} onScroll={onScroll} className={className}>
      <div className="mx-auto max-w-3xl px-4 py-4 space-y-3">
        {transcript.dropped > 0 ? (
          <div className="text-xs text-muted text-center">
            {transcript.dropped} earlier {transcript.dropped === 1 ? "event" : "events"} fell out of
            the buffer
          </div>
        ) : null}

        {transcript.items.length === 0 ? (
          <div className="text-sm text-muted text-center py-12">
            nothing mirrored yet — this fills in as the session runs
          </div>
        ) : (
          transcript.items.map((item) => <Item key={item.id} item={item} />)
        )}
      </div>
    </div>
  );
}
