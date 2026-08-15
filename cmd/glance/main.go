// Command glance is the grok-glance server and its administrative CLI.
//
//	glance serve [--addr :7717] [--dir ~/.grok/glance] [--insecure-cookie]
//	glance apikey add <name> | list | rm <id-or-name>
//	glance bootstrap        # mint a fresh setup token
//	glance version
//
// One binary, one port, no database. See ARCHITECTURE.md for why.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/user/grok-glance/internal/auth"
	"github.com/user/grok-glance/internal/httpapi"
	"github.com/user/grok-glance/internal/hub"
	"github.com/user/grok-glance/internal/state"
	"github.com/user/grok-glance/web"
)

// version is stamped at build time: `-ldflags "-X main.version=$(git describe)"`.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "glance:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "apikey":
		return apikey(args[1:])
	case "bootstrap":
		return bootstrap(args[1:])
	case "version", "--version", "-v":
		fmt.Println("grok-glance", version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `grok-glance -- remote control for grok build

  glance serve [flags]            run the server
  glance apikey add <name>        mint a key for one grok instance
  glance apikey list              list keys
  glance apikey rm <id-or-name>   revoke a key
  glance bootstrap                mint a fresh setup token
  glance version

serve flags:
  --addr <host:port>    listen address (default 127.0.0.1:7717)
  --dir <path>          state directory (default ~/.grok/glance)
  --insecure-cookie     omit the cookie's Secure flag, for plain-HTTP localhost
`)
}

// openStore resolves the state directory and opens it.
func openStore(dir string) (*state.Store, error) {
	if dir == "" {
		var err error
		dir, err = state.DefaultDir()
		if err != nil {
			return nil, fmt.Errorf("locate state directory: %w", err)
		}
	}
	return state.Open(dir)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:7717", "listen address")
	dir := fs.String("dir", "", "state directory (default ~/.grok/glance)")
	insecureCookie := fs.Bool("insecure-cookie", false, "omit the Secure cookie flag (plain-HTTP localhost only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := openStore(*dir)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Binding to a loopback address is the default because this server is a
	// remote control for a shell agent. Exposing it means exposing that, so it
	// should be a deliberate act -- ideally behind a reverse proxy or a tunnel
	// that terminates TLS, which the cookie's Secure flag assumes.
	if !isLoopback(*addr) && *insecureCookie {
		return errors.New("--insecure-cookie is for plain-HTTP localhost only; " +
			"on a non-loopback address the session cookie must be Secure")
	}

	if !store.Enrolled() {
		if err := announceBootstrap(store, *addr); err != nil {
			return err
		}
	}

	assets, err := web.Assets()
	if err != nil {
		log.Warn("no built web UI in this binary; serving a placeholder", "err", err)
		assets = nil
	}

	server := httpapi.New(httpapi.Options{
		Store: store,
		Auth:  auth.NewManager(store, !*insecureCookie),
		Hub:   hub.New(log),
		Log:   log,
		Web:   assets,
	})

	httpServer := &http.Server{
		Addr:    *addr,
		Handler: server,
		// No WriteTimeout: WebSocket connections are long-lived by design and a
		// write deadline would sever them mid-session. Per-write deadlines are
		// applied inside the hub instead, where they can be scoped to one frame.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr, "state", store.Path())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

// announceBootstrap mints a setup token and puts it where the operator will
// actually see it.
//
// It goes to stderr *and* to a 0600 file, because the two failure modes are
// different: a server started under systemd has no terminal to print to, and a
// server started in a scrollback that has since been cleared has no file to
// recover from unless one was written.
func announceBootstrap(store *state.Store, addr string) error {
	token, err := store.NewBootstrapToken()
	if err != nil {
		return fmt.Errorf("mint bootstrap token: %w", err)
	}

	path := filepath.Join(filepath.Dir(store.Path()), "bootstrap.token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	url := fmt.Sprintf("http://%s/setup?token=%s", displayAddr(addr), token)
	fmt.Fprintf(os.Stderr, `
──────────────────────────────────────────────────────────────────────
  grok-glance is not set up yet.

  Open this once to enroll your authenticator:

    %s

  The token is also in %s.
  It stops working the moment enrollment succeeds.
──────────────────────────────────────────────────────────────────────

`, url, path)
	return nil
}

func bootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	dir := fs.String("dir", "", "state directory (default ~/.grok/glance)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := openStore(*dir)
	if err != nil {
		return err
	}
	if store.Enrolled() {
		// Minting a token here would be a way to re-enroll around a lost phone,
		// which is exactly the door the bootstrap gate exists to keep shut. The
		// recovery path is deliberately physical: delete the state file.
		return errors.New("an authenticator is already enrolled; " +
			"to start over, delete " + store.Path() + " (this also revokes every API key and session)")
	}
	token, err := store.NewBootstrapToken()
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(store.Path()), "bootstrap.token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

func apikey(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: glance apikey add <name> | list | rm <id-or-name>")
	}

	fs := flag.NewFlagSet("apikey", flag.ContinueOnError)
	dir := fs.String("dir", "", "state directory (default ~/.grok/glance)")
	sub := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()

	// Go's flag package stops at the first non-flag argument, so `apikey add foo
	// --dir X` silently leaves --dir unparsed. Saying which mistake was made
	// beats a bare usage line, and checking before openStore keeps a typo from
	// creating a state file in the default directory.
	for _, arg := range rest {
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("flags must come before the name: glance apikey %s %s <name>", sub, arg)
		}
	}
	if (sub == "add" || sub == "rm") && len(rest) != 1 {
		return fmt.Errorf("usage: glance apikey %s <%s>", sub, map[string]string{"add": "name", "rm": "id-or-name"}[sub])
	}

	store, err := openStore(*dir)
	if err != nil {
		return err
	}

	switch sub {
	case "add":
		plaintext, key, err := store.AddAPIKey(rest[0])
		if err != nil {
			return err
		}
		// Printed once and never again: only the hash is stored, so there is no
		// way to recover it later. Say so, rather than let someone discover it.
		fmt.Printf(`Key %q created (id %s).

Add it to ~/.grok/config.toml on the machine running grok:

    [remote_control]
    url     = "ws://127.0.0.1:7717/api/acp/agent"
    api_key = "%s"

Then run /rc in grok.

This is the only time the key is shown.
`, key.Name, key.ID, plaintext)
		return nil

	case "list":
		keys := store.ListAPIKeys()
		if len(keys) == 0 {
			fmt.Println("No API keys. Create one with: glance apikey add <name>")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tCREATED\tLAST SEEN")
		for _, k := range keys {
			seen := "never"
			if k.LastSeen != nil {
				seen = k.LastSeen.Local().Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				k.ID, k.Name, k.CreatedAt.Local().Format(time.RFC3339), seen)
		}
		return w.Flush()

	case "rm":
		removed, err := store.RemoveAPIKey(rest[0])
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("no API key matches %q", rest[0])
		}
		fmt.Printf("Removed %q. Any grok using it will be rejected on its next reconnect.\n", rest[0])
		return nil

	default:
		return fmt.Errorf("unknown apikey command %q", sub)
	}
}

func isLoopback(addr string) bool {
	host, _, found := strings.Cut(addr, ":")
	if !found {
		return false
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

// displayAddr turns a listen address into something clickable.
func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}
