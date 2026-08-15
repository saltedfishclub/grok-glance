package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/pquerna/otp/totp"

	"github.com/user/grok-glance/internal/auth"
	"github.com/user/grok-glance/internal/hub"
	"github.com/user/grok-glance/internal/state"
)

// harness is one server with its own state directory, plus the pieces a test
// needs to forge credentials for it.
type harness struct {
	server *Server
	store  *state.Store
	auth   *auth.Manager
	hub    *hub.Hub
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// secure=false so the session cookie is usable over the plain-HTTP test
	// server; every other property of the cookie is unchanged.
	manager := auth.NewManager(store, false)
	h := hub.New(slog.New(slog.DiscardHandler))

	return &harness{
		server: New(Options{Store: store, Auth: manager, Hub: h, Log: slog.New(slog.DiscardHandler)}),
		store:  store,
		auth:   manager,
		hub:    h,
	}
}

// enroll puts the harness in the post-setup state and returns the TOTP secret.
func (h *harness) enroll(t *testing.T) string {
	t.Helper()
	enrollment, err := h.auth.BeginEnrollment("operator@test")
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if err := h.store.EnrollTOTP(enrollment.Secret, auth.Issuer, "operator@test"); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	return enrollment.Secret
}

func (h *harness) do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)
	return rec.Result()
}

func (h *harness) get(t *testing.T, path string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return h.do(t, req)
}

func (h *harness) post(t *testing.T, path string, body any, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(encoded))
	}
	req := httptest.NewRequest(http.MethodPost, path, reader)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return h.do(t, req)
}

func decodeBody[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

func sessionCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie on the response", auth.CookieName)
	return nil
}

func code(t *testing.T, secret string) string {
	t.Helper()
	c, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	return c
}

func TestStatusIsOpenAndCarriesSecurityHeaders(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/api/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The frontend has to be able to ask this before it has any credentials --
	// it is what decides between the setup, login and console screens.
	got := decodeBody[statusResponse](t, resp)
	if got.Enrolled || got.Authenticated {
		t.Fatalf("fresh server reports %+v, want both false", got)
	}

	// Agent output is rendered on this origin, so a missing CSP is a real hole,
	// not a lint failure.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q", csp)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestSetupIsInvisibleWithoutTheBootstrapToken(t *testing.T) {
	h := newHarness(t)
	token, err := h.store.NewBootstrapToken()
	if err != nil {
		t.Fatalf("NewBootstrapToken: %v", err)
	}

	// 404 rather than 403: a probe must not learn that a glance server is
	// sitting here with setup still pending.
	if resp := h.post(t, "/api/setup/begin", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no token: status = %d, want 404", resp.StatusCode)
	}
	if resp := h.post(t, "/api/setup/begin?token=wrong", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong token: status = %d, want 404", resp.StatusCode)
	}

	resp := h.post(t, "/api/setup/begin?token="+token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with token: status = %d, want 200", resp.StatusCode)
	}
	enrollment := decodeBody[auth.Enrollment](t, resp)
	if enrollment.Secret == "" || !strings.HasPrefix(enrollment.URI, "otpauth://totp/") {
		t.Fatalf("unusable enrollment: %+v", enrollment)
	}
}

func TestSetupCompleteVerifiesTheCodeThenSpendsTheToken(t *testing.T) {
	h := newHarness(t)
	token, err := h.store.NewBootstrapToken()
	if err != nil {
		t.Fatalf("NewBootstrapToken: %v", err)
	}
	enrollment := decodeBody[auth.Enrollment](t, h.post(t, "/api/setup/begin?token="+token, nil))

	bad := h.post(t, "/api/setup/complete?token="+token,
		map[string]string{"secret": enrollment.Secret, "code": "000000"})
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad code: status = %d, want 401", bad.StatusCode)
	}
	if h.auth.Enrolled() {
		t.Fatal("a rejected code still enrolled the authenticator")
	}

	good := h.post(t, "/api/setup/complete?token="+token,
		map[string]string{"secret": enrollment.Secret, "code": code(t, enrollment.Secret)})
	if good.StatusCode != http.StatusOK {
		t.Fatalf("good code: status = %d, want 200", good.StatusCode)
	}

	// Enrolling signs you in: you have just proved you hold the authenticator.
	cookie := sessionCookie(t, good)
	status := decodeBody[statusResponse](t, h.get(t, "/api/status", cookie))
	if !status.Enrolled || !status.Authenticated {
		t.Fatalf("after setup, status = %+v, want both true", status)
	}

	// The token is single-use, so the setup route closes behind it. Reopening it
	// would give anyone who saw the startup banner a second admin.
	if resp := h.post(t, "/api/setup/begin?token="+token, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("setup after enrollment: status = %d, want 404", resp.StatusCode)
	}
}

