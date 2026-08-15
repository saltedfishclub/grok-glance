import { useCallback, useEffect, useRef, useState } from "react";
import { Alert, Button, Spinner, useTheme } from "@heroui/react";
import { ApiError, api, type AgentSummary, type Status } from "./lib/api";
import {
  applyFrame,
  applyFrames,
  appendNotice,
  emptyTranscript,
  type Transcript,
} from "./lib/acp";
import {
  GlanceSocket,
  interactionKey,
  type ConnectionState,
  type Interaction,
  type ServerEvent,
} from "./lib/ws";
import { Login } from "./pages/Login";
import { Session } from "./pages/Session";
import { Sessions } from "./pages/Sessions";
import { Setup } from "./pages/Setup";

/** `#/a/<agent-id>` selects a session; anything else is the list. */
function agentFromHash(): string | null {
  const match = /^#\/a\/(.+)$/.exec(window.location.hash);
  const id = match?.[1];
  return id ? decodeURIComponent(id) : null;
}

/**
 * The signed-in application: one socket, one agent list, one open session.
 *
 * The socket is owned here rather than in the session page so that navigating
 * between the list and a session neither drops the connection nor re-requests a
 * snapshot — and so the agent list keeps updating while a session is open.
 */
function Console({ onSignedOut }: { onSignedOut: () => void }) {
  const [connection, setConnection] = useState<ConnectionState>("connecting");
  const [agents, setAgents] = useState<AgentSummary[]>([]);
  const [selected, setSelected] = useState<string | null>(() => agentFromHash());
  const [transcript, setTranscript] = useState<Transcript>(() => emptyTranscript());
  const [interactions, setInteractions] = useState<Interaction[]>([]);

  const socket = useRef<GlanceSocket | null>(null);

  // Latest-value ref: the socket's handlers are installed once, but they have to
  // test events against whichever session is open *now*.
  const selectedRef = useRef(selected);
  selectedRef.current = selected;

  useEffect(() => {
    const onHashChange = () => setSelected(agentFromHash());
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);

  useEffect(() => {
    const onEvent = (event: ServerEvent) => {
      switch (event.type) {
        case "agents":
          setAgents(event.agents ?? []);
          break;

        case "snapshot": {
          if (event.agents) setAgents(event.agents);
          if (event.agent !== selectedRef.current) break;
          // A snapshot is authoritative: it replaces local state rather than
          // merging into it, which is what makes a reload or a reconnect land
          // on exactly the server's view instead of a half-stale one.
          setTranscript(
            applyFrames(
              emptyTranscript(event.dropped ?? 0, event.turnActive ?? false),
              event.frames ?? [],
            ),
          );
          setInteractions(event.open ?? []);
          break;
        }

        case "frame":
          if (event.agent !== selectedRef.current) break;
          setTranscript((current) => applyFrame(current, event.frame));
          break;

        case "interaction": {
          if (event.agent !== selectedRef.current) break;
          const key = interactionKey(event.interaction.id);
          setInteractions((current) =>
            current.some((item) => interactionKey(item.id) === key)
              ? current
              : [...current, event.interaction],
          );
          break;
        }

        case "interaction_resolved": {
          if (event.agent !== selectedRef.current) break;
          const resolved = event.id;
          setInteractions((current) =>
            current.filter((item) => interactionKey(item.id) !== resolved),
          );
          // Losing the race is normal, not an error — but it must be visible,
          // or a card vanishing under your finger looks like a bug.
          const message = event.message;
          if (event.by === "elsewhere" && message) {
            setTranscript((current) => appendNotice(current, message));
          }
          break;
        }

        case "notice":
          if (event.agent && event.agent !== selectedRef.current) break;
          setTranscript((current) => appendNotice(current, event.message));
          break;

        case "error":
          setTranscript((current) => appendNotice(current, event.message));
          break;
      }
    };

    const onState = (state: ConnectionState) => {
      setConnection(state);
      if (state !== "closed") return;
      // A rejected upgrade and an unreachable server close identically, so the
      // only honest way to tell an expired session from a restart is to ask.
      api
        .status()
        .then((status) => {
          if (!status.authenticated) onSignedOut();
        })
        .catch(() => {
          /* server is down; the socket's own backoff handles it */
        });
    };

    const instance = new GlanceSocket({ onEvent, onState });
    socket.current = instance;
    instance.start();
    return () => {
      instance.stop();
      socket.current = null;
    };
  }, [onSignedOut]);

  useEffect(() => {
    if (!selected) {
      socket.current?.clearSubscription();
      return;
    }
    setTranscript(emptyTranscript());
    setInteractions([]);
    socket.current?.subscribe(selected);
  }, [selected]);

  const signOut = useCallback(() => {
    void api.logout().finally(onSignedOut);
  }, [onSignedOut]);

  if (!selected) {
    return (
      <Sessions
        agents={agents}
        connection={connection}
        onOpen={(id) => {
          window.location.hash = `#/a/${encodeURIComponent(id)}`;
        }}
        onSignOut={signOut}
      />
    );
  }

  const agent = agents.find((candidate) => candidate.id === selected);

  return (
    <Session
      agent={agent}
      transcript={transcript}
      interactions={interactions}
      connection={connection}
      onBack={() => {
        window.location.hash = "#/";
      }}
      onPrompt={(text) => socket.current?.send({ type: "prompt", agent: selected, text })}
      onCancel={() => socket.current?.send({ type: "cancel", agent: selected })}
      onAnswer={(interaction, result) =>
        socket.current?.send({
          type: "answer",
          agent: selected,
          id: interactionKey(interaction.id),
          result,
        })
      }
      onDecline={(interaction, reason) =>
        socket.current?.send({
          type: "decline",
          agent: selected,
          id: interactionKey(interaction.id),
          reason,
        })
      }
    />
  );
}

export function App() {
  // Called once, at the root: each call owns its own state, so a second call
  // elsewhere would give a toggle that only half the app agrees with. "system"
  // means the page follows the OS, which is the right default for something
  // read on a phone at night.
  useTheme("system");

  const [status, setStatus] = useState<Status | null>(null);
  const [fatal, setFatal] = useState<string | null>(null);

  const refresh = useCallback(() => {
    api
      .status()
      .then((next) => {
        setStatus(next);
        setFatal(null);
      })
      .catch((cause: unknown) => {
        setFatal(cause instanceof ApiError ? cause.message : "could not reach the glance server");
      });
  }, []);

  useEffect(refresh, [refresh]);

  if (fatal) {
    return (
      <div className="min-h-full flex items-center justify-center p-4">
        <div className="max-w-sm w-full space-y-3">
          <Alert status="danger">
            <Alert.Content>
              <Alert.Title>Cannot reach glance</Alert.Title>
              <Alert.Description>{fatal}</Alert.Description>
            </Alert.Content>
          </Alert>
          <Button fullWidth variant="outline" onPress={refresh}>
            Retry
          </Button>
        </div>
      </div>
    );
  }

  if (!status) {
    return (
      <div className="min-h-full flex items-center justify-center">
        <Spinner color="accent" aria-label="loading" />
      </div>
    );
  }

  if (!status.enrolled) return <Setup onDone={refresh} />;
  if (!status.authenticated) return <Login onDone={refresh} />;
  return <Console onSignedOut={refresh} />;
}
