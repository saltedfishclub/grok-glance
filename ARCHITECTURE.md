# grok-glance — architecture

grok-glance is remote control for [`grok`](https://github.com/xai-org/grok-build): a
self-hosted server plus web UI that mirrors a *running* terminal session and lets you
drive it from a browser — send prompts, interrupt a turn, approve tool calls — without
the terminal ceding anything.

It is one Go binary with the frontend embedded, one port, and no database.

```
   ┌────────────── your machine ──────────────┐        ┌──── anywhere ────┐
   │  grok TUI                                │        │                  │
   │    │                                     │        │                  │
   │    ├── AcpClientRx ──▶ tee ──▶ TUI       │        │                  │
   │    │                    │                │        │                  │
   │    │                    ▼                │        │                  │
   │    │              /rc bridge ═══ WSS ════╪══▶ glance ══ WSS ══▶ browser
   │    │                    │      (ACP)     │   (Go)     (glance     (HeroUI)
   │    └── AcpAgentTx ◀─────┘                │            envelope)   │
   └──────────────────────────────────────────┘        └──────────────────┘
```

Two links, two protocols, deliberately:

| Link | Protocol | Auth | Who dials |
|---|---|---|---|
| grok ↔ glance | ACP over WebSocket, **grok as the Agent** | `Authorization: Bearer <api key>` | grok dials out |
| glance ↔ browser | glance's own JSON envelope | `__Host-glance` session cookie | browser dials in |

---

## Why it is shaped this way

### grok dials out

`grok` runs on a laptop, in a devcontainer, on a box behind NAT. glance runs where you can
reach it. Making grok the dialer means remote control works without a port forward, a
tunnel, or an inbound firewall rule on the machine that holds your source tree — the one
place you least want an open port.

The consequence is that glance never initiates: an agent appears when it connects and
disappears when it hangs up, and the session list is exactly the set of live sockets.

### The roles are inverted relative to the terminal

Inside grok, the TUI is an ACP **Client** talking to the agent runtime. Over the `/rc`
link grok presents itself as the **Agent** and glance is the **Client**.

That inversion is the whole trick. It means glance is a stock ACP client — it receives
`session/update` and `session/request_permission`, it sends `session/prompt` and
`session/cancel` — and needs no knowledge of grok's internals to be a second head on the
same session. It also means the browser's Stop button is a real `session/cancel`, not a
simulated keypress.

### The browser is not an ACP peer

`/api/ws` speaks a small glance-specific envelope (see [Browser protocol](#browser-protocol)),
not ACP. Translating once, server-side, keeps three things out of the frontend: JSON-RPC
id correlation, the pending-interaction table, and the ring buffer. The browser sends
`{"type":"prompt", ...}` and receives `{"type":"frame", ...}`; everything that must be
exactly right about the ACP conversation is exactly right in one place, in Go, with tests.

### No database

Transcript history is a per-agent in-memory ring (4096 frames, `internal/hub/ring.go`).
A reload replays it; a server restart does not. This was a deliberate choice: the
alternative is a store of every prompt, file path, diff and command output from every
session, on disk, forever, guarded by one TOTP secret. The ring is bounded, unbackupable
by construction, and enough for what the UI is actually for — watching the turn that is
happening now.

What *is* persisted is only what cannot be re-derived: the TOTP secret, the cookie signing
key, and API key hashes, in `~/.grok/glance/state.json` (0600).

---

## The grok side (`/rc`)

Implemented as patches `0003`/`0004` in the sibling `newgrok/` tree. Summarised here
because the two halves only make sense together; the authoritative comments are in
`newgrok/work/crates/codegen/xai-grok-pager/src/rc/`.

**The tee.** The pager already holds an `AcpAgentTx` (to the agent) and an `AcpClientRx`
(from it). Every inbound message passes through one function, so `/rc` inserts one call
there: mirror a copy to the bridge, return the original untouched. Remote control needs no
change to the agent runtime, the session actor, or the shell — which is what makes "does
not disturb the session" true rather than aspirational.

**Both notification rails are mirrored.** Alongside the stable `session/update`, grok emits
~60 grok-specific variants as `x.ai/session_notification` — tool-call deltas, subagents,
retries, turn boundaries. Forwarding only the standard rail would give glance a transcript
with the streaming taken out. `_meta` (`eventId`, `promptId`, `chunkId`, `isReplay`) is
forwarded verbatim so a viewer can dedup and order the stream the same way the TUI does.

**Interactions are raced, not routed.** The three reverse-requests a human must answer —
`session/request_permission`, `x.ai/ask_user_question`, `x.ai/exit_plan_mode` — carry their
own oneshot reply channel. The tee lifts the real sender out, hands the TUI a substitute,
forwards a copy to glance, and gives the answer to whichever side replies first. The loser
is told to retract its dialog. Neither side is privileged, and no configuration decides who
may answer.

**Failure is local-only.** The bridge is its own task with its own reconnect backoff. If
glance is down, unreachable, or killed mid-turn, the terminal session is unaffected — the
tee degrades to a plain move the moment the bridge's channel closes.

---

## The glance side

```
cmd/glance/main.go     serve | apikey add·list·rm | bootstrap | version
internal/
  acp/       JSON-RPC framing, the ACP subset glance needs, method predicates
  hub/       the live system: agents, browsers, ring buffer, interaction table
  auth/      TOTP enrollment and login, bootstrap gate, cookie signing
  state/     ~/.grok/glance/state.json (0600)
  httpapi/   routing, middleware, the two upgrades, the embedded SPA
web/         Vite + React 19 + Tailwind 4 + HeroUI 3
```

### hub — where everything meets

`Hub` owns two maps: connected agents by API-key id, and connected browsers. Everything
else is per-agent state on `Agent` (`internal/hub/agent.go`):

- `ring` — the replay buffer. Frames are stored **exactly as received**, bytes unchanged.
- `interactions` — open reverse-requests, keyed by grok's JSON-RPC id.
- `calls` — glance's own outstanding requests, keyed by id, each with a buffered reply
  channel so a late response never blocks the reader goroutine.
- `outbound` — a bounded queue drained by a single writer goroutine, because
  `coder/websocket` permits one concurrent writer and prompts, replies and pings all
  originate on different goroutines.

**Fan-out is unfiltered.** Every browser receives every agent's frames. At this scale
(a handful of sessions, a handful of viewers) per-browser filtering would be one more
thing to get wrong for no measurable gain; the frontend ignores frames for the agent it is
not showing.

**Backpressure is a disconnect, not a buffer.** A browser that cannot keep up fills its
256-frame queue and is dropped; it reconnects into a fresh snapshot. An unbounded queue
would turn one slow phone on hotel wifi into the server's memory problem, and a snapshot
is cheaper than the backlog it would have replayed anyway.

**Turn state is derived, not declared.** `classifyUpdate` reads a single field —
`update.sessionUpdate` — from either rail to decide whether a turn is running. Nothing
else in the xAI rail's payload is interpreted, because those ~60 variants are internal to
grok and drift with every upstream sync. The stable rail carries correctness; the xAI rail
is presentation.

### The permission race, end to end

```
grok ──▶ session/request_permission (id 7) ──▶ glance
                                                 │  ring? no. interactions[7] = …
                                                 └─▶ {"type":"interaction"} to all browsers

  … whichever happens first:

  browser answers  ──▶ Agent.Answer(7)  ──▶ JSON-RPC response id 7 ──▶ grok
                                        └──▶ {"type":"interaction_resolved", by:"browser"}

  terminal answers ──▶ grok sends x.ai/rc/interaction_cancelled(7)
                                        └──▶ {"type":"interaction_resolved", by:"terminal"}
```

`Agent.Answer` deletes from `interactions` under the lock and reports whether it was still
there. A `false` return is not an error — it is the ordinary outcome of losing the race —
and the browser that lost is sent `by:"elsewhere"` with *"already handled elsewhere"*.
That is why exactly one JSON-RPC response ever reaches grok for a given id, even when two
browsers and a terminal all click at once.

A browser may also **decline**, which replies to grok with a JSON-RPC *error*. grok reads
that as "glance is not answering this", leaves the terminal's dialog up, and the turn
proceeds when the user answers there. Declining is not denying — denying is an ordinary
answer with a `reject` option.

### Requests glance will not serve

grok drives nothing on this link. An inbound request that is not one of the three
interactions gets `-32601 Method not found`, including `fs/*` and `terminal/*`. glance has
no filesystem to offer, and a fabricated success would leave grok acting on a lie. (In its
default configuration the pager does not advertise those capabilities, so they should not
arrive at all; the arm exists because "should not" is not "cannot".)

---

## Browser protocol

One JSON object per WebSocket message, in both directions.

**Browser → server** (`internal/hub/browser.go`, `command`):

| `type` | Fields | Meaning |
|---|---|---|
| `list` | — | resend the agent list |
| `subscribe` | `agent` | send a full snapshot of one agent |
| `prompt` | `agent`, `text` | start a turn |
| `cancel` | `agent` | interrupt the running turn |
| `answer` | `agent`, `id`, `result` | answer an interaction |
| `decline` | `agent`, `id`, `reason` | hand it back to the terminal |

**Server → browser** (`Event`): `agents`, `snapshot`, `frame`, `interaction`,
`interaction_resolved`, `notice`, `error`.

Two details that matter:

- **`prompt` and `cancel` run detached.** A prompt does not return until the turn ends,
  which can be many minutes. Waiting inline would stall the browser's whole command
  stream — including the Stop button it might need next. They run on their own goroutine
  under `context.WithoutCancel`, and progress arrives as mirrored frames like anything else.
- **`snapshot` is authoritative.** It carries the ring, the open interactions, the session
  metadata and the turn state in one message. The frontend *replaces* state with it rather
  than merging, which is what makes a reload or a reconnect land on the server's view
  instead of a half-stale one.

---

## Authentication

There is no username and no password. There is one authenticator app and one server.

**Bootstrap.** On first `serve`, glance mints a one-time token, prints it with a setup URL,
and writes it to `~/.grok/glance/bootstrap.token` (0600) — stderr for the terminal case,
the file for the systemd case. `/api/setup/*` answers **404** unless the request carries a
valid, unused token, and 404 again once enrollment succeeds. Without that gate, the window
between "server starts" and "operator opens the browser" is a race anyone who can reach
the port may enter.

**Enrollment.** `/api/setup/begin` returns a candidate secret and `otpauth://` URI, which
the page renders as a QR code. Nothing is persisted until `/api/setup/complete` verifies a
code generated *from* that secret — a failed QR scan must not be able to lock the operator
out of their own server. Success stores the secret, burns the bootstrap token, and issues a
session cookie, because you have just proved you hold the authenticator and a login form
one second later would ask for the same proof.

**Login.** Six digits, ±1 time step for clock skew. Failures are rate-limited (8 per 5
minutes, counted globally — the limiter protects the secret, not a user account). Accepted
time steps are burned, so a code observed over a shoulder cannot be replayed inside its
30-second window.

**Session cookie.** `__Host-glance` — the prefix is a browser-enforced promise of Secure +
`Path=/` + no `Domain`, so a sibling host cannot set or overwrite it. The value is
`<expiry>.<hmac>`, signed with a key in `state.json`: no session table, so a restart does
not sign everyone out, and deleting `state.json` invalidates every outstanding cookie at
once. HttpOnly, SameSite=Strict, 12 hours.

**API keys.** `glance apikey add <name>` prints `glance_sk_…` once; only a SHA-256 hash is
stored. The key identifies one grok instance, and its id is the agent id — which is why
reconnecting with the same key *replaces* the previous connection instead of accumulating a
ghost session in the list.

### What this model does not protect against

Stated plainly, because the threat model is small on purpose:

- **glance is a remote control for a shell agent.** Anyone who can authenticate can approve
  arbitrary tool calls. It listens on `127.0.0.1` by default; exposing it should be a
  deliberate act, ideally behind a reverse proxy that terminates TLS.
- **No TLS of its own.** `--insecure-cookie` exists for plain-HTTP localhost and is refused
  on a non-loopback address, because a session cookie without `Secure` on a real network is
  a credential in cleartext.
- **No account recovery.** Losing the authenticator means deleting `state.json` — which
  also revokes every API key and session. `glance bootstrap` refuses to mint a second token
  once enrolled, since a re-enrollment path is exactly the door the bootstrap gate exists
  to keep shut.
- **One operator.** There are no roles, no audit log, and no per-key permissions.

### Content-Security-Policy

`default-src 'self'` with no `unsafe-inline` for scripts. glance renders agent output —
file contents, command output, model text — and none of it is trusted markup. Plan content
is rendered as preformatted text rather than parsed as Markdown for the same reason: it
avoids shipping a parser and a sanitiser to display text an agent wrote.

---

## Frontend

React 19 + HeroUI 3 (react-aria-components) + Tailwind 4, built by Vite into `web/dist`
and embedded with `//go:embed all:dist`.

- `App.tsx` gates on `/api/status` → **Setup** / **Login** / **Console**, and calls
  `useTheme` exactly once at the root (each call owns its own state — a second call would
  desync).
- `Console` owns the single `GlanceSocket`, so moving between the session list and a
  session neither drops the connection nor re-requests a snapshot.
- `lib/acp.ts` folds frames into a `Transcript` of seven item kinds (`user`, `assistant`,
  `thought`, `tool`, `plan`, `notice`, `turn-end`). Tool calls are updated in place by id,
  so a streaming `tool_call_update` refines the card that is already on screen.
- `components/PermissionDialog.tsx` renders all three interaction types as **cards, not
  modals** — a modal that closes itself when the terminal wins the race is more jarring
  than a card doing the same, and cards stack when several are open.

The response payloads it builds are pinned to grok's Rust types, not guessed:
`{"outcome":{"outcome":"selected","optionId":…}}` for permissions, `{"outcome":"approved"}`
for plan mode, and for questions an `{"outcome":"accepted","answers":…}` envelope whose map
is keyed by **question text**, in original order, values as arrays of labels, unanswered
questions omitted — the exact construction `xai-grok-pager/src/views/question_view.rs`
performs. Getting this wrong does not fail loudly; it fails as an agent that received an
answer to a question nobody asked.

An unrecognised interaction method renders its raw params and offers only *decline*, which
is the honest response to a request whose reply shape glance does not know.

---

## Testing

`internal/hub/hub_test.go` drives the hub over **real WebSocket connections** with a fake
grok and fake browsers. The behaviour that matters — an interaction reaching a browser, an
answer reaching grok, two sides racing — lives in the interleaving of three goroutines, and
a mocked transport would test the mock. It covers the permission round trip, terminal-wins
retraction, browser-vs-browser arbitration, decline, ring replay on subscribe, method-not-
found, reconnect-replaces-ghost.

`internal/httpapi/server_test.go` covers the auth boundary: the bootstrap 404, enrollment,
rate limiting, cookie forgery, both upgrades, and the SPA fallback's refusal to swallow
`/api/*`.

What tests cannot cover is the race against a *real* terminal. The end-to-end checklist in
`CLAUDE.md` is not optional.
