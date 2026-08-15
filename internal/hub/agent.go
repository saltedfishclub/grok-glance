package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/user/grok-glance/internal/acp"
)

const (
	// One turn of a busy session produces thousands of streaming deltas. This
	// holds a few minutes of that -- enough that a browser opened mid-turn sees
	// the turn, not so much that an idle server holds megabytes per agent.
	defaultRingCapacity = 4096

	// Depth of the per-agent outbound queue. Large enough to absorb a burst of
	// browser input; a full queue means the socket is wedged, and dropping the
	// connection is better than blocking a request handler on it.
	agentSendQueue = 64

	// How long a call to grok waits before giving up. `session/prompt` is the
	// exception: it blocks for the whole turn and gets no deadline of its own.
	callTimeout = 30 * time.Second
)

// ErrAgentGone means the grok instance disconnected before answering.
var ErrAgentGone = errors.New("agent disconnected")

// Interaction is a reverse-request from grok that a human must answer:
// a tool permission, a question, or a plan approval.
//
// Both the terminal and any browser can answer. Whoever answers first wins and
// the other side's dialog is retracted -- from glance's perspective that means
// either a browser answers (and glance replies to grok), or grok sends
// `x.ai/rc/interaction_cancelled` because the terminal got there first.
type Interaction struct {
	// ID is grok's JSON-RPC id, echoed back in the response.
	ID json.RawMessage `json:"id"`
	// Method is the ACP method, so the UI knows which dialog to render.
	Method string `json:"method"`
	// Params is passed through untouched: the browser renders from it, and
	// glance has no reason to understand its every field.
	Params json.RawMessage `json:"params"`
	// ToolCallID keys the retraction on both sides.
	ToolCallID string    `json:"toolCallId,omitempty"`
	OpenedAt   time.Time `json:"openedAt"`
}

// Agent is one connected grok instance and the single session it mirrors.
type Agent struct {
	ID      string
	KeyName string

	hub  *Hub
	conn *websocket.Conn
	log  *slog.Logger

	// outbound is drained by the writer goroutine. coder/websocket permits one
	// concurrent writer, and prompts, replies and heartbeats all originate on
	// different goroutines.
	outbound chan acp.Frame

	// done closes when the connection is torn down, unblocking everything
	// waiting on this agent.
	done     chan struct{}
	closeOne sync.Once

	nextID atomic.Uint64

	mu           sync.RWMutex
	meta         acp.SessionMeta
	connectedAt  time.Time
	lastActivity time.Time
	turnActive   bool
	ring         *ring
	interactions map[string]*Interaction
	// calls correlates responses to requests glance sent. Each channel is
	// buffered so a reply never blocks the reader goroutine, even if the caller
	// timed out and stopped listening.
	calls map[uint64]chan acp.Frame
}

// AgentSummary is the JSON view of an agent for the session list.
type AgentSummary struct {
	ID           string          `json:"id"`
	KeyName      string          `json:"keyName"`
	Session      acp.SessionMeta `json:"session"`
	Label        string          `json:"label"`
	ConnectedAt  time.Time       `json:"connectedAt"`
	LastActivity time.Time       `json:"lastActivity"`
	TurnActive   bool            `json:"turnActive"`
	Pending      int             `json:"pending"`
	Frames       int             `json:"frames"`
	Dropped      int             `json:"dropped"`
}

// Summary snapshots the agent for the UI.
func (a *Agent) Summary() AgentSummary {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return AgentSummary{
		ID:           a.ID,
		KeyName:      a.KeyName,
		Session:      a.meta,
		Label:        a.meta.Label(),
		ConnectedAt:  a.connectedAt,
		LastActivity: a.lastActivity,
		TurnActive:   a.turnActive,
		Pending:      len(a.interactions),
		Frames:       a.ring.size,
		Dropped:      a.ring.dropped,
	}
}

// Transcript returns the buffered frames, oldest first, plus how many were
// dropped off the front.
func (a *Agent) Transcript() ([]json.RawMessage, int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ring.snapshot(), a.ring.dropped
}

// OpenInteractions lists what is currently waiting on a human.
func (a *Agent) OpenInteractions() []*Interaction {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*Interaction, 0, len(a.interactions))
	for _, in := range a.interactions {
		out = append(out, in)
	}
	return out
}

