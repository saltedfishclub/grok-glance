package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func TestOpenCreatesPrivateStateAndSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	// The file holds the TOTP secret and the cookie-signing key. Group- or
	// world-readable would make every other precaution in this package pointless.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %o, want 600", perm)
	}

	key := append([]byte(nil), store.SessionKey()...)

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if string(reopened.SessionKey()) != string(key) {
		t.Fatal("session key changed across reopen; every cookie would be invalidated")
	}
}

func TestOpenRefusesAFutureVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	blob, _ := json.Marshal(map[string]any{"version": Version + 1})
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	// Silently "upgrading" a file we do not understand would drop fields a newer
	// build wrote. Refusing keeps a downgrade from being destructive.
	if _, err := Open(dir); err == nil {
		t.Fatal("expected a refusal for a newer state file")
	}
}

func TestEnrollmentBurnsTheBootstrapToken(t *testing.T) {
	store := open(t)

	if store.Enrolled() {
		t.Fatal("a fresh store should not be enrolled")
	}
	token, err := store.NewBootstrapToken()
	if err != nil {
		t.Fatalf("NewBootstrapToken: %v", err)
	}
	if !store.BootstrapValid(token) {
		t.Fatal("a freshly minted token should be valid")
	}
	if store.BootstrapValid(token + "x") {
		t.Fatal("a wrong token was accepted")
	}

	if err := store.EnrollTOTP("SECRET", "grok-glance", "operator"); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	if !store.Enrolled() {
		t.Fatal("Enrolled should report true after enrollment")
	}
	// This is the whole point of the gate: once setup succeeds, the token that
	// opened it must never open it again.
	if store.BootstrapValid(token) {
		t.Fatal("the bootstrap token still works after enrollment")
	}
	if store.BootstrapPending() {
		t.Fatal("BootstrapPending should be false after enrollment")
	}
}

func TestBootstrapStateOutlivesTheProcess(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.NewBootstrapToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnrollTOTP("SECRET", "grok-glance", "operator"); err != nil {
		t.Fatal(err)
	}

	// A restart must not reopen the enrollment window.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.BootstrapValid(token) {
		t.Fatal("a spent bootstrap token came back after a restart")
	}
	if !reopened.Enrolled() {
		t.Fatal("enrollment did not persist")
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	store := open(t)

	plaintext, key, err := store.AddAPIKey("laptop")
	if err != nil {
		t.Fatalf("AddAPIKey: %v", err)
	}
	if !strings.HasPrefix(plaintext, "glance_sk_") {
		t.Fatalf("key %q lacks the glance_sk_ prefix that makes it greppable in a leak", plaintext)
	}

	// Only the hash is kept, so a stolen state.json does not yield usable keys.
	blob, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), plaintext) {
		t.Fatal("the plaintext API key was written to disk")
	}

	found := store.LookupAPIKey(plaintext)
	if found == nil || found.ID != key.ID {
		t.Fatalf("LookupAPIKey did not find the key it just minted")
	}
	if store.LookupAPIKey("glance_sk_nonsense") != nil {
		t.Fatal("an unknown key was accepted")
	}
	if store.LookupAPIKey("") != nil {
		t.Fatal("an empty key was accepted")
	}

	store.TouchAPIKey(key.ID)
	keys := store.ListAPIKeys()
	if len(keys) != 1 {
		t.Fatalf("ListAPIKeys returned %d keys, want 1", len(keys))
	}
	if keys[0].LastSeen == nil {
		t.Fatal("TouchAPIKey did not record a last-seen time")
	}
	if keys[0].Hash != "" {
		t.Fatal("ListAPIKeys leaked the stored hash to its caller")
	}

	removed, err := store.RemoveAPIKey("laptop")
	if err != nil || !removed {
		t.Fatalf("RemoveAPIKey by name: removed=%v err=%v", removed, err)
	}
	if store.LookupAPIKey(plaintext) != nil {
		t.Fatal("a revoked key still authenticates")
	}
	removed, err = store.RemoveAPIKey("laptop")
	if err != nil || removed {
		t.Fatalf("removing a missing key should be a no-op: removed=%v err=%v", removed, err)
	}
}

func TestAPIKeysAreDistinct(t *testing.T) {
	store := open(t)
	seen := make(map[string]bool)
	for i := 0; i < 16; i++ {
		plaintext, _, err := store.AddAPIKey("k")
		if err != nil {
			t.Fatal(err)
		}
		if seen[plaintext] {
			t.Fatal("AddAPIKey repeated a key")
		}
		seen[plaintext] = true
	}
}

// `glance apikey add` runs in a second process against the state directory of a
// server that is already up. The server has to see that key without a restart,
// and must not persist its own older snapshot over it afterwards -- so this
// exercises both directions with two Stores on one file.
func TestASecondProcessCanAddKeysToARunningServer(t *testing.T) {
	dir := t.TempDir()

	server, err := Open(dir)
	if err != nil {
		t.Fatalf("Open server: %v", err)
	}
	existing, _, err := server.AddAPIKey("first")
	if err != nil {
		t.Fatal(err)
	}

	cli, err := Open(dir)
	if err != nil {
		t.Fatalf("Open cli: %v", err)
	}
	added, _, err := cli.AddAPIKey("added-while-running")
	if err != nil {
		t.Fatal(err)
	}

	if server.LookupAPIKey(added) == nil {
		t.Fatal("the running server does not see a key added by the CLI")
	}

	// The server writing afterwards must not resurrect its startup snapshot.
	server.TouchAPIKey(server.LookupAPIKey(added).ID)
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.LookupAPIKey(added) == nil {
		t.Fatal("a later server write dropped the CLI's key")
	}
	if reopened.LookupAPIKey(existing) == nil {
		t.Fatal("the CLI's write dropped the server's earlier key")
	}
	if got := len(reopened.ListAPIKeys()); got != 2 {
		t.Fatalf("keys = %d, want 2", got)
	}
}

// The same hazard for `glance bootstrap`: a token minted while the server is up
// has to be accepted by that server.
func TestASecondProcessCanMintABootstrapToken(t *testing.T) {
	dir := t.TempDir()
	server, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if server.BootstrapPending() {
		t.Fatal("a fresh store should have no pending token")
	}

	cli, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	token, err := cli.NewBootstrapToken()
	if err != nil {
		t.Fatal(err)
	}

	if !server.BootstrapPending() {
		t.Fatal("the running server does not see the new token as pending")
	}
	if !server.BootstrapValid(token) {
		t.Fatal("the running server rejects a token the CLI just minted")
	}
}
