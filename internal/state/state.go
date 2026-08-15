// Package state owns everything grok-glance keeps across restarts.
//
// That is deliberately very little: the TOTP secret, the hashes of issued API
// keys, the bootstrap token's hash, and the key used to sign session cookies.
// Transcripts are not here and never will be -- they live in a bounded
// in-memory ring per connected agent and are gone when the process exits. A
// control plane that records everything an agent ever said is a much larger
// security promise than this one is prepared to keep.
//
// The whole file is rewritten atomically under a mutex on every change. It is a
// few kilobytes at most and changes a handful of times per install, so a
// database would buy nothing and cost a migration story.
package state

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Version of the on-disk format. Bump only for incompatible changes; unknown
// higher versions are refused rather than silently reinterpreted.
const Version = 1

// ErrFutureVersion means the state file was written by a newer glance.
var ErrFutureVersion = errors.New("state file was written by a newer grok-glance")

// TOTP is the enrolled authenticator. Exactly one exists once setup completes.
type TOTP struct {
	Secret     string    `json:"secret"`
	Issuer     string    `json:"issuer"`
	Account    string    `json:"account"`
	EnrolledAt time.Time `json:"enrolled_at"`
}

// Bootstrap is the one-time token that gates /setup.
//
// Only its hash is stored. Without this gate, whoever loads /setup first
// becomes the admin -- including anyone who finds the port before the operator
// does. Requiring a token printed on the server's own stdout closes that
// window.
type Bootstrap struct {
	Hash      string     `json:"hash"`
	CreatedAt time.Time  `json:"created_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
}

// Used reports whether enrollment has already consumed this token.
func (b *Bootstrap) Used() bool { return b != nil && b.UsedAt != nil }

// APIKey is one credential a grok instance uses to dial in. Only the hash is
// stored: a leaked state file must not yield working keys.
type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Hash      string     `json:"hash"`
	CreatedAt time.Time  `json:"created_at"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
}

type data struct {
	Version    int        `json:"version"`
	TOTP       *TOTP      `json:"totp,omitempty"`
	Bootstrap  *Bootstrap `json:"bootstrap,omitempty"`
	SessionKey string     `json:"session_key"`
	APIKeys    []APIKey   `json:"api_keys"`
}

// Store is the process-wide handle on the state file.
//
// The file is shared with a second process more often than it looks: `glance
// apikey add` and `glance bootstrap` run against the state directory of a server
// that is already up. So the in-memory copy is a cache of the file, not the
// authority — see refreshLocked.
type Store struct {
	mu    sync.RWMutex
	path  string
	d     data
	stamp fileStamp
}

// fileStamp is how the store notices someone else wrote the file. Modtime and
// size are not a strong identity, but the alternative — re-reading and parsing
// on every cookie check — costs more than it is worth for a file that changes a
// handful of times per install.
type fileStamp struct {
	mod  time.Time
	size int64
}

// DefaultDir is where glance keeps its files. It sits alongside grok's own
// config so an operator has one directory to back up and one to lock down.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".grok", "glance"), nil
}

// Open loads the store at dir, creating a fresh one if absent.
//
// Pre-existing files in the directory (grok's own `secret.key`, `hook.secret`)
// are neither read nor touched: glance owns exactly `state.json` and
// `bootstrap.token`.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	s := &Store{path: filepath.Join(dir, "state.json")}

	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		key, err := randomBytes(32)
		if err != nil {
			return nil, err
		}
		s.d = data{
			Version:    Version,
			SessionKey: base64.StdEncoding.EncodeToString(key),
			APIKeys:    []APIKey{},
		}
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}

	if err := json.Unmarshal(raw, &s.d); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if s.d.Version > Version {
		return nil, fmt.Errorf("%w: found v%d, this build understands v%d",
			ErrFutureVersion, s.d.Version, Version)
	}
	if s.d.SessionKey == "" {
		key, err := randomBytes(32)
		if err != nil {
			return nil, err
		}
		s.d.SessionKey = base64.StdEncoding.EncodeToString(key)
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
		return s, nil
	}
	s.stampLocked()
	return s, nil
}

// Path is the state file's location, for error messages and `glance version`.
func (s *Store) Path() string { return s.path }

// SessionKey is the HMAC key for session cookies. Rotating it (by deleting the
// state file) invalidates every outstanding cookie, which is the intended
// panic button.
//
// This is the one accessor that does not consult the file first: it runs on
// every authenticated request, and the key is written once at Open and never
// again by any command. A refresh from a neighbouring call picks up a
// hand-replaced file soon enough.
func (s *Store) SessionKey() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, _ := base64.StdEncoding.DecodeString(s.d.SessionKey)
	return key
}

// Enrolled reports whether a TOTP authenticator exists. Until it does, the
// whole UI is closed except /setup.
func (s *Store) Enrolled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return s.d.TOTP != nil
}

// TOTPSecret returns the enrolled secret, or "" if setup has not run.
func (s *Store) TOTPSecret() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	if s.d.TOTP == nil {
		return ""
	}
	return s.d.TOTP.Secret
}

// EnrollTOTP persists the authenticator and burns the bootstrap token in one
// write, so a crash cannot leave a usable token behind an enrolled server.
func (s *Store) EnrollTOTP(secret, issuer, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	if s.d.TOTP != nil {
		return errors.New("an authenticator is already enrolled")
	}
	now := time.Now().UTC()
	s.d.TOTP = &TOTP{Secret: secret, Issuer: issuer, Account: account, EnrolledAt: now}
	if s.d.Bootstrap != nil {
		s.d.Bootstrap.UsedAt = &now
	}
	return s.persistLocked()
}