// Prompt runs a turn. It returns when the turn ends, which can be minutes --
// callers that only want the turn *started* should not wait on it.
func (a *Agent) Prompt(ctx context.Context, text string) (json.RawMessage, error) {
	a.mu.RLock()
	sessionID := a.meta.SessionID
	a.mu.RUnlock()

	a.markTurn(true)
	frame, err := a.call(ctx, acp.MethodSessionPrompt, acp.PromptParams{
		SessionID: sessionID,
		Text:      text,
	})
	if err != nil {
		return nil, err
	}
	if frame.Error != nil {
		return nil, frame.Error
	}
	return frame.Result, nil
}

// Cancel interrupts the running turn.
//
// grok classifies this the same way it classifies Esc, but attributes it to
// glance rather than to the terminal, so the session log records who stopped it.
func (a *Agent) Cancel(ctx context.Context) error {
	a.mu.RLock()
	sessionID := a.meta.SessionID
	a.mu.RUnlock()

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	frame, err := a.call(ctx, acp.MethodSessionCancel, acp.CancelParams{SessionID: sessionID})
	if err != nil {
		return err
	}
	if frame.Error != nil {
		return frame.Error
	}
	return nil
}

// Answer resolves an open interaction with a result from the browser.
//
// It reports whether the interaction was still open: a false return is the
// normal outcome of losing the race to the terminal, not an error, and the UI
// shows "already handled elsewhere" rather than a failure.
func (a *Agent) Answer(id string, result json.RawMessage) bool {
	a.mu.Lock()
	interaction, ok := a.interactions[id]
	if ok {
		delete(a.interactions, id)
	}
	a.mu.Unlock()
	if !ok {
		return false
	}

	frame, err := acp.NewResponse(interaction.ID, json.RawMessage(result))
	if err != nil {
		a.log.Warn("could not encode interaction answer", "err", err)
		return false
	}
	a.send(frame)
	a.hub.broadcast(interactionResolvedEvent(a.ID, interaction, "browser"))
	return true
}

// Decline hands an interaction back to the terminal without answering it.
//
// grok treats a JSON-RPC error on an interaction as "glance is not answering
// this", leaves the terminal's dialog up, and the turn proceeds normally once
// the user answers there.
func (a *Agent) Decline(id, reason string) bool {
	a.mu.Lock()
	interaction, ok := a.interactions[id]
	if ok {
		delete(a.interactions, id)
	}
	a.mu.Unlock()
	if !ok {
		return false
	}
	a.send(acp.NewErrorResponse(interaction.ID, acp.CodeInternal, reason))
	a.hub.broadcast(interactionResolvedEvent(a.ID, interaction, "declined"))
	return true
}

// NotifyViewers tells the bridge how many browsers are watching, so the
// terminal's `/rc status` can say so. Cosmetic and best-effort.
func (a *Agent) NotifyViewers(n int) {
	frame, err := acp.NewNotification(acp.MethodRCViewers, acp.ViewersParams{Count: n})
	if err != nil {
		return
	}
	a.send(frame)
}

// call sends a request and waits for its response.
func (a *Agent) call(ctx context.Context, method string, params any) (acp.Frame, error) {
	id := a.nextID.Add(1)
	frame, err := acp.NewRequest(id, method, params)
	if err != nil {
		return acp.Frame{}, err
	}

	reply := make(chan acp.Frame, 1)
	a.mu.Lock()
	a.calls[id] = reply
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.calls, id)
		a.mu.Unlock()
	}()

	if !a.trySend(frame) {
		return acp.Frame{}, ErrAgentGone
	}

	select {
	case f := <-reply:
		return f, nil
	case <-a.done:
		return acp.Frame{}, ErrAgentGone
	case <-ctx.Done():
		return acp.Frame{}, ctx.Err()
	}
}

func (a *Agent) send(frame acp.Frame) {
	a.trySend(frame)
}

// trySend queues a frame, reporting whether it was accepted. A full queue means
// the socket is not draining; killing the connection turns a silent stall into a
// reconnect, which the bridge handles by design.
func (a *Agent) trySend(frame acp.Frame) bool {
	select {
	case a.outbound <- frame:
		return true
	case <-a.done:
		return false
	default:
		a.log.Warn("agent send queue full; dropping connection", "agent", a.ID)
		a.close()
		return false
	}
}