func TestSetupCompleteRejectsUnknownFields(t *testing.T) {
	h := newHarness(t)
	token, err := h.store.NewBootstrapToken()
	if err != nil {
		t.Fatalf("NewBootstrapToken: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/setup/complete?token="+token,
		strings.NewReader(`{"secret":"X","code":"000000","admin":true}`))
	if resp := h.do(t, req); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestLoginIssuesACookieAndRateLimits(t *testing.T) {
	h := newHarness(t)
	secret := h.enroll(t)

	resp := h.post(t, "/api/login", map[string]string{"code": code(t, secret)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good code: status = %d, want 200", resp.StatusCode)
	}
	cookie := sessionCookie(t, resp)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Fatalf("weak session cookie: %+v", cookie)
	}

	// Eight wrong codes are allowed, then the endpoint stops answering. Six
	// digits is 10^6 possibilities; unlimited guessing exhausts that in minutes.
	for attempt := range 8 {
		got := h.post(t, "/api/login", map[string]string{"code": "000000"})
		if got.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", attempt, got.StatusCode)
		}
	}
	limited := h.post(t, "/api/login", map[string]string{"code": "000000"})
	if limited.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after 8 failures: status = %d, want 429", limited.StatusCode)
	}

	// The limiter counts failures, not identities, so a correct code is also
	// held off until the window drains. That is deliberate: otherwise the
	// attacker's guesses would be free as long as the operator kept logging in.
	if got := h.post(t, "/api/login", map[string]string{"code": code(t, secret)}); got.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("good code while limited: status = %d, want 429", got.StatusCode)
	}
}

func TestSessionRoutesRequireACookie(t *testing.T) {
	h := newHarness(t)
	secret := h.enroll(t)

	for _, path := range []string{"/api/agents", "/api/ws"} {
		resp := h.get(t, path)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a cookie: status = %d, want 401", path, resp.StatusCode)
		}
	}

	cookie := sessionCookie(t, h.post(t, "/api/login", map[string]string{"code": code(t, secret)}))
	resp := h.get(t, "/api/agents", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/agents with a cookie: status = %d, want 200", resp.StatusCode)
	}
	listing := decodeBody[struct {
		Agents []hub.AgentSummary `json:"agents"`
	}](t, resp)
	// Empty, not null: the frontend maps over this without a guard.
	if listing.Agents == nil {
		t.Fatal("agents = null, want []")
	}
	if len(listing.Agents) != 0 {
		t.Fatalf("agents = %d, want 0", len(listing.Agents))
	}

	// A forged cookie must not be enough -- the value is HMAC-signed.
	forged := &http.Cookie{Name: auth.CookieName, Value: "9999999999.notasignature"}
	if got := h.get(t, "/api/agents", forged); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged cookie: status = %d, want 401", got.StatusCode)
	}
}

func TestLogoutClearsTheSession(t *testing.T) {
	h := newHarness(t)
	secret := h.enroll(t)
	cookie := sessionCookie(t, h.post(t, "/api/login", map[string]string{"code": code(t, secret)}))

	cleared := sessionCookie(t, h.post(t, "/api/logout", nil, cookie))
	if cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Fatalf("logout cookie = %+v, want an expiring empty value", cleared)
	}

	// The browser now holds the cleared cookie; the server must treat it as
	// anonymous rather than as a malformed session.
	status := decodeBody[statusResponse](t, h.get(t, "/api/status", cleared))
	if status.Authenticated {
		t.Fatal("still authenticated after logout")
	}
}

func TestAgentSocketRejectsBadKeysBeforeUpgrading(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.store.AddAPIKey("laptop"); err != nil {
		t.Fatalf("AddAPIKey: %v", err)
	}

	for name, header := range map[string]string{
		"no header":   "",
		"not bearer":  "Basic abc",
		"unknown key": "Bearer glance_sk_nope",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/acp/agent", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp := h.do(t, req)
		// 401 rather than a failed upgrade: the bridge reads a 4xx here as a
		// verdict on its credentials and stops retrying.
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}
}

