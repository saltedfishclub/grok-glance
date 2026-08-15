// Package acp speaks the Agent Client Protocol dialect that grok's `/rc` bridge
// exposes.
//
// There is no Go SDK for ACP upstream, so this is hand-written -- but
// deliberately thin. Glance is a control plane, not a second agent: it needs to
// correlate requests with responses, recognise the handful of methods it acts
// on, and pass everything else through to the browser untouched. Modelling all
// ~60 xAI notification variants as Go structs would be a large amount of code
// that breaks on every upstream sync and buys nothing, since the browser renders
// from the JSON either way.
//
// The roles are inverted relative to the terminal: over this link *grok is the
// Agent* and glance is the Client. That is what lets glance drive a session it
// did not create -- it sends `session/prompt` and `session/cancel`, and receives
// `session/update` plus permission requests.
package acp

import (
	"encoding/json"
	"fmt"
)

// Methods glance sends to grok.
const (
	MethodInitialize    = "initialize"
	MethodSessionList   = "session/list"
	MethodSessionPrompt = "session/prompt"
	MethodSessionCancel = "session/cancel"
	MethodRCReplay      = "x.ai/rc/replay"
	MethodRCViewers     = "x.ai/rc/viewers"
)

// Methods grok sends to glance.
const (
	MethodSessionUpdate = "session/update"
	// The xAI rail: tool-call deltas, subagent activity, turn boundaries. Not
	// in the upstream schema and not stable -- treated as opaque presentation
	// data, never as something correctness depends on.
	MethodXAINotification = "x.ai/session_notification"

	MethodRequestPermission = "session/request_permission"
	MethodAskUserQuestion   = "x.ai/ask_user_question"
	MethodExitPlanMode      = "x.ai/exit_plan_mode"

	MethodRCStatus = "x.ai/rc/status"
	// The terminal answered an interaction first: retract the browser's dialog.
	MethodRCInteractionCancelled = "x.ai/rc/interaction_cancelled"
)

// JSON-RPC error codes used on this link.
const (
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// Frame is one JSON-RPC 2.0 message in either direction.
//
// Every field is optional because the same struct decodes requests,
// notifications, and responses; which one it is follows from which fields are
// set, per Kind.
type Frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message)
}

// Kind classifies a decoded frame.
type Kind int

const (
	// KindRequest expects a response: it has both a method and an id.
	KindRequest Kind = iota
	// KindNotification is fire-and-forget: method, no id.
	KindNotification
	// KindResponse answers a request we sent: id, no method.
	KindResponse
	// KindInvalid is none of the above.
	KindInvalid
)

// Kind reports what f is.
func (f *Frame) Kind() Kind {
	hasID := len(f.ID) > 0 && string(f.ID) != "null"
	switch {
	case f.Method != "" && hasID:
		return KindRequest
	case f.Method != "":
		return KindNotification
	case hasID && (len(f.Result) > 0 || f.Error != nil):
		return KindResponse
	default:
		return KindInvalid
	}
}

// IsInteraction reports whether a request from grok is one the user must answer.
//
// These three are the set grok's own leader broadcasts for first-answer-wins
// arbitration. Handling only permissions would strand a browser user the moment
// the agent asked a question instead of requesting a tool.
func IsInteraction(method string) bool {
	switch method {
	case MethodRequestPermission, MethodAskUserQuestion, MethodExitPlanMode:
		return true
	}
	return false
}

// IsTranscript reports whether a notification from grok belongs in the
// transcript ring and should be forwarded to browsers.
//
// Both rails qualify: the stable `session/update` carries correctness, and the
// xAI rail carries the streaming detail that makes the transcript readable.
func IsTranscript(method string) bool {
	switch method {
	case MethodSessionUpdate, MethodXAINotification:
		return true
	}
	// grok's own predicate accepts an `x.ai/session/update` spelling too;
	// mirroring that keeps glance working if the bridge starts emitting it.
	return method == "x.ai/session/update"
}

// NewRequest builds a request frame.
func NewRequest(id uint64, method string, params any) (Frame, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return Frame{}, err
	}
	return Frame{JSONRPC: "2.0", ID: encodeID(id), Method: method, Params: raw}, nil
}

