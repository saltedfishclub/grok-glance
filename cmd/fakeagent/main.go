// Command fakeagent impersonates grok's `/rc` bridge so the glance server and
// web UI can be developed without rebuilding grok.
//
// Rebuilding the Rust side is a multi-minute cargo run through a patch tree,
// which is a poor inner loop for "does this tool card wrap correctly". This
// dials the agent socket exactly as the real bridge does, answers `initialize`
// with session metadata, and runs a scripted turn on every prompt: streamed
// text on both notification rails, a thought, a plan, a tool call, and a real
// `session/request_permission` that waits for a genuine answer.
//
//	glance apikey add dev            # prints glance_sk_...
//	go run ./cmd/fakeagent --key glance_sk_...
//
// It is a development aid, not a test fixture -- the tests in internal/hub have
// their own in-process fake. What this adds is a browser you can click.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/user/grok-glance/internal/acp"
)

func main() {
	url := flag.String("url", "ws://127.0.0.1:7717/api/acp/agent", "glance agent socket")
	key := flag.String("key", os.Getenv("GLANCE_API_KEY"), "API key from `glance apikey add` (or $GLANCE_API_KEY)")
	title := flag.String("title", "fake session", "session title shown in the UI")
	model := flag.String("model", "grok-4-fake", "model name shown in the UI")
	cwd := flag.String("cwd", mustCwd(), "working directory shown in the UI")
	// Simulates the terminal answering a permission first, which is the one
	// path a browser alone cannot exercise: the card must retract by itself.
	terminalAfter := flag.Duration("terminal-after", 0, "answer permissions from the `terminal` after this delay (0 = never)")
	speed := flag.Duration("speed", 220*time.Millisecond, "delay between streamed chunks")
	flag.Parse()

	if *key == "" {
		fmt.Fprintln(os.Stderr, "fakeagent: --key is required (run `glance apikey add dev`)")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := &agent{
		out:     make(chan acp.Frame, 64),
		pending: make(map[string]chan acp.Frame),
		meta: acp.SessionMeta{
			SessionID: "fake-session-1",
			CWD:       *cwd,
			Title:     *title,
			Model:     *model,
			Hostname:  hostname(),
			Version:   "fakeagent",
		},
		terminalAfter: *terminalAfter,
		speed:         *speed,
	}

	if err := a.run(ctx, *url, *key); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("fakeagent: %v", err)
	}
}

type agent struct {
	conn *websocket.Conn
	out  chan acp.Frame

	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan acp.Frame // our requests, awaiting glance's answer
	cancel  context.CancelFunc        // cancels the turn in flight, if any

	meta          acp.SessionMeta
	terminalAfter time.Duration
	speed         time.Duration
}

func (a *agent) run(ctx context.Context, url, key string) error {
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + key}},
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("glance rejected the API key (%s)", resp.Status)
		}
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8 << 20)
	a.conn = conn

	log.Printf("connected to %s as %q", url, a.meta.Label())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.writeLoop(ctx)

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var frame acp.Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			log.Printf("undecodable frame: %v", err)
			continue
		}
		a.handle(ctx, frame)
	}
}

func (a *agent) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-a.out:
			data, err := json.Marshal(frame)
			if err != nil {
				log.Printf("unencodable frame: %v", err)
				continue
			}
			if err := a.conn.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
	}
}

