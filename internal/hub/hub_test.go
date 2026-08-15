package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/user/grok-glance/internal/acp"
)

// These tests drive the hub over real WebSocket connections rather than mocking
// the transport. The behaviour that matters here -- an interaction reaching a
// browser, an answer reaching grok, and the two sides racing -- lives in the
// interleaving of three goroutines, and a mocked conn would test the mock.

const testTimeout = 5 * time.Second

// link is a hub with an HTTP front door for both socket kinds.
type link struct {
	hub    *Hub
	server *httptest.Server
	url    string
}

func newLink(t *testing.T) *link {
	t.Helper()
	l := &link{hub: New(slog.New(slog.DiscardHandler))}

	l.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		conn.SetReadLimit(8 << 20)
		if strings.HasPrefix(r.URL.Path, "/agent") {
			l.hub.ServeAgent(r.Context(), conn, r.URL.Query().Get("id"), "laptop")
			return
		}
		l.hub.ServeBrowser(r.Context(), conn)
	}))
	t.Cleanup(l.server.Close)
	l.url = "ws" + strings.TrimPrefix(l.server.URL, "http")
	return l
}

// fakeAgent stands in for the grok bridge.
//
// Its reader answers `initialize` by itself, the way the real bridge does, so
// tests do not have to step through a handshake they are not about.
type fakeAgent struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan acp.Frame
	ready  chan struct{}
}

func (l *link) dialAgent(t *testing.T, id string, meta acp.SessionMeta) *fakeAgent {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, l.url+"/agent?id="+id, nil)
	if err != nil {
		t.Fatalf("dial agent: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	a := &fakeAgent{t: t, conn: conn, frames: make(chan acp.Frame, 64), ready: make(chan struct{})}
	go a.read(meta)

	// Wait for the handshake so that tests which read the agent list do not race
	// the metadata that names the session.
	select {
	case <-a.ready:
	case <-time.After(testTimeout):
		t.Fatal("hub never sent initialize")
	}
	return a
}

func (a *fakeAgent) read(meta acp.SessionMeta) {
	ctx := context.Background()
	handshake := false
	for {
		_, data, err := a.conn.Read(ctx)
		if err != nil {
			close(a.frames)
			return
		}
		var frame acp.Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		if frame.Method == acp.MethodInitialize {
			reply, err := acp.NewResponse(frame.ID, acp.InitializeResult{
				ProtocolVersion: 1,
				Meta:            &acp.InitializeMeta{Session: meta},
			})
			if err == nil {
				a.write(reply)
			}
			if !handshake {
				handshake = true
				close(a.ready)
			}
			continue
		}
		select {
		case a.frames <- frame:
		default:
		}
	}
}

func (a *fakeAgent) write(frame any) {
	a.t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		a.t.Errorf("encode frame: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := a.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		a.t.Errorf("write frame: %v", err)
	}
}

// raw writes a frame written out as JSON, for shapes the Go types do not model.
func (a *fakeAgent) raw(body string) {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := a.conn.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
		a.t.Errorf("write raw: %v", err)
	}
}

