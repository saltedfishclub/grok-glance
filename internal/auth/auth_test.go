package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/user/grok-glance/internal/state"
)

func newManager(t *testing.T) (*Manager, *state.Store) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return NewManager(store, true), store
}

func code(t *testing.T, secret string) string {
	t.Helper()
	c, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	return c
}

func TestEnrollmentRequiresAWorkingCode(t *testing.T) {
	m, store := newManager(t)

	enrollment, err := m.BeginEnrollment("operator@localhost")
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if enrollment.Secret == "" || !strings.HasPrefix(enrollment.URI, "otpauth://totp/") {
		t.Fatalf("unusable enrollment: %+v", enrollment)
	}

	// Nothing is persisted until the user proves the secret reached their phone.
	// Otherwise a failed QR scan would lock the operator out of their own server.
	if store.Enrolled() {
		t.Fatal("BeginEnrollment persisted the secret before it was confirmed")
	}

	if err := m.CompleteEnrollment(enrollment.Secret, "000000", ""); !errors.Is(err, ErrBadCode) {
		t.Fatalf("CompleteEnrollment with a wrong code: %v, want ErrBadCode", err)
	}
	if store.Enrolled() {
		t.Fatal("a failed confirmation still enrolled")
	}

	if err := m.CompleteEnrollment(enrollment.Secret, code(t, enrollment.Secret), ""); err != nil {
		t.Fatalf("CompleteEnrollment: %v", err)
	}
	if !store.Enrolled() {
		t.Fatal("enrollment did not persist")
	}

	// A second enrollment would be a password reset with no authentication on it.
	if _, err := m.BeginEnrollment(""); err == nil {
		t.Fatal("a second enrollment was allowed")
	}
}

func TestLoginRejectsReplayAndRateLimits(t *testing.T) {
	m, _ := newManager(t)

	if err := m.Login("123456"); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("login before enrollment: %v, want ErrNotEnrolled", err)
	}

	enrollment, err := m.BeginEnrollment("")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CompleteEnrollment(enrollment.Secret, code(t, enrollment.Secret), ""); err != nil {
		t.Fatal(err)
	}

	valid := code(t, enrollment.Secret)
	if err := m.Login(valid); err != nil {
		t.Fatalf("Login with a fresh code: %v", err)
	}
	// A TOTP code stays valid for its whole 30-second step, so anyone who saw it
	// could use it again inside that window.
	if err := m.Login(valid); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed code: %v, want ErrReplay", err)
	}

	for i := 0; i < maxAttempts; i++ {
		if err := m.Login("000000"); errors.Is(err, ErrRateLimited) {
			t.Fatalf("rate limit tripped early, after %d attempts", i)
		}
	}
	// 10^6 codes is a short brute force at network speed; the limiter is what
	// makes a 6-digit secret adequate.
	if err := m.Login("000000"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after %d failures: %v, want ErrRateLimited", maxAttempts, err)
	}
}

func TestSessionCookieRoundTrip(t *testing.T) {
	m, _ := newManager(t)

	rec := httptest.NewRecorder()
	m.IssueCookie(rec)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("IssueCookie wrote %d cookies, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != CookieName {
		t.Fatalf("cookie name = %q, want %q", cookie.Name, CookieName)
	}
	// The `__Host-` prefix is only honoured by browsers when all three hold.
	if !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/" {
		t.Fatalf("cookie does not satisfy the __Host- prefix rules: %+v", cookie)
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("cookie is not SameSite=Strict")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.AddCookie(cookie)
	if !m.Authenticated(req) {
		t.Fatal("a freshly issued cookie did not authenticate")
	}

	// No cookie at all.
	if m.Authenticated(httptest.NewRequest(http.MethodGet, "/api/agents", nil)) {
		t.Fatal("an unauthenticated request was accepted")
	}
}

func TestForgedCookiesAreRejected(t *testing.T) {
	m, _ := newManager(t)

	far := strconv.FormatInt(time.Now().Add(100*time.Hour).Unix(), 10)
	cases := map[string]string{
		"no signature":      far,
		"empty":             "",
		"garbage signature": far + ".not-a-signature",
		"unsigned future":   far + ".",
		"expired":           m.signSession(time.Now().Add(-time.Minute).Unix()),
	}
	for name, value := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: value})
		if m.Authenticated(req) {
			t.Fatalf("%s: forged cookie accepted", name)
		}
	}

	// Extending an otherwise-valid cookie's expiry must invalidate the signature.
	valid := m.signSession(time.Now().Add(time.Hour).Unix())
	_, sig, _ := strings.Cut(valid, ".")
	tampered := strconv.FormatInt(time.Now().Add(1000*time.Hour).Unix(), 10) + "." + sig
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: tampered})
	if m.Authenticated(req) {
		t.Fatal("a cookie with an extended expiry was accepted")
	}
}

func TestBearerAndBootstrapExtraction(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/acp/agent", nil)
	if got := BearerToken(req); got != "" {
		t.Fatalf("BearerToken with no header = %q", got)
	}
	req.Header.Set("Authorization", "Bearer glance_sk_abc")
	if got := BearerToken(req); got != "glance_sk_abc" {
		t.Fatalf("BearerToken = %q", got)
	}
	// Some proxies and clients normalise the scheme's case.
	req.Header.Set("Authorization", "bearer glance_sk_abc")
	if got := BearerToken(req); got != "glance_sk_abc" {
		t.Fatalf("BearerToken with a lowercase scheme = %q", got)
	}
	req.Header.Set("Authorization", "Basic glance_sk_abc")
	if got := BearerToken(req); got != "" {
		t.Fatalf("BearerToken accepted a Basic header: %q", got)
	}

	// The token arrives in the URL the operator pastes from the terminal, and in
	// a header once the SPA takes over.
	q := httptest.NewRequest(http.MethodPost, "/api/setup/begin?token=abc", nil)
	if got := BootstrapToken(q); got != "abc" {
		t.Fatalf("BootstrapToken from query = %q", got)
	}
	h := httptest.NewRequest(http.MethodPost, "/api/setup/begin", nil)
	h.Header.Set("X-Glance-Bootstrap", "abc")
	if got := BootstrapToken(h); got != "abc" {
		t.Fatalf("BootstrapToken from header = %q", got)
	}
}
