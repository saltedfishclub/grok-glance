# CLAUDE.md — working on grok-glance

grok-glance is the web control plane for grok's `/rc` remote control. Read
[ARCHITECTURE.md](ARCHITECTURE.md) first — it explains *why* the pieces are shaped the way
they are. This file is the operational half: how to build it, how to test it, and the
handful of things that will waste an afternoon if you learn them the hard way.

The Rust half lives in the sibling `newgrok/` tree as patches `0003`/`0004`. **The two are
one feature.** A change to the wire format is a change to both.

---

## Layout

```
cmd/glance/main.go     serve | apikey add·list·rm | bootstrap | version
cmd/fakeagent/main.go  a fake grok, for UI work without rebuilding Rust
internal/
  acp/       JSON-RPC framing + the ACP subset glance needs   ~280 lines
  hub/       agents, browsers, ring buffer, interaction table  the live system
  auth/      TOTP, bootstrap gate, signed cookies
  state/     ~/.grok/glance/state.json (0600)
  httpapi/   chi router, both WS upgrades, embedded SPA
web/src/
  App.tsx           status gate: Setup / Login / Console
  pages/            Setup, Login, Sessions, Session
  components/       Transcript, ToolCall, PermissionDialog, PromptBox, TurnStatus
  lib/              api.ts (REST), ws.ts (GlanceSocket), acp.ts (frames → transcript)
```

If you are looking for where a decision lives:

| Question | File |
|---|---|
| What does the browser send/receive? | `internal/hub/browser.go` |
| What happens when two people click Allow? | `internal/hub/agent.go` → `Answer`, `retract` |
| Which methods does glance refuse? | `internal/hub/agent.go` → `handleFrame` |
| Is this frame part of the transcript? | `internal/acp/jsonrpc.go` → `IsTranscript` |
| Why is `/setup` a 404? | `internal/auth/auth.go` → `BootstrapToken` |
| How does a frame become a bubble? | `web/src/lib/acp.ts` → `applyFrame` |

---

## Build and run

```sh
make build        # frontend, then binary → bin/glance
make server       # binary only, keeping the last frontend build (fast server loop)
make web          # frontend only
make check        # go vet + go test + tsc --noEmit
make dev          # Go server + Vite with hot reload → http://localhost:5173
```

`//go:embed all:dist` resolves at compile time, so **`go build` ships whatever `make web`
last produced**. If a UI change does not appear in `bin/glance`, that is why.

Use `make dev` for frontend work: Vite proxies `/api` to the Go server, which is what keeps
the `__Host-glance` cookie working — it is `SameSite=Strict` and would never survive a
cross-origin request. Hitting the Go port directly on 7717 with the Vite UI will look like
a mysterious auth failure.

### First run

```sh
make build
bin/glance serve --addr 127.0.0.1:7717 --insecure-cookie
#   → prints a bootstrap URL; open it, scan the QR, enter one code
bin/glance apikey add dev
#   → prints glance_sk_… once, plus a [remote_control] block for ~/.grok/config.toml
```

`--insecure-cookie` drops the `Secure` attribute so a plain-HTTP localhost session works.
It is refused on a non-loopback address, deliberately. In production put glance behind a
TLS-terminating proxy and leave the flag off.

Lost the authenticator? Delete `~/.grok/glance/state.json` and start over. That is the
whole recovery story, on purpose — see ARCHITECTURE.md. **`~/.grok/glance/` also contains
`secret.key` and `hook.secret` that belong to something else entirely. Do not touch them.**

---

## Testing without rebuilding grok

`cmd/fakeagent` dials the agent socket exactly as the real bridge does and plays a scripted
turn: streamed text, a thought, a plan, a tool call, and a **real**
`session/request_permission` that waits for a real answer.

```sh
bin/glance serve --insecure-cookie &
make fakeagent KEY=glance_sk_…            # or: go run ./cmd/fakeagent --key …
```

Then in the browser: prompt it, watch it stream, answer the permission. Useful flags:

- `--terminal-after 3s` — the "terminal" answers the permission first, so you can watch the
  browser's card retract by itself. This is the path a browser alone cannot exercise.
- `--speed 1s` — slow the stream down to catch layout problems mid-turn.
- Send the prompt and hit **Stop**: the fake agent honours `session/cancel` and finishes the
  turn with `stopReason: cancelled`.

Run several at once with different `--title` to test the session list.

It emits `turn_completed` on the **xAI rail** (`x.ai/session_notification`) on purpose: if
the turn stops showing as finished in the UI, rail mirroring has regressed. That is the
most likely thing to break silently after an upstream sync.

What fakeagent does *not* prove is that the real bridge speaks this dialect. Only the
end-to-end checklist does.