// NewBootstrapToken mints a token, stores its hash, and returns the plaintext
// exactly once. Calling it again replaces any unused token.
func (s *Store) NewBootstrapToken() (string, error) {
	raw, err := randomBytes(32)
	if err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	s.d.Bootstrap = &Bootstrap{Hash: hashString(token), CreatedAt: time.Now().UTC()}
	if err := s.persistLocked(); err != nil {
		return "", err
	}
	return token, nil
}

// BootstrapValid reports whether token matches the live, unused bootstrap
// token. Comparison is constant-time; an unset or already-used token is never
// valid, which is what makes /setup 404 after enrollment.
func (s *Store) BootstrapValid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	if token == "" || s.d.Bootstrap == nil || s.d.Bootstrap.Used() {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashString(token)), []byte(s.d.Bootstrap.Hash)) == 1
}

// BootstrapPending reports whether an unused token exists, for the CLI's
// startup banner.
func (s *Store) BootstrapPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return s.d.Bootstrap != nil && !s.d.Bootstrap.Used()
}

// AddAPIKey mints a key for one grok instance and returns the plaintext once.
func (s *Store) AddAPIKey(name string) (string, APIKey, error) {
	raw, err := randomBytes(32)
	if err != nil {
		return "", APIKey{}, err
	}
	id, err := randomBytes(8)
	if err != nil {
		return "", APIKey{}, err
	}
	plaintext := "glance_sk_" + base64.RawURLEncoding.EncodeToString(raw)
	key := APIKey{
		ID:        hex.EncodeToString(id),
		Name:      name,
		Hash:      hashString(plaintext),
		CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.APIKeys = append(s.d.APIKeys, key)
	if err := s.persistLocked(); err != nil {
		return "", APIKey{}, err
	}
	return plaintext, key, nil
}

// LookupAPIKey resolves a presented key to its record, or nil.
//
// Every stored hash is compared even after a match, so the time taken does not
// reveal which key matched or how many are configured.
func (s *Store) LookupAPIKey(plaintext string) *APIKey {
	if plaintext == "" {
		return nil
	}
	want := []byte(hashString(plaintext))

	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	var found *APIKey
	for i := range s.d.APIKeys {
		if subtle.ConstantTimeCompare(want, []byte(s.d.APIKeys[i].Hash)) == 1 {
			key := s.d.APIKeys[i]
			found = &key
		}
	}
	return found
}

// TouchAPIKey records a successful connection. Best-effort: a failed write
// must not reject an otherwise-valid agent.
func (s *Store) TouchAPIKey(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	now := time.Now().UTC()
	for i := range s.d.APIKeys {
		if s.d.APIKeys[i].ID == id {
			s.d.APIKeys[i].LastSeen = &now
			_ = s.persistLocked()
			return
		}
	}
}

// ListAPIKeys returns the key records for display, with hashes stripped.
//
// Callers only ever print names and dates, so handing them the hash would be
// giving away material for an offline guess in exchange for nothing. Stripping
// it here makes that a property of the API rather than a rule callers must know.
func (s *Store) ListAPIKeys() []APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	out := make([]APIKey, len(s.d.APIKeys))
	copy(out, s.d.APIKeys)
	for i := range out {
		out[i].Hash = ""
	}
	return out
}

// RemoveAPIKey deletes by id or exact name. Returns whether anything matched.
func (s *Store) RemoveAPIKey(idOrName string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	kept := s.d.APIKeys[:0:0]
	removed := false
	for _, k := range s.d.APIKeys {
		if k.ID == idOrName || k.Name == idOrName {
			removed = true
			continue
		}
		kept = append(kept, k)
	}
	if !removed {
		return false, nil
	}
	s.d.APIKeys = kept
	return true, s.persistLocked()
}

// refreshLocked re-reads the file when another process has written it.
//
// `glance apikey add` runs while the server is up, and without this the new key
// would be invisible twice over: the server would keep serving its startup
// snapshot, and its next write would persist that snapshot back over the CLI's
// addition. Treating the file as the source of truth whenever its stamp moves
// fixes both directions, and reduces the remaining race to two processes writing
// in the same instant — which for a one-operator control plane is not a race
// worth a lock file.
//
// A read failure is deliberately silent: the in-memory copy is still the best
// answer available, and refusing to authenticate an agent because a stat failed
// would be a worse outcome than serving slightly stale keys.
func (s *Store) refreshLocked() {
	info, err := os.Stat(s.path)
	if err != nil {
		return
	}
	if info.ModTime().Equal(s.stamp.mod) && info.Size() == s.stamp.size {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var fresh data
	if err := json.Unmarshal(raw, &fresh); err != nil {
		return
	}
	if fresh.Version > Version || fresh.SessionKey == "" {
		// A file we do not understand, or one still being written. Keep what we
		// have rather than signing cookies with a half-read key.
		return
	}
	s.d = fresh
	s.stamp = fileStamp{mod: info.ModTime(), size: info.Size()}
}

// stampLocked records the file as we last left it, so our own writes do not look
// like somebody else's.
func (s *Store) stampLocked() {
	if info, err := os.Stat(s.path); err == nil {
		s.stamp = fileStamp{mod: info.ModTime(), size: info.Size()}
	}
}

// persistLocked writes via a temp file + rename, so a crash mid-write leaves
// the previous state intact rather than a truncated file that would lock the
// operator out of their own server.
func (s *Store) persistLocked() error {
	s.d.Version = Version
	raw, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	s.stampLocked()
	return nil
}

func hashString(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("read random bytes: %w", err)
	}
	return b, nil
}