// TestBothSocketsCarryTheirTraffic is the one test that exercises the real
// upgrade path: an agent dials in with an API key, a browser dials in with a
// cookie, and the browser is told the agent is there.
func TestBothSocketsCarryTheirTraffic(t *testing.T) {
	h := newHarness(t)
	secret := h.enroll(t)
	plaintext, key, err := h.store.AddAPIKey("laptop")
	if err != nil {
		t.Fatalf("AddAPIKey: %v", err)
	}

	server := httptest.NewServer(h.server)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	agentConn, _, err := websocket.Dial(ctx, wsURL+"/api/acp/agent", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + plaintext}},
	})
	if err != nil {
		t.Fatalf("agent dial: %v", err)
	}
	defer agentConn.CloseNow()

	// The hub asks who just connected. Reading it proves the connection is a
	// live ACP channel and not merely an accepted upgrade.
	_, hello, err := agentConn.Read(ctx)
	if err != nil {
		t.Fatalf("read initialize: %v", err)
	}
	var request struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(hello, &request); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if request.Method != "initialize" {
		t.Fatalf("first frame from the hub = %q, want initialize", request.Method)
	}

	cookie := sessionCookie(t, h.post(t, "/api/login", map[string]string{"code": code(t, secret)}))
	browserConn, _, err := websocket.Dial(ctx, wsURL+"/api/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}},
	})
	if err != nil {
		t.Fatalf("browser dial: %v", err)
	}
	defer browserConn.CloseNow()

	// A browser is handed the current agent list the moment it connects, so the
	// sessions page is populated without asking for anything.
	_, greeting, err := browserConn.Read(ctx)
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	var event struct {
		Type   string             `json:"type"`
		Agents []hub.AgentSummary `json:"agents"`
	}
	if err := json.Unmarshal(greeting, &event); err != nil {
		t.Fatalf("decode greeting: %v", err)
	}
	if event.Type != "agents" {
		t.Fatalf("greeting type = %q, want agents", event.Type)
	}
	if len(event.Agents) != 1 || event.Agents[0].ID != key.ID {
		t.Fatalf("greeting agents = %+v, want the one that just connected", event.Agents)
	}
	if event.Agents[0].KeyName != "laptop" {
		t.Fatalf("keyName = %q, want laptop", event.Agents[0].KeyName)
	}

	// Hanging up deregisters: a stale entry would show in the UI as a session
	// that never updates again.
	agentConn.Close(websocket.StatusNormalClosure, "done")
	waitFor(t, func() bool { return len(h.hub.Summaries()) == 0 })
}

func TestBrowserSocketRefusesAnUnauthenticatedUpgrade(t *testing.T) {
	h := newHarness(t)
	server := httptest.NewServer(h.server)
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/ws", nil)
	if err == nil {
		conn.CloseNow()
		t.Fatal("dialed /api/ws without a session cookie")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("upgrade response = %v, want 401", resp)
	}
}

func TestUnknownAPIPathsStayJSON(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/api/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	// The SPA fallback must not swallow API paths: a client that asked for JSON
	// and got index.html fails with a parse error miles from the real mistake.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON", ct)
	}
}

func TestPlaceholderPageWhenTheFrontendIsNotBuilt(t *testing.T) {
	h := newHarness(t)

	resp := h.get(t, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	resp.Body.Close()
	// `go run ./cmd/glance` before `npm run build` should explain itself rather
	// than 404.
	if !strings.Contains(string(body), "make web") {
		t.Fatalf("placeholder does not mention how to build the UI: %q", body)
	}
}

func TestSPAFallbackAndAssetCaching(t *testing.T) {
	h := newHarness(t)
	h.server = New(Options{
		Store: h.store,
		Auth:  h.auth,
		Hub:   h.hub,
		Log:   slog.New(slog.DiscardHandler),
		Web: fstest.MapFS{
			"index.html":            {Data: []byte("<!doctype html><title>glance</title>")},
			"assets/index-abc12.js": {Data: []byte("console.log(1)")},
		},
	})

	for _, path := range []string{"/", "/index.html", "/a/agent-1", "/deep/unknown/route"} {
		resp := h.get(t, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: read body: %v", path, err)
		}
		// Client-side routes have to survive a reload, so anything unclaimed
		// resolves to the shell.
		if !strings.Contains(string(body), "<title>glance</title>") {
			t.Fatalf("%s served %q, want index.html", path, body)
		}
		// index.html names the hashed bundles, so caching it would strand
		// browsers on the previous deploy.
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: Cache-Control = %q, want no-cache", path, cc)
		}
	}

	asset := h.get(t, "/assets/index-abc12.js")
	if asset.StatusCode != http.StatusOK {
		t.Fatalf("asset: status = %d, want 200", asset.StatusCode)
	}
	asset.Body.Close()
	// Hashed filenames change when the content does, so the response is
	// immutable by construction.
	if cc := asset.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset Cache-Control = %q, want immutable", cc)
	}

	// A missing asset falls back to the shell like any other path, but the API
	// namespace still refuses to.
	if resp := h.get(t, "/api/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/api/nope with a frontend present: status = %d, want 404", resp.StatusCode)
	}
}

// waitFor polls until cond holds, because hub bookkeeping happens on the
// connection's own goroutine and is not synchronous with the close.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never held")
}