// NewNotification builds a notification frame.
func NewNotification(method string, params any) (Frame, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return Frame{}, err
	}
	return Frame{JSONRPC: "2.0", Method: method, Params: raw}, nil
}

// NewResponse builds a success response to id.
func NewResponse(id json.RawMessage, result any) (Frame, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return Frame{}, err
	}
	return Frame{JSONRPC: "2.0", ID: id, Result: raw}, nil
}

// NewErrorResponse builds a failure response to id.
func NewErrorResponse(id json.RawMessage, code int, message string) Frame {
	return Frame{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: message}}
}

func encodeID(id uint64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf("%d", id))
}

// marshalParams keeps `params` absent rather than null when there is nothing to
// send: some JSON-RPC peers distinguish the two, and absent is the safer of the
// two to emit.
func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(params)
}

// SessionMeta labels a mirrored session in the UI. grok sends it in the
// `initialize` result and again in every `x.ai/rc/status` notification, so a
// reconnecting browser can label the session without another round trip.
type SessionMeta struct {
	SessionID string `json:"sessionId,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Title     string `json:"title,omitempty"`
	Model     string `json:"model,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	Version   string `json:"version,omitempty"`
}

// Label is the best human-readable name available for this session.
func (m SessionMeta) Label() string {
	switch {
	case m.Title != "":
		return m.Title
	case m.CWD != "":
		return m.CWD
	case m.SessionID != "":
		return m.SessionID
	default:
		return "session"
	}
}

// InitializeResult is grok's reply to `initialize`.
type InitializeResult struct {
	ProtocolVersion int             `json:"protocolVersion"`
	Meta            *InitializeMeta `json:"_meta,omitempty"`
}

// InitializeMeta carries the session identity in `initialize`'s `_meta`.
type InitializeMeta struct {
	Session       SessionMeta          `json:"session"`
	RemoteControl *RemoteControlStatus `json:"remoteControl,omitempty"`
}

// RemoteControlStatus describes the bridge's replay ring.
type RemoteControlStatus struct {
	ReplayBuffer int `json:"replayBuffer"`
	Frames       int `json:"frames"`
	Dropped      int `json:"dropped"`
}

// StatusParams is the payload of `x.ai/rc/status`.
type StatusParams struct {
	Session SessionMeta `json:"session"`
	Replay  struct {
		Frames  int `json:"frames"`
		Dropped int `json:"dropped"`
	} `json:"replay"`
}

// InteractionCancelledParams is the payload of `x.ai/rc/interaction_cancelled`:
// the terminal answered first, so the browser's dialog must close.
type InteractionCancelledParams struct {
	ID         uint64 `json:"id"`
	ToolCallID string `json:"toolCallId,omitempty"`
}

// PromptParams asks grok to run a turn. The bridge accepts either ACP content
// blocks or a plain string; glance sends the string, which is all a browser
// prompt box produces.
type PromptParams struct {
	SessionID string `json:"sessionId,omitempty"`
	Text      string `json:"text,omitempty"`
}

// CancelParams interrupts the running turn.
type CancelParams struct {
	SessionID string `json:"sessionId,omitempty"`
}

// ViewersParams tells the bridge how many browsers are watching, so `/rc status`
// in the terminal can say so. Cosmetic.
type ViewersParams struct {
	Count int `json:"count"`
}

// ToolCallID digs the tool call id out of an interaction's params.
//
// It is what both sides key a retraction by: when one side answers, the other's
// dialog is closed by tool call id rather than by JSON-RPC id, because the
// browser never sees the terminal's ids. The three interaction shapes spell it
// differently, hence the two probes.
func ToolCallID(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var probe struct {
		ToolCallID string `json:"toolCallId"`
		ToolCall   struct {
			ToolCallID string `json:"toolCallId"`
		} `json:"toolCall"`
	}
	if err := json.Unmarshal(params, &probe); err != nil {
		return ""
	}
	if probe.ToolCallID != "" {
		return probe.ToolCallID
	}
	return probe.ToolCall.ToolCallID
}