// expect waits for the next frame satisfying match.
func (a *fakeAgent) expect(match func(acp.Frame) bool, what string) acp.Frame {
	a.t.Helper()
	deadline := time.After(testTimeout)
	for {
		select {
		case frame, ok := <-a.frames:
			if !ok {
				a.t.Fatalf("agent socket closed while waiting for %s", what)
			}
			if match(frame) {
				return frame
			}
		case <-deadline:
			a.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// fakeBrowser stands in for the web UI.
type fakeBrowser struct {
	t      *testing.T
	conn   *websocket.Conn
	events chan Event
}

func (l *link) dialBrowser(t *testing.T) *fakeBrowser {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, l.url+"/browser", nil)
	if err != nil {
		t.Fatalf("dial browser: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	b := &fakeBrowser{t: t, conn: conn, events: make(chan Event, 256)}
	go b.read()
	return b
}

func (b *fakeBrowser) read() {
	ctx := context.Background()
	for {
		_, data, err := b.conn.Read(ctx)
		if err != nil {
			close(b.events)
			return
		}
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		select {
		case b.events <- ev:
		default:
		}
	}
}

func (b *fakeBrowser) send(cmd command) {
	b.t.Helper()
	raw, err := json.Marshal(cmd)
	if err != nil {
		b.t.Fatalf("encode command: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := b.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		b.t.Fatalf("write command: %v", err)
	}
}

// expect waits for the next event of the given type, skipping the agent-list
// churn that connects and disconnects produce.
func (b *fakeBrowser) expect(kind EventType) Event {
	b.t.Helper()
	deadline := time.After(testTimeout)
	for {
		select {
		case ev, ok := <-b.events:
			if !ok {
				b.t.Fatalf("browser socket closed while waiting for %s", kind)
			}
			if ev.Type == kind {
				return ev
			}
		case <-deadline:
			b.t.Fatalf("timed out waiting for a %s event", kind)
		}
	}
}

// permissionRequest is what grok asks when a tool needs approval.
func permissionRequest(id int, toolCallID string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  acp.MethodRequestPermission,
		"params": map[string]any{
			"sessionId":  "s-1",
			"toolCallId": toolCallID,
			"options": []map[string]string{
				{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
				{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
			},
		},
	}
}

func TestAgentConnectAnnouncesItselfWithSessionMetadata(t *testing.T) {
	l := newLink(t)
	browser := l.dialBrowser(t)
	browser.expect(EventAgents) // the empty greeting

	l.dialAgent(t, "key-1", acp.SessionMeta{
		SessionID: "s-1",
		CWD:       "/home/user/project",
		Model:     "grok-4",
		Hostname:  "workstation",
	})

	// The list is broadcast on connect and again when initialize answers, so the
	// named entry may be the second event, not the first.
	deadline := time.After(testTimeout)
	for {
		ev := browser.expect(EventAgents)
		if len(ev.Agents) == 1 && ev.Agents[0].Session.Model == "grok-4" {
			if got := ev.Agents[0].Label; got != "/home/user/project" {
				t.Fatalf("label = %q, want the cwd when there is no title", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("agent never appeared with its session metadata")
		default:
		}
	}
}

func TestBrowserAnswerReachesGrokAndClosesEveryDialog(t *testing.T) {
	l := newLink(t)
	agent := l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-1"})
	browser := l.dialBrowser(t)
	browser.expect(EventAgents)

	agent.write(permissionRequest(7, "call-1"))

	opened := browser.expect(EventInteraction)
	if opened.Interaction == nil || opened.Interaction.Method != acp.MethodRequestPermission {
		t.Fatalf("interaction = %+v, want a permission request", opened.Interaction)
	}
	if opened.Interaction.ToolCallID != "call-1" {
		t.Fatalf("toolCallId = %q, want call-1", opened.Interaction.ToolCallID)
	}
	// The params are passed through untouched: glance renders nothing from them
	// itself, so anything it rewrote would be a bug the UI inherits.
	if !strings.Contains(string(opened.Interaction.Params), `"allow_once"`) {
		t.Fatalf("params were not passed through: %s", opened.Interaction.Params)
	}

	id := string(opened.Interaction.ID)
	browser.send(command{
		Type:   "answer",
		Agent:  "key-1",
		ID:     id,
		Result: json.RawMessage(`{"outcome":{"outcome":"selected","optionId":"allow"}}`),
	})

	// grok gets a JSON-RPC response on the id it asked with.
	reply := agent.expect(func(f acp.Frame) bool {
		return f.Kind() == acp.KindResponse && string(f.ID) == "7"
	}, "the permission response")
	if !strings.Contains(string(reply.Result), `"optionId":"allow"`) {
		t.Fatalf("result = %s, want the browser's choice", reply.Result)
	}

	resolved := browser.expect(EventInteractionResolved)
	if resolved.ID != id || resolved.By != "browser" {
		t.Fatalf("resolution = %+v, want id %s by browser", resolved, id)
	}

	// The interaction is gone, so a reload does not show a dialog grok is no
	// longer waiting on.
	if pending := l.hub.Summaries()[0].Pending; pending != 0 {
		t.Fatalf("pending = %d after answering, want 0", pending)
	}
}

func TestTerminalWinningRetractsTheBrowserDialog(t *testing.T) {
	l := newLink(t)
	agent := l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-1"})
	browser := l.dialBrowser(t)
	browser.expect(EventAgents)

	agent.write(permissionRequest(11, "call-2"))
	opened := browser.expect(EventInteraction)

	// The user approved in the terminal, so the bridge tells glance to take the
	// card down. glance must not answer grok afterwards: grok already has it.
	agent.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  acp.MethodRCInteractionCancelled,
		"params":  acp.InteractionCancelledParams{ID: 11, ToolCallID: "call-2"},
	})

	resolved := browser.expect(EventInteractionResolved)
	if resolved.By != "terminal" {
		t.Fatalf("resolved by %q, want terminal", resolved.By)
	}
	if resolved.ID != string(opened.Interaction.ID) {
		t.Fatalf("resolved id = %q, want %q", resolved.ID, opened.Interaction.ID)
	}

	// Answering now is the losing half of the race and must say so rather than
	// fail: the browser had the card open when the terminal won.
	browser.send(command{
		Type:   "answer",
		Agent:  "key-1",
		ID:     resolved.ID,
		Result: json.RawMessage(`{"outcome":{"outcome":"selected","optionId":"allow"}}`),
	})
	late := browser.expect(EventInteractionResolved)
	if late.By != "elsewhere" || late.Message == "" {
		t.Fatalf("late answer = %+v, want by=elsewhere with an explanation", late)
	}
}

func TestOnlyTheFirstBrowserToAnswerWins(t *testing.T) {
	l := newLink(t)
	agent := l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-1"})
	first := l.dialBrowser(t)
	second := l.dialBrowser(t)
	first.expect(EventAgents)
	second.expect(EventAgents)

	agent.write(permissionRequest(13, "call-3"))
	opened := first.expect(EventInteraction)
	second.expect(EventInteraction)
	id := string(opened.Interaction.ID)

	answer := command{
		Type:   "answer",
		Agent:  "key-1",
		ID:     id,
		Result: json.RawMessage(`{"outcome":{"outcome":"selected","optionId":"allow"}}`),
	}
	first.send(answer)
	if got := first.expect(EventInteractionResolved); got.By != "browser" {
		t.Fatalf("first answer resolved by %q, want browser", got.By)
	}

	// The resolution is broadcast, so the other browser's card closes on its own
	// without anyone touching it.
	if got := second.expect(EventInteractionResolved); got.By != "browser" || got.ID != id {
		t.Fatalf("second browser saw %+v, want the first browser's resolution", got)
	}

	// Answering anyway -- the click that was already in flight -- is told what
	// happened rather than failing.
	second.send(answer)
	if got := second.expect(EventInteractionResolved); got.By != "elsewhere" {
		t.Fatalf("second answer resolved by %q, want elsewhere", got.By)
	}

	// One response, not two: grok is waiting on a single id and a duplicate
	// would be an unsolicited frame.
	agent.expect(func(f acp.Frame) bool {
		return f.Kind() == acp.KindResponse && string(f.ID) == "13"
	}, "the permission response")
	select {
	case frame, ok := <-agent.frames:
		if ok && frame.Kind() == acp.KindResponse && string(frame.ID) == "13" {
			t.Fatal("the losing browser's answer was also sent to grok")
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDeclineHandsTheInteractionBackToTheTerminal(t *testing.T) {
	l := newLink(t)
	agent := l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-1"})
	browser := l.dialBrowser(t)
	browser.expect(EventAgents)

	agent.write(permissionRequest(17, "call-4"))
	opened := browser.expect(EventInteraction)

	browser.send(command{
		Type:   "decline",
		Agent:  "key-1",
		ID:     string(opened.Interaction.ID),
		Reason: "answering in the terminal",
	})

	// A JSON-RPC error, not a fabricated outcome: grok reads it as "glance is
	// not answering this" and leaves the terminal's dialog up.
	reply := agent.expect(func(f acp.Frame) bool {
		return f.Kind() == acp.KindResponse && string(f.ID) == "17"
	}, "the decline")
	if reply.Error == nil {
		t.Fatalf("decline produced result %s, want a JSON-RPC error", reply.Result)
	}
	if !strings.Contains(reply.Error.Message, "answering in the terminal") {
		t.Fatalf("error message = %q, want the browser's reason", reply.Error.Message)
	}
}

func TestTranscriptIsRingedAndReplayedOnSubscribe(t *testing.T) {
	l := newLink(t)
	agent := l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-1"})

	// One frame from each rail. The xAI rail is opaque to glance but must still
	// reach the browser, or the transcript loses its streaming detail.
	agent.raw(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s-1",
	  "update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}},
	  "_meta":{"eventId":"e-1"}}}`)
	agent.raw(`{"jsonrpc":"2.0","method":"x.ai/session_notification","params":{"sessionId":"s-1",
	  "update":{"sessionUpdate":"tool_call_update","tool_call_id":"call-9","status":"in_progress"}}}`)

	browser := l.dialBrowser(t)
	browser.expect(EventAgents)

	// Subscribing late still renders the turn so far -- that is what the ring is
	// for, and what makes a page reload mid-turn survivable.
	waitFor(t, func() bool { return l.hub.Summaries()[0].Frames == 2 })
	browser.send(command{Type: "subscribe", Agent: "key-1"})

	snapshot := browser.expect(EventSnapshot)
	if len(snapshot.Frames) != 2 {
		t.Fatalf("snapshot has %d frames, want 2", len(snapshot.Frames))
	}
	if snapshot.Dropped != 0 {
		t.Fatalf("dropped = %d, want 0", snapshot.Dropped)
	}
	// A chunk means a turn is running; the UI's spinner is driven by this.
	if !snapshot.TurnActive {
		t.Fatal("turnActive = false after a message chunk")
	}
	// `_meta` is what lets a viewer dedup and order the stream, so it has to
	// survive the round trip verbatim.
	if !strings.Contains(string(snapshot.Frames[0]), `"eventId":"e-1"`) {
		t.Fatalf("frame lost its _meta: %s", snapshot.Frames[0])
	}

	// A live frame arrives on the same socket after the snapshot.
	agent.raw(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s-1",
	  "update":{"sessionUpdate":"turn_completed"}}}`)
	live := browser.expect(EventFrame)
	if !strings.Contains(string(live.Frame), "turn_completed") {
		t.Fatalf("live frame = %s, want the turn_completed update", live.Frame)
	}
	waitFor(t, func() bool { return !l.hub.Summaries()[0].TurnActive })
}

func TestUnsupportedRequestsGetMethodNotFound(t *testing.T) {
	l := newLink(t)
	agent := l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-1"})

	// glance drives grok, not the other way round: it has no filesystem to
	// offer. Saying so beats a fabricated success grok would then act on.
	agent.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      21,
		"method":  "fs/read_text_file",
		"params":  map[string]any{"path": "/etc/passwd"},
	})

	reply := agent.expect(func(f acp.Frame) bool {
		return f.Kind() == acp.KindResponse && string(f.ID) == "21"
	}, "the method-not-found reply")
	if reply.Error == nil || reply.Error.Code != acp.CodeMethodNotFound {
		t.Fatalf("reply = %+v, want a method-not-found error", reply)
	}
}

func TestCommandsForAMissingAgentAreReportedNotDropped(t *testing.T) {
	l := newLink(t)
	browser := l.dialBrowser(t)
	browser.expect(EventAgents)

	for _, cmd := range []command{
		{Type: "subscribe", Agent: "ghost"},
		{Type: "prompt", Agent: "ghost", Text: "hi"},
		{Type: "cancel", Agent: "ghost"},
		{Type: "answer", Agent: "ghost", ID: "1", Result: json.RawMessage(`{}`)},
	} {
		browser.send(cmd)
		if ev := browser.expect(EventError); !strings.Contains(ev.Message, "no such agent") {
			t.Fatalf("%s: message = %q, want a missing-agent error", cmd.Type, ev.Message)
		}
	}

	browser.send(command{Type: "nonsense"})
	if ev := browser.expect(EventError); !strings.Contains(ev.Message, "unknown command") {
		t.Fatalf("message = %q, want an unknown-command error", ev.Message)
	}
}

func TestAgentDisconnectLeavesNoGhost(t *testing.T) {
	l := newLink(t)
	agent := l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-1"})
	waitFor(t, func() bool { return len(l.hub.Summaries()) == 1 })

	agent.conn.Close(websocket.StatusNormalClosure, "bye")

	// A stale entry would show in the UI as a session that never updates again.
	waitFor(t, func() bool { return len(l.hub.Summaries()) == 0 })
}

func TestReconnectWithTheSameKeyReplacesTheOldConnection(t *testing.T) {
	l := newLink(t)
	l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-1"})
	waitFor(t, func() bool { return len(l.hub.Summaries()) == 1 })

	// The bridge reconnects after every network blip. Accumulating a second
	// entry per blip would fill the session list with dead sessions.
	l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-2"})
	waitFor(t, func() bool {
		summaries := l.hub.Summaries()
		return len(summaries) == 1 && summaries[0].Session.SessionID == "s-2"
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

// The prompt box and the Stop button, which are the two things the browser can
// do to a turn. Both are dispatched on their own goroutine -- a prompt does not
// return until the turn ends -- so this also checks that a browser can still be
// heard while one is outstanding.
func TestPromptAndStopReachGrok(t *testing.T) {
	l := newLink(t)
	agent := l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-1", CWD: "/repo"})
	browser := l.dialBrowser(t)

	browser.send(command{Type: "prompt", Agent: "key-1", Text: "summarise the diff"})

	prompt := agent.expect(func(f acp.Frame) bool {
		return f.Kind() == acp.KindRequest && f.Method == acp.MethodSessionPrompt
	}, "the prompt")
	var got acp.PromptParams
	if err := json.Unmarshal(prompt.Params, &got); err != nil {
		t.Fatalf("decode prompt params: %v", err)
	}
	if got.Text != "summarise the diff" {
		t.Fatalf("prompt text = %q, want the browser's text", got.Text)
	}
	if got.SessionID != "s-1" {
		t.Fatalf("prompt sessionId = %q, want the mirrored session", got.SessionID)
	}

	// The turn is now running and grok has not answered the prompt. Stop must
	// still get through rather than queueing behind it.
	browser.send(command{Type: "cancel", Agent: "key-1"})

	cancel := agent.expect(func(f acp.Frame) bool {
		return f.Kind() == acp.KindRequest && f.Method == acp.MethodSessionCancel
	}, "the cancel")
	var cancelled acp.CancelParams
	if err := json.Unmarshal(cancel.Params, &cancelled); err != nil {
		t.Fatalf("decode cancel params: %v", err)
	}
	if cancelled.SessionID != "s-1" {
		t.Fatalf("cancel sessionId = %q, want the mirrored session", cancelled.SessionID)
	}

	// Answering both keeps the agent's call table clean, which is what the next
	// prompt depends on.
	agent.write(map[string]any{"jsonrpc": "2.0", "id": cancel.ID, "result": map[string]any{}})
	agent.write(map[string]any{
		"jsonrpc": "2.0", "id": prompt.ID,
		"result": map[string]any{"stopReason": "cancelled"},
	})

	browser.send(command{Type: "prompt", Agent: "key-1", Text: "again"})
	agent.expect(func(f acp.Frame) bool {
		return f.Kind() == acp.KindRequest && f.Method == acp.MethodSessionPrompt &&
			string(f.ID) != string(prompt.ID)
	}, "a second prompt")
}

// An empty prompt is the accidental Enter in an empty box. It must not reach
// grok and start a turn nobody asked for.
func TestEmptyPromptsAreRefusedLocally(t *testing.T) {
	l := newLink(t)
	agent := l.dialAgent(t, "key-1", acp.SessionMeta{SessionID: "s-1"})
	browser := l.dialBrowser(t)

	browser.send(command{Type: "prompt", Agent: "key-1", Text: "   "})
	if ev := browser.expect(EventError); ev.Message == "" {
		t.Fatal("an empty prompt should be reported to the browser")
	}

	// Nothing reached grok: a real prompt afterwards is the first one it sees.
	browser.send(command{Type: "prompt", Agent: "key-1", Text: "real"})
	prompt := agent.expect(func(f acp.Frame) bool {
		return f.Kind() == acp.KindRequest && f.Method == acp.MethodSessionPrompt
	}, "the prompt")
	var got acp.PromptParams
	if err := json.Unmarshal(prompt.Params, &got); err != nil {
		t.Fatalf("decode prompt params: %v", err)
	}
	if got.Text != "real" {
		t.Fatalf("prompt text = %q, want the first prompt grok sees to be the real one", got.Text)
	}
}