---

## Where the ACP types come from

There is no Go SDK for ACP. `internal/acp` is hand-written against
[`agentclientprotocol/agent-client-protocol`](https://github.com/agentclientprotocol/agent-client-protocol)
(`schema/v1/schema.json`), and it is thin on purpose — glance correlates ids, recognises a
dozen methods, and passes payloads through to the browser as `json.RawMessage`.

**Do not "finish" it by modelling every update variant.** The stable `session/update` rail
is a small closed set; the xAI rail has ~60 grok-internal variants that drift with every
upstream sync. Typed structs for those would be a large amount of code whose only effect is
to turn an upstream rename into a parse error that kills a live connection. The contract is:

- **The stable rail carries correctness.** Turn state comes from `update.sessionUpdate`,
  read by `classifyUpdate` in `internal/hub/agent.go` — one field, both rails.
- **The xAI rail is presentation.** Unrecognised variants are stored, forwarded, and
  skipped by the renderer. Never an error.
- **`_meta` is forwarded byte-for-byte.** `eventId`, `promptId`, `chunkId` and `isReplay`
  are how a viewer dedups and orders; rewriting the envelope would break replay.

To regenerate anything, clone the spec repo and read `schema/v1/schema.json`. There is no
codegen step and adding one would be a mistake at this size.

### Response shapes are pinned to grok's Rust types, not to the spec

The three interaction replies are built in `web/src/components/PermissionDialog.tsx`, and
their exact shapes were read off grok's source, not guessed:

| Interaction | Reply |
|---|---|
| `session/request_permission` | `{"outcome":{"outcome":"selected","optionId":"…"}}`, or `{"outcome":{"outcome":"cancelled"}}` |
| `x.ai/exit_plan_mode` | `{"outcome":"approved"}`, `{"outcome":"cancelled","feedback":"…"}`, `{"outcome":"abandoned"}` |
| `x.ai/ask_user_question` | `{"outcome":"accepted","answers":{"<question text>":["<label>"]}}`, plus optional `annotations` |

`ask_user_question` is the fiddly one, and `buildAccepted` reproduces
`xai-grok-pager/src/views/question_view.rs` rule for rule: values are **arrays** of labels;
the map is keyed by question **text** in the original order; unanswered questions are
**omitted** rather than sent empty; a freeform-only answer is the literal `["Other"]` with
the typed text in `annotations[q].notes`; `preview` rides along only for single-select.

Getting these wrong does not fail loudly. It produces an agent that acts on an answer to a
question nobody asked. If you change one, change it in `newgrok/` too and re-run the
end-to-end checklist.

---

## Conventions

**Go.** Standard library plus chi, coder/websocket, pquerna/otp — that is the whole
dependency list and it should stay that way. `slog` for logging, never `fmt.Println`.
Comments explain *why*; the code already says what. Errors reaching a browser go through
`Event{Type: EventError}` so the UI can show them rather than the socket dying quietly.

**One writer per socket.** `coder/websocket` allows a single concurrent writer, so every
connection has exactly one writer goroutine draining a bounded queue. If you need to send
from a new place, push to `outbound`/`send`; do not call `conn.Write` directly.

**Backpressure is a disconnect.** A full queue drops the connection and the client
reconnects into a fresh snapshot. Do not "fix" this by growing the buffer — the snapshot is
cheaper than the backlog it would replay.

**Nothing blocks the reader goroutine.** Anything slow (a prompt, a cancel) runs detached
under `context.WithoutCancel`. A prompt can take minutes; waiting inline would freeze the
Stop button that is meant to end it.

**Frontend.** HeroUI 3 sits on react-aria-components: buttons take `onPress`, not
`onClick`, and there is no provider component to wrap. Tailwind 4 is CSS-first — there is
**no `tailwind.config.js`**; theme tokens and `@source "./"` live in `web/src/index.css`.
Use the semantic token classes already in use rather than arbitrary `bg-[var(--x)]` values.

**Never render agent output as markup.** Plans, tool output and model text are printed as
text. The CSP is `default-src 'self'` with no `unsafe-inline` for scripts; keep it that way.

**`useTheme` is called exactly once,** at the root in `App.tsx`. Each call owns its own
state, so a second call silently desyncs the toggle.

---

## Tests

```sh
go test ./...              # all five internal packages
go test -race ./internal/hub -run TestOnlyTheFirst -count=3
```

`internal/hub/hub_test.go` drives the hub over **real WebSocket connections** with a fake
grok and fake browsers, because the behaviour under test is the interleaving of three
goroutines and a mocked transport would test the mock. If you add a command or an event,
add it there.

Two things to know before you write a hub test:

- `interaction_resolved` is **broadcast to every browser**. A test with two browsers must
  consume the broadcast on the second one before asserting anything about its own reply, or
  it will read the first browser's resolution and report a confusing failure.
- Agent-list churn arrives whenever a connection comes or goes. `fakeBrowser.expect` skips
  it; assert on the event kind you care about, not on message order.

`internal/httpapi/server_test.go` covers the auth boundary: the bootstrap 404, enrollment,
rate limiting, cookie forgery, both upgrades, and the SPA fallback's refusal to swallow
`/api/*`.

---

## End-to-end checklist

Unit tests cannot cover the race against a real terminal. Run this against a real grok with
`/rc` on before shipping any change to the bridge, the interaction path, or the wire format.

1. `glance serve` → bootstrap → enroll TOTP → `glance apikey add laptop`.
2. Put the printed `[remote_control]` block in `~/.grok/config.toml`.
3. Start grok, type `/rc` → a system block confirms connected, **and the TUI stays fully
   usable**. This is the whole premise of the feature.
4. Send a prompt from the browser → it appears in the terminal and streams to both.
5. Trigger a tool needing approval → approve in the **browser** → the terminal's modal
   closes by itself.
6. Same again, approving in the **terminal** → the browser's card closes by itself.
7. Repeat 5–6 for `x.ai/ask_user_question` and `x.ai/exit_plan_mode`. All three race through
   the same path, but only permissions get exercised by accident.
8. Start a long turn, press **Stop** in the browser → the turn aborts, and the session log
   attributes it to `client:glance`, not to Esc.
9. Confirm tool-call deltas and `turn_completed` reach the browser — proof the xAI rail is
   mirrored and not just `session/update`.
10. **Kill glance mid-turn.** The TUI must keep working; the bridge reconnects with backoff
    and replays. Remote control must never be able to take the local session down with it.

Step 10 is the one that matters most.

---

## Working on the Rust half

In `newgrok/`, and read its `CLAUDE.md` first. The short version:

- `patches/` is the source of truth and is tracked; `work/` is disposable build output.
  **Edit in `work/`, commit there, then `make rebuild` to export patches.** Never edit a
  `.patch` by hand.
- **`make apply` does `git reset --hard` + `git clean -fdx` in `work/`.** Running it with
  uncommitted work destroys that work with no warning.
- The root `Cargo.toml` is generated. Treat it as read-only; per-crate manifests take
  `workspace = true` deps.
- Cargo on this box is I/O-heavy enough to disturb the machine. Build throttled and in the
  foreground:
  ```sh
  nice -n 19 env CARGO_CMD=test PKG=xai-grok-pager ./scripts/build.sh --lib -j 2
  ```
  `cargo` is only on `PATH` via `scripts/lib.sh`, so go through `scripts/build.sh`. Use
  module-qualified test filters (`rc::`, `slash::commands::rc`) — a bare `rc` matches
  hundreds of unrelated tests.
- `doctor_cmd::tests::fake_standalone_facts_compose_through_shared_view` fails on a clean
  upstream tree. It is not yours.
- Adding a pager slash command requires listing it in `xai-grok-shell`'s
  `PAGER_COMMAND_KEYS`, or `pager_builtin_triggers_are_reserved_in_shell` fails.

The RC code is `xai-grok-pager/src/rc/` (`mod`, `protocol`, `ring`, `tee`, `transport`) plus
`slash/commands/rc.rs` and `app/dispatch/rc.rs`. `tee.rs` is the interesting one: it is a
single interception point on the pager's ACP channel, and keeping it that way is what keeps
upstream conflicts survivable.

---

## Things that will bite you

- **`vite build` empties `web/dist/`,** taking `.gitkeep` with it — and `web/embed.go` needs
  a file there or `go build` fails on a clean clone with an unhelpful embed error. The
  `web` and `clean` targets restore it; a bare `npm run build` does not.
- **`__Host-` is a browser-enforced contract**: Secure, `Path=/`, no `Domain`. Change any of
  those and the browser silently discards the cookie, which looks exactly like a broken
  login.
- **A used TOTP step is burned.** Logging in twice inside one 30-second window fails the
  second time. That is the replay defence, not a bug.
- **The rate limiter is global, not per-user** — there are no users. Eight failures in five
  minutes locks out *everyone*, including a correct code. Tests must account for it.
- **Reconnecting with the same API key replaces the previous connection.** Two grok
  instances sharing one key will fight over the slot; give each its own.
- **A `false` return from `Answer`/`Decline` is not an error.** It is the ordinary outcome
  of losing the race, and it must produce `by:"elsewhere"`, not a 500.