func (a *agent) handle(ctx context.Context, frame acp.Frame) {
	switch frame.Kind() {
	case acp.KindResponse:
		a.mu.Lock()
		ch := a.pending[string(frame.ID)]
		delete(a.pending, string(frame.ID))
		a.mu.Unlock()
		if ch != nil {
			ch <- frame
		}
		return

	case acp.KindNotification:
		if frame.Method == acp.MethodRCViewers {
			var p acp.ViewersParams
			_ = json.Unmarshal(frame.Params, &p)
			log.Printf("viewers: %d", p.Count)
		}
		return
	}

	switch frame.Method {
	case acp.MethodInitialize:
		a.reply(frame.ID, acp.InitializeResult{
			ProtocolVersion: 1,
			Meta: &acp.InitializeMeta{
				Session:       a.meta,
				RemoteControl: &acp.RemoteControlStatus{ReplayBuffer: 2048},
			},
		})

	case acp.MethodSessionList:
		a.reply(frame.ID, map[string]any{"sessions": []acp.SessionMeta{a.meta}})

	case acp.MethodSessionPrompt:
		var p acp.PromptParams
		_ = json.Unmarshal(frame.Params, &p)
		log.Printf("prompt: %q", p.Text)
		go a.turn(ctx, frame.ID, p.Text)

	case acp.MethodSessionCancel:
		a.mu.Lock()
		cancel := a.cancel
		a.mu.Unlock()
		if cancel != nil {
			log.Print("cancelled by glance")
			cancel()
		}
		a.reply(frame.ID, map[string]any{})

	default:
		a.send(acp.NewErrorResponse(frame.ID, acp.CodeMethodNotFound, "fakeagent: "+frame.Method))
	}
}

// turn plays a scripted turn. The response to the prompt request is sent last,
// which is what the real bridge does: `session/prompt` does not return until the
// turn is over.
func (a *agent) turn(parent context.Context, promptID json.RawMessage, text string) {
	ctx, cancel := context.WithCancel(parent)
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel() // a second prompt supersedes the turn in flight
	}
	a.cancel = cancel
	a.mu.Unlock()

	defer func() {
		cancel()
		a.mu.Lock()
		a.cancel = nil
		a.mu.Unlock()
	}()

	stopped := func() bool {
		select {
		case <-ctx.Done():
			return true
		case <-time.After(a.speed):
			return false
		}
	}

	a.update("user_message_chunk", map[string]any{"content": textBlock(text)})
	a.update("agent_thought_chunk", map[string]any{"content": textBlock("Considering how to answer that.")})
	if stopped() {
		a.finish(promptID, "cancelled")
		return
	}

	a.update("plan", map[string]any{"entries": []map[string]any{
		{"content": "Read the file", "status": "in_progress", "priority": "high"},
		{"content": "Report back", "status": "pending", "priority": "medium"},
	}})

	for _, chunk := range []string{"Sure — ", "let me look at ", "that file.\n\n"} {
		if stopped() {
			a.finish(promptID, "cancelled")
			return
		}
		a.update("agent_message_chunk", map[string]any{"content": textBlock(chunk)})
	}

	const toolCallID = "call-1"
	a.update("tool_call", map[string]any{
		"toolCallId": toolCallID,
		"title":      "Read src/main.rs",
		"kind":       "read",
		"status":     "pending",
		"rawInput":   map[string]any{"path": "src/main.rs"},
	})

	granted, err := a.requestPermission(ctx, toolCallID)
	if err != nil {
		a.update("tool_call_update", map[string]any{"toolCallId": toolCallID, "status": "failed"})
		a.finish(promptID, "cancelled")
		return
	}
	if !granted {
		a.update("tool_call_update", map[string]any{"toolCallId": toolCallID, "status": "failed"})
		a.update("agent_message_chunk", map[string]any{"content": textBlock("Understood, leaving it alone.")})
		a.turnCompleted()
		a.finish(promptID, "end_turn")
		return
	}

	a.update("tool_call_update", map[string]any{
		"toolCallId": toolCallID,
		"status":     "completed",
		"content": []map[string]any{
			{"type": "content", "content": textBlock("fn main() {\n    println!(\"hello\");\n}\n")},
		},
	})
	if stopped() {
		a.finish(promptID, "cancelled")
		return
	}

	a.update("plan", map[string]any{"entries": []map[string]any{
		{"content": "Read the file", "status": "completed", "priority": "high"},
		{"content": "Report back", "status": "completed", "priority": "medium"},
	}})
	a.update("agent_message_chunk", map[string]any{"content": textBlock("It prints `hello`. Nothing else in there.")})
	a.turnCompleted()
	a.finish(promptID, "end_turn")
}

