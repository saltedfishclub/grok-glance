package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// browserSendQueue bounds how far behind a viewer may fall.
//
// A turn can emit thousands of frames faster than a slow link drains them. Once
// this fills, the viewer is dropped and reconnects into a fresh snapshot --
// which is strictly better than an unbounded queue that turns one slow browser
// into the server's memory problem.
const browserSendQueue = 256

// Browser is one connected web client.
type Browser struct {
	hub *Hub
	log *slog.Logger

	conn     *websocket.Conn
	outbound chan []byte

	done     chan struct{}
	closeOne sync.Once
}

// command is what a browser sends.
type command struct {
	Type  string `json:"type"`
	Agent string `json:"agent,omitempty"`

	// Prompt.
	Text string `json:"text,omitempty"`

	// Interaction answer.
	ID     string          `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Reason string          `json:"reason,omitempty"`
}

// ServeBrowser runs a browser connection to completion.
func (h *Hub) ServeBrowser(ctx context.Context, conn *websocket.Conn) {
	b := &Browser{
		hub:      h,
		log:      h.log,
		conn:     conn,
		outbound: make(chan []byte, browserSendQueue),
		done:     make(chan struct{}),
	}

	h.addBrowser(b)
	defer func() {
		h.removeBrowser(b)
		b.close()
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go b.writeLoop(ctx)

	b.send(Event{Type: EventAgents, Agents: h.Summaries()})

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var cmd command
		if err := json.Unmarshal(data, &cmd); err != nil {
			b.send(Event{Type: EventError, Message: "could not parse command"})
			continue
		}
		b.handle(ctx, cmd)
	}
}

func (b *Browser) handle(ctx context.Context, cmd command) {
	switch cmd.Type {
	case "list":
		b.send(Event{Type: EventAgents, Agents: b.hub.Summaries()})

	case "subscribe":
		b.subscribe(cmd.Agent)

	case "prompt":
		b.prompt(ctx, cmd)

	case "cancel":
		b.cancel(ctx, cmd)

	case "answer":
		b.answer(cmd)

	case "decline":
		b.decline(cmd)

	default:
		b.send(Event{Type: EventError, Message: "unknown command: " + cmd.Type})
	}
}

// subscribe sends the full current state of one agent.
//
// Every browser receives every agent's frames regardless -- fan-out is cheap at
// this scale and per-browser filtering would be one more thing to get wrong.
// Subscribing is how a browser gets *history*: the ring, the open interactions
// and the session metadata, in one message, so a reload or a mid-session open
// renders immediately instead of waiting for the next frame.
func (b *Browser) subscribe(agentID string) {
	agent, ok := b.hub.Agent(agentID)
	if !ok {
		b.send(Event{Type: EventError, Agent: agentID, Message: "no such agent connected"})
		return
	}
	frames, dropped := agent.Transcript()
	summary := agent.Summary()
	session := summary.Session

	b.send(Event{
		Type:       EventSnapshot,
		Agent:      agentID,
		Agents:     b.hub.Summaries(),
		Frames:     frames,
		Dropped:    dropped,
		Open:       agent.OpenInteractions(),
		Session:    &session,
		TurnActive: summary.TurnActive,
	})
}

func (b *Browser) prompt(ctx context.Context, cmd command) {
	agent, ok := b.hub.Agent(cmd.Agent)
	if !ok {
		b.send(Event{Type: EventError, Agent: cmd.Agent, Message: "no such agent connected"})
		return
	}
	// Whitespace counts as empty: the UI trims before sending, but the server is
	// the boundary, and a stray Enter in a box holding a space should not start a
	// turn. The text itself goes on unmodified -- refusing an accident is the
	// server's business, editing someone's prompt is not.
	if strings.TrimSpace(cmd.Text) == "" {
		b.send(Event{Type: EventError, Agent: cmd.Agent, Message: "prompt is empty"})
		return
	}

	// A prompt does not return until the turn ends, which can be many minutes.
	// Waiting here would stall this browser's whole command stream -- including
	// the Stop button it might need next -- so the turn runs detached and its
	// progress arrives as mirrored frames like any other.
	go func() {
		if _, err := agent.Prompt(context.WithoutCancel(ctx), cmd.Text); err != nil {
			if errors.Is(err, ErrAgentGone) {
				b.hub.Notice(cmd.Agent, "the session disconnected before the turn finished")
				return
			}
			b.hub.Notice(cmd.Agent, "prompt failed: "+err.Error())
		}
	}()
}

func (b *Browser) cancel(ctx context.Context, cmd command) {
	agent, ok := b.hub.Agent(cmd.Agent)
	if !ok {
		b.send(Event{Type: EventError, Agent: cmd.Agent, Message: "no such agent connected"})
		return
	}
	go func() {
		if err := agent.Cancel(context.WithoutCancel(ctx)); err != nil {
			b.hub.Notice(cmd.Agent, "interrupt failed: "+err.Error())
			return
		}
		b.hub.Notice(cmd.Agent, "turn interrupted from the web UI")
	}()
}

func (b *Browser) answer(cmd command) {
	agent, ok := b.hub.Agent(cmd.Agent)
	if !ok {
		b.send(Event{Type: EventError, Agent: cmd.Agent, Message: "no such agent connected"})
		return
	}
	if len(cmd.Result) == 0 {
		b.send(Event{Type: EventError, Agent: cmd.Agent, Message: "answer has no result"})
		return
	}
	if !agent.Answer(cmd.ID, cmd.Result) {
		// Losing the race is the expected outcome half the time, not an error:
		// the terminal answered first, or another browser did.
		b.send(Event{
			Type:    EventInteractionResolved,
			Agent:   cmd.Agent,
			ID:      cmd.ID,
			By:      "elsewhere",
			Message: "already handled elsewhere",
		})
	}
}

func (b *Browser) decline(cmd command) {
	agent, ok := b.hub.Agent(cmd.Agent)
	if !ok {
		b.send(Event{Type: EventError, Agent: cmd.Agent, Message: "no such agent connected"})
		return
	}
	reason := cmd.Reason
	if reason == "" {
		reason = "declined in the web UI; answer in the terminal"
	}
	if !agent.Decline(cmd.ID, reason) {
		b.send(Event{
			Type:    EventInteractionResolved,
			Agent:   cmd.Agent,
			ID:      cmd.ID,
			By:      "elsewhere",
			Message: "already handled elsewhere",
		})
	}
}

func (b *Browser) send(ev Event) {
	raw, err := json.Marshal(ev)
	if err != nil {
		b.log.Warn("could not encode event for browser", "err", err)
		return
	}
	b.deliver(raw)
}

func (b *Browser) deliver(raw []byte) {
	select {
	case b.outbound <- raw:
	case <-b.done:
	default:
		b.log.Info("browser too slow; dropping it to reconnect")
		b.close()
	}
}

func (b *Browser) writeLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-b.done:
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := b.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				b.close()
				return
			}
		case raw := <-b.outbound:
			writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := b.conn.Write(writeCtx, websocket.MessageText, raw)
			cancel()
			if err != nil {
				b.close()
				return
			}
		}
	}
}

func (b *Browser) close() {
	b.closeOne.Do(func() {
		close(b.done)
		b.conn.CloseNow()
	})
}
