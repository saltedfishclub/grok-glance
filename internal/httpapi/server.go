// Package httpapi is the server's outer edge: routing, authentication
// middleware, the two WebSocket upgrades, and the embedded web UI.
//
// Everything is one origin and one port. The frontend is served from the same
// binary, so there is no CORS story to get wrong and no second thing to deploy.
package httpapi

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/user/grok-glance/internal/auth"
	"github.com/user/grok-glance/internal/hub"
	"github.com/user/grok-glance/internal/state"
)

// Options configures the server.
type Options struct {
	Store *state.Store
	Auth  *auth.Manager
	Hub   *hub.Hub
	Log   *slog.Logger
	// Web is the built frontend, rooted at index.html. Nil serves a plain
	// placeholder page instead, so `go run ./cmd/glance` works before `npm run
	// build` has ever been run.
	Web fs.FS
}

// Server is the HTTP handler tree.
type Server struct {
	opts   Options
	router chi.Router
}

// New wires the routes.
func New(opts Options) *Server {
	s := &Server{opts: opts}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	r.Route("/api", func(r chi.Router) {
		// Open: tells an unauthenticated browser which page to render. It leaks
		// only whether setup has happened, which the /setup 404 reveals anyway.
		r.Get("/status", s.handleStatus)

		// Bootstrap-gated: the token is the only thing standing between a fresh
		// server and whoever reaches the port first.
		r.Group(func(r chi.Router) {
			r.Use(s.requireBootstrap)
			r.Post("/setup/begin", s.handleSetupBegin)
			r.Post("/setup/complete", s.handleSetupComplete)
		})

		r.Post("/login", s.handleLogin)
		r.Post("/logout", s.handleLogout)

		// The agent link authenticates with an API key, not a cookie: it is a
		// program on another machine, not a browser.
		r.Get("/acp/agent", s.handleAgentSocket)

		r.Group(func(r chi.Router) {
			r.Use(s.requireSession)
			r.Get("/agents", s.handleAgents)
			r.Get("/ws", s.handleBrowserSocket)
		})
	})

	r.NotFound(s.serveWeb)
	s.router = r
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// securityHeaders keeps the UI from being framed or sniffed.
//
// The CSP is strict because glance renders agent output — file contents, command
// output, model text — and none of that is trusted markup. `default-src 'self'`
// with no `unsafe-inline` means an injected <script> in a diff cannot execute.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"img-src 'self' data:; "+
				"style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self' ws: wss:; "+
				"frame-ancestors 'none'; "+
				"base-uri 'none'; "+
				"form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.opts.Auth.Authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireBootstrap gates enrollment.
//
// It answers 404 rather than 403 once enrollment is done or the token is wrong:
// a probe should not be able to tell a glance server with a pending setup from
// one that is already configured.
func (s *Server) requireBootstrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Auth.Enrolled() || !s.opts.Auth.BootstrapValid(auth.BootstrapToken(r)) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusResponse struct {
	Enrolled      bool `json:"enrolled"`
	Authenticated bool `json:"authenticated"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, statusResponse{
		Enrolled:      s.opts.Auth.Enrolled(),
		Authenticated: s.opts.Auth.Authenticated(r),
	})
}

func (s *Server) handleSetupBegin(w http.ResponseWriter, r *http.Request) {
	enrollment, err := s.opts.Auth.BeginEnrollment(auth.DescribeEnrollment(r.Host))
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, enrollment)
}

func (s *Server) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Secret string `json:"secret"`
		Code   string `json:"code"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.opts.Auth.CompleteEnrollment(body.Secret, body.Code, auth.DescribeEnrollment(r.Host)); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, auth.ErrBadCode) {
			status = http.StatusUnauthorized
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	// Enrolling logs you in: you have just proved you hold the authenticator,
	// and a login form immediately afterwards would ask for the same proof.
	s.opts.Auth.IssueCookie(w)
	s.opts.Log.Info("authenticator enrolled; bootstrap token is now spent")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.opts.Auth.Login(body.Code); err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrRateLimited) {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	s.opts.Auth.IssueCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.opts.Auth.ClearCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"agents": s.opts.Hub.Summaries()})
}

