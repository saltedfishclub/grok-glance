import { Chip, Spinner } from "@heroui/react";
import type { ConnectionState } from "../lib/ws";

const CONNECTION: Record<
  ConnectionState,
  { label: string; color: "default" | "accent" | "success" | "warning" | "danger" }
> = {
  connecting: { label: "connecting", color: "warning" },
  open: { label: "live", color: "success" },
  closed: { label: "offline", color: "danger" },
};

/**
 * The two facts a remote viewer needs before trusting anything on screen: is
 * this stream still live, and is the agent working right now.
 *
 * They are shown together because a stalled transcript is ambiguous otherwise —
 * "no new output" looks identical whether the agent is thinking or the socket
 * died ten minutes ago.
 */
export function TurnStatus({
  connection,
  turnActive,
  pending = 0,
}: {
  connection: ConnectionState;
  turnActive: boolean;
  pending?: number;
}) {
  const conn = CONNECTION[connection];
  return (
    <div className="flex items-center gap-2">
      <Chip size="sm" color={conn.color} variant="soft">
        <Chip.Label>{conn.label}</Chip.Label>
      </Chip>

      {turnActive ? (
        <span className="flex items-center gap-1.5 text-xs text-muted">
          <Spinner size="sm" color="accent" aria-label="turn in progress" />
          working
        </span>
      ) : (
        <span className="text-xs text-muted">idle</span>
      )}

      {pending > 0 ? (
        <Chip size="sm" color="warning" variant="soft">
          <Chip.Label>
            {pending} waiting on you
          </Chip.Label>
        </Chip>
      ) : null}
    </div>
  );
}