// requestPermission raises a real interaction and waits for a real answer, so
// the browser's dialog is exercised end to end rather than mocked.
func (a *agent) requestPermission(ctx context.Context, toolCallID string) (bool, error) {
	id, reply := a.request(acp.MethodRequestPermission, map[string]any{
		"sessionId": a.meta.SessionID,
		"toolCall": map[string]any{
			"toolCallId": toolCallID,
			"title":      "Read src/main.rs",
			"kind":       "read",
		},
		"options": []map[string]any{
			{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
			{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
		},
	})

	// The terminal racing the browser for the same answer.
	var terminal <-chan time.Time
	if a.terminalAfter > 0 {
		t := time.NewTimer(a.terminalAfter)
		defer t.Stop()
		terminal = t.C
	}

	select {
	case <-ctx.Done():
		return false, ctx.Err()

	case <-terminal:
		log.Print("terminal answered first; retracting the browser's dialog")
		a.notify(acp.MethodRCInteractionCancelled, acp.InteractionCancelledParams{ID: id, ToolCallID: toolCallID})
		a.mu.Lock()
		delete(a.pending, fmt.Sprint(id))
		a.mu.Unlock()
		return true, nil

	case frame := <-reply:
		if frame.Error != nil {
			// A decline: glance is not answering, so in the real bridge the
			// terminal's dialog stays up. Here, the terminal shrugs and allows.
			log.Printf("declined by glance (%s); falling back to the terminal", frame.Error.Message)
			return true, nil
		}
		var out struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		_ = json.Unmarshal(frame.Result, &out)
		log.Printf("answered: %s/%s", out.Outcome.Outcome, out.Outcome.OptionID)
		return out.Outcome.OptionID != "reject" && out.Outcome.Outcome != "cancelled", nil
	}
}

// turnCompleted is emitted on the xAI rail on purpose: if it shows up in the UI,
// both rails are being mirrored, which is the thing most likely to silently
// regress.
func (a *agent) turnCompleted() {
	a.notify(acp.MethodXAINotification, map[string]any{
		"sessionId": a.meta.SessionID,
		"update":    map[string]any{"sessionUpdate": "turn_completed", "stopReason": "end_turn"},
	})
}

func (a *agent) finish(promptID json.RawMessage, stopReason string) {
	a.reply(promptID, map[string]any{"stopReason": stopReason})
}

func (a *agent) update(kind string, fields map[string]any) {
	update := map[string]any{"sessionUpdate": kind}
	for k, v := range fields {
		update[k] = v
	}
	a.notify(acp.MethodSessionUpdate, map[string]any{
		"sessionId": a.meta.SessionID,
		"update":    update,
	})
}

func (a *agent) notify(method string, params any) {
	frame, err := acp.NewNotification(method, params)
	if err != nil {
		log.Printf("notify %s: %v", method, err)
		return
	}
	a.send(frame)
}

func (a *agent) request(method string, params any) (uint64, <-chan acp.Frame) {
	a.mu.Lock()
	a.nextID++
	id := a.nextID
	ch := make(chan acp.Frame, 1)
	a.pending[fmt.Sprint(id)] = ch
	a.mu.Unlock()

	frame, err := acp.NewRequest(id, method, params)
	if err != nil {
		log.Printf("request %s: %v", method, err)
		return id, ch
	}
	a.send(frame)
	return id, ch
}

func (a *agent) reply(id json.RawMessage, result any) {
	frame, err := acp.NewResponse(id, result)
	if err != nil {
		log.Printf("reply: %v", err)
		return
	}
	a.send(frame)
}

func (a *agent) send(frame acp.Frame) {
	select {
	case a.out <- frame:
	default:
		log.Print("send queue full, dropping frame")
	}
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func mustCwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return "/"
	}
	return dir
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	return name
}
