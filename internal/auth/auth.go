// Package auth is grok-glance's whole access-control story.
//
// There is no username and no password. A single TOTP authenticator, enrolled
// once, is the only credential — and enrolling it requires a token the server
// printed on its own stdout. That combination is what makes it safe to expose
// this port at all: without the bootstrap gate, whoever loaded /setup first
// would become the admin, and on a machine reachable from a network that is not
// necessarily the operator.
//
// What this does not defend against, stated plainly because it shapes how the
// server should be deployed: anyone who can read `~/.grok/glance/state.json` has
// the TOTP secret and the cookie-signing key, and anyone who can watch the
// network without TLS has the session cookie. Glance is meant to run behind TLS
// (a reverse proxy or a tunnel), on a machine whose home directory the operator
// controls.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/user/grok-glance/internal/state"
)

const (
	// CookieName is the session cookie. The `__Host-` prefix is a browser-
	// enforced promise: Secure, path=/, and no Domain attribute, so it cannot be
	// set or overwritten by a sibling host.
	CookieName = "__Host-glance"

	// SessionTTL is how long a login lasts. Long enough not to nag someone
	// checking on an agent through the day; short enough that a forgotten open
	// tab is not indefinite access.
	SessionTTL = 12 * time.Hour

	// Issuer labels the entry in the authenticator app.
	Issuer = "grok-glance"

	// loginWindow is how many 30-second steps either side of now are accepted,
	// covering ordinary clock skew between the phone and the server.
	loginWindow = 1

	// maxAttempts is the number of failed codes allowed per window before the
	// endpoint stops answering. A 6-digit code is 10^6 possibilities; unlimited
	// guessing would exhaust that in minutes.
	maxAttempts   = 8
	attemptWindow = 5 * time.Minute
)

var (
	// ErrNotEnrolled means setup has not run.
	ErrNotEnrolled = errors.New("no authenticator is enrolled")
	// ErrBadCode means the TOTP code did not verify.
	ErrBadCode = errors.New("that code is not valid")
	// ErrReplay means the code was already used. TOTP codes stay valid for a
	// whole step, so accepting one twice would let an observer reuse it.
	ErrReplay = errors.New("that code has already been used")
	// ErrRateLimited means too many failures too fast.
	ErrRateLimited = errors.New("too many attempts; wait a minute and try again")
)

// Manager verifies codes and mints session cookies.
type Manager struct {
	store  *state.Store
	secure bool

	mu       sync.Mutex
	usedStep map[int64]time.Time
	failures []time.Time
}

// NewManager builds the verifier.
//
// secure controls the cookie's Secure attribute. It is false only for plain-HTTP
// localhost development, where browsers would otherwise refuse the cookie
// outright; any real deployment sets it.
func NewManager(store *state.Store, secure bool) *Manager {
	return &Manager{
		store:    store,
		secure:   secure,
		usedStep: make(map[int64]time.Time),
	}
}

// Enrolled reports whether an authenticator exists.
func (m *Manager) Enrolled() bool { return m.store.Enrolled() }

// BootstrapValid reports whether token unlocks /setup.
func (m *Manager) BootstrapValid(token string) bool { return m.store.BootstrapValid(token) }

// Enrollment is a proposed authenticator, not yet persisted.
type Enrollment struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
}

// BeginEnrollment generates a candidate secret and its `otpauth://` URI.
//
// Nothing is persisted here: the secret only becomes the server's credential
// once the user proves they can generate a code from it. Persisting first would
// lock the operator out whenever a QR scan silently failed.
func (m *Manager) BeginEnrollment(account string) (Enrollment, error) {
	if m.store.Enrolled() {
		return Enrollment{}, errors.New("an authenticator is already enrolled")
	}
	if account == "" {
		account = "operator"
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      Issuer,
		AccountName: account,
		Period:      30,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return Enrollment{}, err
	}
	return Enrollment{Secret: key.Secret(), URI: key.URL()}, nil
}

// CompleteEnrollment verifies one code against the candidate secret and, on
// success, stores it and burns the bootstrap token.
func (m *Manager) CompleteEnrollment(secret, code, account string) error {
	if m.store.Enrolled() {
		return errors.New("an authenticator is already enrolled")
	}
	if !verify(code, secret) {
		return ErrBadCode
	}
	if account == "" {
		account = "operator"
	}
	return m.store.EnrollTOTP(secret, Issuer, account)
}

// Login verifies a code against the enrolled authenticator.
func (m *Manager) Login(code string) error {
	secret := m.store.TOTPSecret()
	if secret == "" {
		return ErrNotEnrolled
	}

	if err := m.checkRate(); err != nil {
		return err
	}

	code = strings.TrimSpace(code)
	if !verify(code, secret) {
		m.recordFailure()
		return ErrBadCode
	}

	// A TOTP code is valid for its whole 30-second step, so a code observed on
	// the wire or over a shoulder can be replayed within that window. Burning
	// the step closes it.
	step := time.Now().Unix() / 30
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneStepsLocked()
	if _, used := m.usedStep[step]; used {
		return ErrReplay
	}
	m.usedStep[step] = time.Now()
	return nil
}

func verify(code, secret string) bool {
	ok, err := totp.ValidateCustom(code, secret, time.Now(), totp.ValidateOpts{
		Period:    30,
		Skew:      loginWindow,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	return err == nil && ok
}

func (m *Manager) checkRate() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-attemptWindow)
	kept := m.failures[:0]
	for _, at := range m.failures {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	m.failures = kept
	if len(m.failures) >= maxAttempts {
		return ErrRateLimited
	}
	return nil
}

func (m *Manager) recordFailure() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures = append(m.failures, time.Now())
}

// pruneStepsLocked drops burnt steps that can no longer be replayed, so the map
// does not grow for the life of the process.
func (m *Manager) pruneStepsLocked() {
	cutoff := time.Now().Add(-2 * time.Minute)
	for step, at := range m.usedStep {
		if at.Before(cutoff) {
			delete(m.usedStep, step)
		}
	}
}

// IssueCookie writes a signed session cookie.
//
// The cookie is `<expiry>.<hmac>` — self-contained, so the server keeps no
// session table and a restart does not log everyone out (the signing key
// survives in state.json). Deleting state.json is the panic button: it rotates
// the key and invalidates every outstanding cookie.
func (m *Manager) IssueCookie(w http.ResponseWriter) {
	expiry := time.Now().Add(SessionTTL).Unix()
	value := m.signSession(expiry)
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Unix(expiry, 0),
	})
}

// ClearCookie logs the browser out.
func (m *Manager) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// Authenticated reports whether r carries a valid, unexpired session.
func (m *Manager) Authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	return m.validSession(cookie.Value)
}

func (m *Manager) signSession(expiry int64) string {
	payload := strconv.FormatInt(expiry, 10)
	mac := hmac.New(sha256.New, m.store.SessionKey())
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (m *Manager) validSession(value string) bool {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	expiry, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, m.store.SessionKey())
	mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return false
	}
	// Signature first, expiry second: checking expiry on an unverified payload
	// would be reading attacker-controlled data as though it meant something.
	return time.Now().Unix() < expiry
}

// BearerToken pulls the API key out of an Authorization header.
func BearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// BootstrapToken pulls the setup token from the query string or a header.
func BootstrapToken(r *http.Request) string {
	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}
	return r.Header.Get("X-Glance-Bootstrap")
}

// DescribeEnrollment renders the account label for an otpauth URI.
func DescribeEnrollment(host string) string {
	if host == "" {
		return "operator"
	}
	return fmt.Sprintf("operator@%s", host)
}
