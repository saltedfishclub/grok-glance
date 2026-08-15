import { Alert, Button } from "@heroui/react";
import type { Transcript as TranscriptModel } from "../lib/acp";
import type { AgentSummary } from "../lib/api";
import { shortPath } from "../lib/format";
import { interactionKey, type ConnectionState, type Interaction } from "../lib/ws";
import { PermissionDialog } from "../components/PermissionDialog";
import { PromptBox } from "../components/PromptBox";
import { Transcript } from "../components/Transcript";
import { TurnStatus } from "../components/TurnStatus";

export function Session({
  agent,
  transcript,
  interactions,
  connection,
  onBack,
  onPrompt,
  onCancel,
  onAnswer,
  onDecline,
}: {
  /** Undefined once the session disconnects — the transcript stays readable. */
  agent: AgentSummary | undefined;
  transcript: TranscriptModel;
  interactions: Interaction[];
  connection: ConnectionState;
  onBack: () => void;
  onPrompt: (text: string) => void;
  onCancel: () => void;
  onAnswer: (interaction: Interaction, result: unknown) => void;
  onDecline: (interaction: Interaction, reason: string) => void;
}) {
  const meta = agent?.session ?? {};
  const live = connection === "open" && agent !== undefined;

  return (
    <div className="h-full flex flex-col">
      <header className="border-b border-border shrink-0">
        <div className="mx-auto max-w-3xl px-4 py-3 flex items-center gap-3">
          <Button size="sm" variant="ghost" onPress={onBack} aria-label="back to sessions">
            ←
          </Button>
          <div className="min-w-0 grow">
            <div className="text-sm font-semibold truncate">{agent?.label ?? "session"}</div>
            <div className="text-xs text-muted truncate">
              {[meta.model, meta.hostname, shortPath(meta.cwd)].filter(Boolean).join(" · ")}
            </div>
          </div>
          <TurnStatus
            connection={connection}
            turnActive={transcript.turnActive}
            pending={interactions.length}
          />
        </div>
      </header>

      {agent === undefined ? (
        <div className="mx-auto max-w-3xl w-full px-4 pt-3 shrink-0">
          <Alert status="warning">
            <Alert.Content>
              <Alert.Title>This session has disconnected</Alert.Title>
              <Alert.Description>
                What was mirrored is still here to read. It will reappear if the session runs
                <code className="glance-pre"> /rc </code>again.
              </Alert.Description>
            </Alert.Content>
          </Alert>
        </div>
      ) : null}

      <Transcript transcript={transcript} className="grow overflow-y-auto" />

      {interactions.length > 0 ? (
        <div className="shrink-0 max-h-[60vh] overflow-y-auto border-t border-border bg-surface-secondary">
          <div className="mx-auto max-w-3xl px-4 py-3 space-y-3">
            {interactions.map((interaction) => (
              <PermissionDialog
                // Keying on the interaction id is what resets the form state
                // when one card replaces another: React would otherwise reuse
                // the mounted component and carry the previous answer over.
                key={interactionKey(interaction.id)}
                interaction={interaction}
                onAnswer={(result) => onAnswer(interaction, result)}
                onDecline={(reason) => onDecline(interaction, reason)}
              />
            ))}
          </div>
        </div>
      ) : null}

      <div className="shrink-0">
        <PromptBox
          disabled={!live}
          turnActive={transcript.turnActive}
          onSend={onPrompt}
          onStop={onCancel}
        />
      </div>
    </div>
  );
}