func (a *Agent) close() {
	a.closeOne.Do(func() {
		close(a.done)
		a.conn.CloseNow()
	})
}

func (a *Agent) markTurn(active bool) {
	a.mu.Lock()
	a.turnActive = active
	a.lastActivity = time.Now()
	a.mu.Unlock()
}

// serve runs the agent's read and write loops until the socket dies.
func (a *Agent) serve(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go a.writeLoop(ctx)

	for {
		typ, data, err := a.conn.Read(ctx)
		if err != nil {
			a.log.Info("agent disconnected", "agent", a.ID, "err", err)
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		a.handleFrame(data)
	}
}

func (a *Agent) writeLoop(ctx context.Context) {
	// A periodic ping is what turns a silently dead TCP connection (laptop
	// asleep, NAT entry evicted) into a normal disconnect the bridge reconnects
	// from, instead of an agent that shows as connected forever.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.done:
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := a.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				a.close()
				return
			}
		case frame := <-a.outbound:
			raw, err := json.Marshal(frame)
			if err != nil {
				a.log.Warn("could not encode frame for agent", "err", err)
				continue
			}
			writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err = a.conn.Write(writeCtx, websocket.MessageText, raw)
			cancel()
			if err != nil {
				a.log.Info("agent write failed", "agent", a.ID, "err", err)
				a.close()
				return
			}
		}
	}
}

// handleFrame dispatches one inbound frame from grok.
func (a *Agent) handleFrame(raw []byte) {
	var frame acp.Frame
	if err := json.Unmarshal(raw, &frame); err != nil {
		a.log.Warn("unparseable frame from agent", "agent", a.ID, "err", err)
		return
	}

	switch frame.Kind() {
	case acp.KindResponse:
		a.resolveCall(frame)

	case acp.KindRequest:
		if acp.IsInteraction(frame.Method) {
			a.openInteraction(frame)
			return
		}
		// grok drives nothing else on this link; saying so beats a fabricated
		// success that would leave it waiting for behaviour glance does not have.
		a.send(acp.NewErrorResponse(frame.ID, acp.CodeMethodNotFound,
			fmt.Sprintf("Method not found: %s", frame.Method)))

	case acp.KindNotification:
		a.handleNotification(frame, raw)

	default:
		a.log.Warn("frame from agent is neither request, response nor notification", "agent", a.ID)
	}
}

func (a *Agent) resolveCall(frame acp.Frame) {
	var id uint64
	if err := json.Unmarshal(frame.ID, &id); err != nil {
		a.log.Warn("response from agent with unusable id", "agent", a.ID)
		return
	}
	a.mu.Lock()
	reply, ok := a.calls[id]
	delete(a.calls, id)
	a.mu.Unlock()
	if !ok {
		// The caller timed out, or this is a duplicate. Neither is worth more
		// than a debug line.
		a.log.Debug("response for an unknown call", "agent", a.ID, "id", id)
		return
	}
	reply <- frame
}

func (a *Agent) openInteraction(frame acp.Frame) {
	interaction := &Interaction{
		ID:         frame.ID,
		Method:     frame.Method,
		Params:     frame.Params,
		ToolCallID: acp.ToolCallID(frame.Params),
		OpenedAt:   time.Now(),
	}

	a.mu.Lock()
	a.interactions[string(frame.ID)] = interaction
	a.lastActivity = interaction.OpenedAt
	a.mu.Unlock()

	a.hub.broadcast(Event{
		Type:        EventInteraction,
		Agent:       a.ID,
		Interaction: interaction,
	})
}

func (a *Agent) handleNotification(frame acp.Frame, raw []byte) {
	switch {
	case frame.Method == acp.MethodRCStatus:
		var params acp.StatusParams
		if err := json.Unmarshal(frame.Params, &params); err == nil {
			a.mu.Lock()
			a.meta = params.Session
			a.mu.Unlock()
			a.hub.broadcast(Event{Type: EventAgents, Agents: a.hub.Summaries()})
		}

	case frame.Method == acp.MethodRCInteractionCancelled:
		// The terminal answered first. Close the browser's dialog with the same
		// wording it would get if another browser had answered.
		var params acp.InteractionCancelledParams
		if err := json.Unmarshal(frame.Params, &params); err != nil {
			return
		}
		a.retract(fmt.Sprintf("%d", params.ID), params.ToolCallID, "terminal")

	case acp.IsTranscript(frame.Method):
		a.recordTranscript(frame, raw)

	default:
		a.log.Debug("unhandled notification from agent", "agent", a.ID, "method", frame.Method)
	}
}

