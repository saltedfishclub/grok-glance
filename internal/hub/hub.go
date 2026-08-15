// Package hub is the live half of grok-glance: connected grok instances,
// connected browsers, and the routing between them.
//
// Nothing here is persisted. A restart drops every connection; the bridges
// reconnect on their own and browsers reconnect on their own, so the cost of a
// restart is the transcript history and nothing else.
//
// # Two protocols, deliberately
//
// grok speaks ACP over `/api/acp/agent`. Browsers speak a small glance envelope
// over `/api/ws`. Making the browser a real ACP peer was possible and rejected:
// it would push JSON-RPC correlation, request ids and the agent/client role
// inversion into the frontend, in exchange for nothing the UI actually needs.
// So the hub translates, and the frontend sees events with a `type`.
package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/user/grok-glance/internal/acp"
)

// EventType tags a message from glance to a browser.
type EventType string

const (
	// EventSnapshot is the first message on a browser socket: everything needed
	// to render without further round trips.
	EventSnapshot EventType = "snapshot"
	// EventAgents means the set of connected agents (or their metadata) changed.
	EventAgents EventType = "agents"
	// EventFrame is one mirrored ACP notification, passed through verbatim.
	EventFrame EventType = "frame"
	// EventInteraction is a request awaiting a human answer.
	EventInteraction EventType = "interaction"
	// EventInteractionResolved means it was answered -- possibly elsewhere.
	EventInteractionResolved EventType = "interaction_resolved"
	// EventNotice is a human-readable line for the UI to surface.
	EventNotice EventType = "notice"
	// EventError reports that a browser's own action failed.
	EventError EventType = "error"
)