// handleAgentSocket accepts a grok bridge.
func (s *Server) handleAgentSocket(w http.ResponseWriter, r *http.Request) {
	key := s.opts.Store.LookupAPIKey(auth.BearerToken(r))
	if key == nil {
		// 401 before the upgrade is what makes the bridge give up rather than
		// reconnect forever: it treats a 4xx at upgrade time as a verdict on its
		// credentials, and a retry loop against a rejected key helps nobody.
		s.opts.Log.Warn("rejected agent connection", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin is meaningless here: the peer is grok, not a browser, and
		// it authenticates with a bearer token that a cross-site request could
		// not forge.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionContextTakeover,
	})
	if err != nil {
		s.opts.Log.Warn("agent upgrade failed", "err", err)
		return
	}
	// A busy turn produces large frames (file contents, diffs). The default
	// limit would kill the connection on the first big tool result.
	conn.SetReadLimit(8 << 20)

	s.opts.Store.TouchAPIKey(key.ID)
	s.opts.Hub.ServeAgent(r.Context(), conn, key.ID, key.Name)
}

// handleBrowserSocket accepts a web client.
func (s *Server) handleBrowserSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Browsers do not send Origin on same-origin WebSocket handshakes from
		// the page we served, and the cookie is SameSite=Strict, so a cross-site
		// page cannot open this socket with credentials in the first place.
		OriginPatterns:  []string{r.Host},
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		s.opts.Log.Warn("browser upgrade failed", "err", err)
		return
	}
	conn.SetReadLimit(1 << 20)
	s.opts.Hub.ServeBrowser(r.Context(), conn)
}

// serveWeb serves the embedded SPA.
//
// Unknown paths fall back to index.html so client-side routes survive a reload,
// but /api/* never does: a mistyped API path must 404 as an API path, not return
// HTML that the caller will fail to parse.
func (s *Server) serveWeb(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such endpoint"})
		return
	}
	if s.opts.Web == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(placeholderPage))
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	file, err := s.opts.Web.Open(path)
	if err != nil {
		path = "index.html"
		file, err = s.opts.Web.Open(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	defer file.Close()

	seeker, ok := file.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		http.Error(w, "unreadable asset", http.StatusInternalServerError)
		return
	}
	// Hashed asset filenames are immutable; index.html is not and must be
	// revalidated or a deploy would leave browsers on the old bundle.
	if strings.HasPrefix(path, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, path, time.Time{}, seeker)
}

const placeholderPage = `<!doctype html>
<meta charset="utf-8">
<title>grok-glance</title>
<style>
  body { font: 16px/1.6 ui-sans-serif, system-ui, sans-serif; max-width: 40rem;
         margin: 4rem auto; padding: 0 1.5rem; color: #18181b; background: #fafafa; }
  code { background: #f4f4f5; padding: .15em .4em; border-radius: .25rem; }
  @media (prefers-color-scheme: dark) {
    body { color: #fafafa; background: #18181b; }
    code { background: #27272a; }
  }
</style>
<h1>grok-glance</h1>
<p>The server is running, but the web UI has not been built into this binary.</p>
<p>Run <code>make web</code> (or <code>npm --prefix web install &amp;&amp; npm --prefix web run build</code>),
then rebuild with <code>make build</code>.</p>
<p>For frontend development, run <code>make dev</code> instead: Vite serves the UI on
port 5173 and proxies the API here.</p>
`

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already out; nothing useful is left to do.
		return
	}
}

func decodeJSON(r *http.Request, dst any) error {
	// A bounded reader keeps an unauthenticated POST from being a memory
	// allocation primitive.
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return errors.New("could not read request body")
	}
	return nil
}