// recordTranscript rings the frame and fans it out.
//
// The frame is stored and forwarded exactly as it arrived: `_meta` carries
// `eventId`, `promptId`, `chunkId` and `isReplay`, which is what lets a viewer
// dedup and order the stream the same way the terminal does. Rewriting it here
// would quietly break that.
func (a *Agent) recordTranscript(frame acp.Frame, raw []byte) {
	kind, toolCallID := classifyUpdate(frame.Params)

	a.mu.Lock()
	a.ring.push(json.RawMessage(raw))
	a.lastActivity = time.Now()
	switch kind {
	case updateTurnEnd:
		a.turnActive = false
	case updateInTurn:
		a.turnActive = true
	}
	a.mu.Unlock()

	// `interaction_resolved` is grok's own signal that a reverse-request was
	// answered somewhere. It reaches glance for interactions the bridge never
	// raced, so it is handled alongside `x.ai/rc/interaction_cancelled` rather
	// than instead of it.
	if kind == updateInteractionResolved && toolCallID != "" {
		a.retractByToolCall(toolCallID, "terminal")
	}

	a.hub.broadcast(Event{Type: EventFrame, Agent: a.ID, Frame: json.RawMessage(raw)})
}

// retract closes an interaction that was answered elsewhere.
func (a *Agent) retract(id, toolCallID, by string) {
	a.mu.Lock()
	interaction, ok := a.interactions[id]
	if ok {
		delete(a.interactions, id)
	}
	a.mu.Unlock()
	if !ok {
		if toolCallID != "" {
			a.retractByToolCall(toolCallID, by)
		}
		return
	}
	a.hub.broadcast(interactionResolvedEvent(a.ID, interaction, by))
}

func (a *Agent) retractByToolCall(toolCallID, by string) {
	a.mu.Lock()
	var found *Interaction
	for key, in := range a.interactions {
		if in.ToolCallID == toolCallID {
			found = in
			delete(a.interactions, key)
			break
		}
	}
	a.mu.Unlock()
	if found == nil {
		return
	}
	a.hub.broadcast(interactionResolvedEvent(a.ID, found, by))
}

func interactionResolvedEvent(agentID string, in *Interaction, by string) Event {
	return Event{
		Type:       EventInteractionResolved,
		Agent:      agentID,
		ID:         string(in.ID),
		ToolCallID: in.ToolCallID,
		By:         by,
	}
}

type updateKind int

const (
	updateOther updateKind = iota
	updateInTurn
	updateTurnEnd
	updateInteractionResolved
)

// classifyUpdate reads just enough of a notification to keep the UI's turn
// indicator honest.
//
// Both rails share the `{sessionId, update: {sessionUpdate: "...", ...}}` shape,
// so one probe covers them. Everything else in the payload stays opaque: the
// xAI rail's ~60 variants are internal to grok and drift with every upstream
// sync, so nothing load-bearing is keyed off them.
func classifyUpdate(params json.RawMessage) (updateKind, string) {
	if len(params) == 0 {
		return updateOther, ""
	}
	var probe struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			// The stable rail spells it camelCase; the xAI rail keeps Rust's
			// snake_case field names. Accept both rather than guess.
			ToolCallIDCamel string `json:"toolCallId"`
			ToolCallIDSnake string `json:"tool_call_id"`
		} `json:"update"`
	}
	if err := json.Unmarshal(params, &probe); err != nil {
		return updateOther, ""
	}
	toolCallID := probe.Update.ToolCallIDCamel
	if toolCallID == "" {
		toolCallID = probe.Update.ToolCallIDSnake
	}

	switch probe.Update.SessionUpdate {
	case "turn_completed":
		return updateTurnEnd, toolCallID
	case "interaction_resolved":
		return updateInteractionResolved, toolCallID
	case "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update",
		"user_message_chunk", "plan", "pending_interaction":
		return updateInTurn, toolCallID
	}
	return updateOther, toolCallID
}
