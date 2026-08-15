package acp

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, raw string) Frame {
	t.Helper()
	var f Frame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return f
}

func TestKindClassifiesEveryShape(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Kind
	}{
		{"request", `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{}}`, KindRequest},
		{"notification", `{"jsonrpc":"2.0","method":"session/update","params":{}}`, KindNotification},
		{"response", `{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`, KindResponse},
		{"error response", `{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"nope"}}`, KindResponse},
		{"string id request", `{"jsonrpc":"2.0","id":"abc","method":"x.ai/ask_user_question"}`, KindRequest},
		// A null id is JSON-RPC's "no id", not id zero: treating it as a request
		// would have glance reply to something nothing is listening for.
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"session/update"}`, KindNotification},
		{"empty", `{"jsonrpc":"2.0"}`, KindInvalid},
		{"id only", `{"jsonrpc":"2.0","id":7}`, KindInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := decode(t, tc.raw)
			if got := frame.Kind(); got != tc.want {
				t.Fatalf("Kind() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInteractionAndTranscriptMethods(t *testing.T) {
	// These three are the set that races between the terminal and the browser.
	// Dropping one would strand a remote user whenever the agent used it.
	for _, m := range []string{MethodRequestPermission, MethodAskUserQuestion, MethodExitPlanMode} {
		if !IsInteraction(m) {
			t.Fatalf("IsInteraction(%q) = false", m)
		}
	}
	for _, m := range []string{MethodSessionUpdate, "session/list", "fs/read_text_file", ""} {
		if IsInteraction(m) {
			t.Fatalf("IsInteraction(%q) = true", m)
		}
	}

	// Both rails must reach the browser: the stable one carries correctness, the
	// xAI one carries the streaming deltas that make a transcript readable.
	for _, m := range []string{MethodSessionUpdate, MethodXAINotification, "x.ai/session/update"} {
		if !IsTranscript(m) {
			t.Fatalf("IsTranscript(%q) = false", m)
		}
	}
	for _, m := range []string{MethodRCStatus, MethodRCInteractionCancelled, ""} {
		if IsTranscript(m) {
			t.Fatalf("IsTranscript(%q) = true", m)
		}
	}
}

func TestFrameBuilders(t *testing.T) {
	req, err := NewRequest(42, MethodSessionPrompt, PromptParams{SessionID: "s1", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if req.JSONRPC != "2.0" || string(req.ID) != "42" || req.Kind() != KindRequest {
		t.Fatalf("bad request frame: %+v", req)
	}

	// Absent beats null: some JSON-RPC peers distinguish the two.
	note, err := NewNotification(MethodRCViewers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if note.Params != nil {
		t.Fatalf("nil params encoded as %s, want absent", note.Params)
	}
	if note.Kind() != KindNotification {
		t.Fatalf("notification classified as %v", note.Kind())
	}

	resp, err := NewResponse(json.RawMessage(`7`), map[string]string{"outcome": "allow"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Kind() != KindResponse || string(resp.ID) != "7" {
		t.Fatalf("bad response frame: %+v", resp)
	}

	fail := NewErrorResponse(json.RawMessage(`7`), CodeMethodNotFound, "Method not found")
	if fail.Error == nil || fail.Error.Code != CodeMethodNotFound {
		t.Fatalf("bad error frame: %+v", fail)
	}
	if fail.Kind() != KindResponse {
		t.Fatalf("error response classified as %v", fail.Kind())
	}
}

func TestToolCallIDProbesBothShapes(t *testing.T) {
	cases := map[string]string{
		`{"toolCallId":"tc_1"}`:              "tc_1",
		`{"toolCall":{"toolCallId":"tc_2"}}`: "tc_2",
		`{"toolCallId":"","toolCall":{}}`:    "",
		`{"sessionId":"s1"}`:                 "",
		`not json`:                           "",
		``:                                   "",
		`{"toolCallId":"tc_3","toolCall":{"toolCallId":"tc_other"}}`: "tc_3",
	}
	for params, want := range cases {
		if got := ToolCallID(json.RawMessage(params)); got != want {
			t.Fatalf("ToolCallID(%s) = %q, want %q", params, got, want)
		}
	}
}

func TestSessionMetaLabelDegradesGracefully(t *testing.T) {
	cases := []struct {
		meta SessionMeta
		want string
	}{
		{SessionMeta{Title: "fix the parser", CWD: "/src", SessionID: "s1"}, "fix the parser"},
		{SessionMeta{CWD: "/src", SessionID: "s1"}, "/src"},
		{SessionMeta{SessionID: "s1"}, "s1"},
		// A session that connected but has not yet reported anything still needs
		// a row in the UI rather than a blank one.
		{SessionMeta{}, "session"},
	}
	for _, tc := range cases {
		if got := tc.meta.Label(); got != tc.want {
			t.Fatalf("Label(%+v) = %q, want %q", tc.meta, got, tc.want)
		}
	}
}