// Event is what a browser receives.
type Event struct {
	Type  EventType `json:"type"`
	Agent string    `json:"agent,omitempty"`

	// Snapshot payload.
	Agents     []AgentSummary    `json:"agents,omitempty"`
	Frames     []json.RawMessage `json:"frames,omitempty"`
	Dropped    int               `json:"dropped,omitempty"`
	Open       []*Interaction    `json:"open,omitempty"`
	Session    *acp.SessionMeta  `json:"session,omitempty"`
	TurnActive bool              `json:"turnActive,omitempty"`

	// Streaming payload.
	Frame       json.RawMessage `json:"frame,omitempty"`
	Interaction *Interaction    `json:"interaction,omitempty"`

	// Resolution payload.
	ID         string `json:"id,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	// By is "browser", "terminal", or "declined" -- what the UI shows when a
	// dialog closes without this user having answered it.
	By string `json:"by,omitempty"`

	Message string `json:"message,omitempty"`
}

// Hub owns every live connection.
type Hub struct {
	log *slog.Logger

	mu       sync.RWMutex
	agents   map[string]*Agent
	browsers map[*Browser]struct{}
}

// New builds an empty hub.
func New(log *slog.Logger) *Hub {
	return &Hub{
		log:      log,
		agents:   make(map[string]*Agent),
		browsers: make(map[*Browser]struct{}),
	}
}

// ServeAgent runs a grok connection to completion.
//
// agentID identifies the connection for the lifetime of the socket; keyName is
// the API key's label, shown in the UI so several machines can be told apart.
func (h *Hub) ServeAgent(ctx context.Context, conn *websocket.Conn, agentID, keyName string) {
	now := time.Now()
	agent := &Agent{
		ID:           agentID,
		KeyName:      keyName,
		hub:          h,
		conn:         conn,
		log:          h.log.With("agent", agentID),
		outbound:     make(chan acp.Frame, agentSendQueue),
		done:         make(chan struct{}),
		connectedAt:  now,
		lastActivity: now,
		ring:         newRing(defaultRingCapacity),
		interactions: make(map[string]*Interaction),
		calls:        make(map[uint64]chan acp.Frame),
	}

	h.mu.Lock()
	// A reconnect from the same key replaces the old connection rather than
	// accumulating a ghost: the bridge reconnects after every network blip, and
	// a stale entry would show as a second session that never updates.
	if old, ok := h.agents[agentID]; ok {
		go old.close()
	}
	h.agents[agentID] = agent
	h.mu.Unlock()

	h.log.Info("agent connected", "agent", agentID, "key", keyName)
	h.broadcast(Event{Type: EventAgents, Agents: h.Summaries()})

	// Ask who this is. The bridge also volunteers it in `x.ai/rc/status` on
	// connect, but asking means the UI is correct even if that frame is missed.
	go h.initialize(ctx, agent)

	agent.serve(ctx)

	agent.close()
	h.mu.Lock()
	if h.agents[agentID] == agent {
		delete(h.agents, agentID)
	}
	h.mu.Unlock()

	h.log.Info("agent gone", "agent", agentID)
	h.broadcast(Event{Type: EventAgents, Agents: h.Summaries()})
}

func (h *Hub) initialize(ctx context.Context, agent *Agent) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	frame, err := agent.call(ctx, acp.MethodInitialize, map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			// glance is a viewer and a controller, not a workspace: it does not
			// offer grok a filesystem or a terminal, and says so up front.
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	})
	if err != nil {
		agent.log.Info("initialize failed", "err", err)
		return
	}
	if frame.Error != nil {
		agent.log.Warn("agent refused initialize", "err", frame.Error)
		return
	}

	var result acp.InitializeResult
	if err := json.Unmarshal(frame.Result, &result); err != nil {
		agent.log.Warn("could not read initialize result", "err", err)
		return
	}
	if result.Meta != nil {
		agent.mu.Lock()
		agent.meta = result.Meta.Session
		agent.mu.Unlock()
	}
	h.broadcast(Event{Type: EventAgents, Agents: h.Summaries()})
	h.syncViewers(agent)
}

// Agent looks up a connected instance.
func (h *Hub) Agent(id string) (*Agent, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	agent, ok := h.agents[id]
	return agent, ok
}

// Summaries lists connected agents, for the sessions page.
func (h *Hub) Summaries() []AgentSummary {
	h.mu.RLock()
	agents := make([]*Agent, 0, len(h.agents))
	for _, a := range h.agents {
		agents = append(agents, a)
	}
	h.mu.RUnlock()

	out := make([]AgentSummary, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.Summary())
	}
	return out
}

// broadcast fans an event out to every browser.
//
// Delivery is best-effort per browser: a viewer that cannot keep up is
// disconnected and reconnects into a fresh snapshot, which is both simpler and
// more correct than letting it fall arbitrarily far behind.
func (h *Hub) broadcast(ev Event) {
	raw, err := json.Marshal(ev)
	if err != nil {
		h.log.Warn("could not encode event", "type", ev.Type, "err", err)
		return
	}

	h.mu.RLock()
	browsers := make([]*Browser, 0, len(h.browsers))
	for b := range h.browsers {
		browsers = append(browsers, b)
	}
	h.mu.RUnlock()

	for _, b := range browsers {
		b.deliver(raw)
	}
}

// Notice pushes a human-readable line to every browser.
func (h *Hub) Notice(agentID, message string) {
	h.broadcast(Event{Type: EventNotice, Agent: agentID, Message: message})
}

func (h *Hub) addBrowser(b *Browser) {
	h.mu.Lock()
	h.browsers[b] = struct{}{}
	h.mu.Unlock()
	h.syncAllViewers()
}

func (h *Hub) removeBrowser(b *Browser) {
	h.mu.Lock()
	delete(h.browsers, b)
	h.mu.Unlock()
	h.syncAllViewers()
}

// syncAllViewers tells every bridge how many browsers are attached, so `/rc
// status` in the terminal reflects reality.
func (h *Hub) syncAllViewers() {
	h.mu.RLock()
	count := len(h.browsers)
	agents := make([]*Agent, 0, len(h.agents))
	for _, a := range h.agents {
		agents = append(agents, a)
	}
	h.mu.RUnlock()

	for _, a := range agents {
		a.NotifyViewers(count)
	}
}

func (h *Hub) syncViewers(agent *Agent) {
	h.mu.RLock()
	count := len(h.browsers)
	h.mu.RUnlock()
	agent.NotifyViewers(count)
}
