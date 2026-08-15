import { Button, Card, Chip, EmptyState, Spinner } from "@heroui/react";
import type { AgentSummary } from "../lib/api";
import { ago, shortPath } from "../lib/format";
import type { ConnectionState } from "../lib/ws";

function AgentCard({ agent, onOpen }: { agent: AgentSummary; onOpen: () => void }) {
  const meta = agent.session ?? {};
  return (
    <Card>
      <Card.Header>
        <Card.Title className="text-base truncate">{agent.label}</Card.Title>
        <Card.Description className="text-xs truncate">
          {[meta.model, meta.hostname, shortPath(meta.cwd)].filter(Boolean).join(" · ")}
        </Card.Description>
      </Card.Header>

      <Card.Content className="flex flex-wrap items-center gap-2">
        {agent.turnActive ? (
          <span className="flex items-center gap-1.5 text-xs text-muted">
            <Spinner size="sm" color="accent" aria-label="turn in progress" />
            working
          </span>
        ) : (
          <span className="text-xs text-muted">idle · {ago(agent.lastActivity)}</span>
        )}

        {agent.pending > 0 ? (
          <Chip size="sm" color="warning" variant="soft">
            <Chip.Label>{agent.pending} waiting</Chip.Label>
          </Chip>
        ) : null}

        {agent.dropped > 0 ? (
          <Chip size="sm" color="default" variant="soft">
            <Chip.Label>{agent.dropped} dropped</Chip.Label>
          </Chip>
        ) : null}
      </Card.Content>

      <Card.Footer className="flex justify-between items-center">
        <span className="text-xs text-muted truncate">key: {agent.keyName}</span>
        <Button size="sm" onPress={onOpen}>
          Open
        </Button>
      </Card.Footer>
    </Card>
  );
}

export function Sessions({
  agents,
  connection,
  onOpen,
  onSignOut,
}: {
  agents: AgentSummary[];
  connection: ConnectionState;
  onOpen: (id: string) => void;
  onSignOut: () => void;
}) {
  return (
    <div className="min-h-full">
      <header className="border-b border-border">
        <div className="mx-auto max-w-3xl px-4 py-3 flex items-center gap-3">
          <h1 className="text-sm font-semibold grow">grok glance</h1>
          <Chip
            size="sm"
            variant="soft"
            color={connection === "open" ? "success" : connection === "connecting" ? "warning" : "danger"}
          >
            <Chip.Label>{connection === "open" ? "live" : connection}</Chip.Label>
          </Chip>
          <Button size="sm" variant="ghost" onPress={onSignOut}>
            Sign out
          </Button>
        </div>
      </header>

      <main className="mx-auto max-w-3xl px-4 py-6 space-y-3">
        {agents.length === 0 ? (
          <EmptyState className="py-16 text-center">
            <p className="text-sm font-medium">No sessions connected</p>
            <p className="text-sm text-muted mt-2">
              Run <code className="glance-pre">/rc</code> in a grok session to mirror it here.
            </p>
          </EmptyState>
        ) : (
          agents.map((agent) => (
            <AgentCard key={agent.id} agent={agent} onOpen={() => onOpen(agent.id)} />
          ))
        )}
      </main>
    </div>
  );
}
