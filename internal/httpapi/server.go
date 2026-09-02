// Package httpapi exposes cloudup's service layer (internal/config,
// internal/secrets, internal/registry, internal/queue, internal/history,
// internal/settings) as a local REST API. It depends only on those
// packages' public types - never on a concrete provider.
//
// The API is documented in openapi.yaml at the repo root so any HTTP
// client (a Vue frontend, curl, Postman) can be built against it without
// reading this package's source. cmd/server also optionally serves a
// built frontend (frontend/dist) from disk at "/" - see Server.StaticDir -
// so the whole app can run as a single process on a single port with no
// CORS involved, while still letting a developer swap in their own
// frontend build without recompiling Go (see staticHandler's doc comment).
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"cloudup/internal/config"
	"cloudup/internal/history"
	"cloudup/internal/i18n"
	"cloudup/internal/provider"
	"cloudup/internal/queue"
	"cloudup/internal/registry"
	"cloudup/internal/secrets"
	"cloudup/internal/settings"
	"cloudup/internal/watch"
)

// Server bundles every store/manager a handler might need and knows how to
// build an http.Handler for the whole API.
type Server struct {
	Config   *config.Store
	Secrets  *secrets.Store
	History  *history.Store
	Settings *settings.Store
	Queue    *queue.Manager

	// WatchStore persists "watch this local path, upload changes" rules
	// (watches.go); WatchEngine is the live fsnotify-backed engine that
	// actually watches whichever of them are enabled - see
	// internal/watch's package doc comment for why this only ever makes
	// sense against the local filesystem of the machine cmd/server itself
	// runs on. NewServer starts WatchEngine watching every rule that was
	// already enabled when the server started; watches.go's handlers keep
	// it in sync with further changes made over the API.
	WatchStore  *watch.Store
	WatchEngine *watch.Engine

	// Token is the bearer token every request (other than the OpenAPI spec
	// and the health check) must present in an Authorization header. See
	// cmd/server for how it is generated/loaded.
	Token string

	// UploadSpoolDir is where POSTed file bodies are staged before being
	// handed to internal/queue - see uploads.go. Retries reopen this file
	// rather than the original HTTP request body, which is only readable
	// once.
	UploadSpoolDir string

	// StaticDir, if set, is a directory of frontend static files (a Vue
	// `npm run build` output, i.e. frontend/dist) served from disk at "/" -
	// deliberately not embedded into this binary via go:embed, so a
	// developer can rebuild or hand-edit the frontend and just restart the
	// Go process (or not even that, since it's read from disk on every
	// request) instead of recompiling cmd/server. Serving it from the same
	// port keeps browser requests same-origin; it stays a separate build
	// artifact regardless. See cmd/server's -static flag.
	StaticDir string

	// Languages, if set, serves the UI translation catalogs at
	// /api/v1/languages (see languages.go). Nil simply makes those two
	// endpoints report 503 - the API is fully usable without them, since a
	// client can always ship its own strings.
	Languages *i18n.Catalog

	// CORSOrigin, if set, is the single browser origin allowed to call this
	// API cross-origin (e.g. "http://localhost:5173" for Vite's dev
	// server); "*" allows any. Empty - the default - installs no CORS
	// middleware at all, which is correct for both the normal run and a
	// headless service deployment. See withCORS for why this is opt-in.
	CORSOrigin string

	// Version is this build's version string (e.g. "v1.2.3"), set via
	// -ldflags at build time by cmd/server; "dev" for a plain `go build`.
	// UpdateRepo is the "owner/repo" GitHub coordinate handleUpdatesCheck
	// (updates.go) queries for the latest release - see cmd/server's
	// -update-repo flag. Neither is ever read except in direct response to
	// a user clicking "Check for updates" - cloudup never checks on its
	// own.
	Version    string
	UpdateRepo string

	// updateHTTPClient, if set, is used instead of http.DefaultClient for
	// the GitHub call in handleUpdatesCheck - test-only, so tests can point
	// it at an httptest.Server via a custom Transport instead of reaching
	// real GitHub.
	updateHTTPClient *http.Client

	tasks *taskTracker
	oauth *oauthSessions
}

// NewServer wires deps into a Server, starts the background goroutine that
// mirrors queue.Manager.Events() into the in-memory snapshot GET
// /api/v1/tasks polls (see tasks.go - the architecture decided on instead
// of pushing updates over a socket, since any client may be third-party
// code and polling needs no persistent-connection plumbing on either
// side), and resumes every already-enabled rule in watchStore against a
// fresh watch.Engine (see resolveProvider - the same provider-resolution
// path handleUploadCreate uses, just reused here as the closure
// watch.Engine calls whenever a watched file settles).
func NewServer(cfg *config.Store, sec *secrets.Store, hist *history.Store, set *settings.Store, mgr *queue.Manager, watchStore *watch.Store, token, spoolDir string) *Server {
	s := &Server{
		Config:         cfg,
		Secrets:        sec,
		History:        hist,
		Settings:       set,
		Queue:          mgr,
		WatchStore:     watchStore,
		Token:          token,
		UploadSpoolDir: spoolDir,
		tasks:          newTaskTracker(),
		oauth:          newOAuthSessions(),
	}
	go s.tasks.consume(mgr.Events())

	engine, err := watch.NewEngine(mgr, s.resolveProvider, watch.DefaultQuietPeriod)
	if err != nil {
		// fsnotify.NewWatcher() only fails on OS resource exhaustion (too
		// many open file descriptors/inotify instances) - treated the same
		// way languages.go treats a missing languages directory: a broken,
		// clearly logged non-fatal degradation rather than refusing to
		// start the whole server over one subsystem.
		log.Printf("watch: could not start folder-watch engine, that feature will be unavailable: %v", err)
	} else {
		s.WatchEngine = engine
		if rules, err := watchStore.List(); err != nil {
			log.Printf("watch: could not load watch rules: %v", err)
		} else {
			for _, r := range rules {
				if !r.Enabled {
					continue
				}
				if err := engine.Resume(r); err != nil {
					log.Printf("watch: resuming rule %q: %v", r.ID, err)
				}
			}
		}
	}
	return s
}

// resolveProvider resolves connectionID into a ready-to-use
// provider.Provider - the same two-step lookup (config, then
// registry.Create with its secrets) handleUploadCreate needs for an
// ordinary REST upload, extracted here because watch.Engine needs the
// identical thing as a plain function value (watch.ProviderResolver).
func (s *Server) resolveProvider(connectionID string) (provider.Provider, error) {
	conn, err := s.Config.Get(connectionID)
	if err != nil {
		return nil, err
	}
	return registry.Create(conn.ProviderType, conn.ProviderConfig, s.Secrets)
}

// Handler builds the complete routed, authenticated http.Handler.
func (s *Server) Handler(openapiPath string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /openapi.yaml", serveFile(openapiPath))

	// The OAuth callback is hit by the browser after a redirect from the
	// provider (Google/Dropbox/...), which has no way to attach a bearer
	// token - so, like /healthz and /openapi.yaml above, it must sit
	// outside s.authenticate(api) below. Go 1.22+ ServeMux resolves the
	// overlap with the "/api/v1/" prefix mount by most-specific-pattern-
	// wins, so registration order here doesn't matter, but it's grouped
	// with the other unauthenticated routes for readability.
	mux.HandleFunc("GET /api/v1/oauth/callback", s.handleOAuthCallback)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/provider-types", s.handleProviderTypes)
	api.HandleFunc("GET /api/v1/provider-types/{type}/schema", s.handleProviderSchema)

	api.HandleFunc("GET /api/v1/connections", s.handleConnectionsList)
	api.HandleFunc("POST /api/v1/connections", s.handleConnectionsCreate)
	api.HandleFunc("GET /api/v1/connections/{id}", s.handleConnectionsGet)
	api.HandleFunc("PUT /api/v1/connections/{id}", s.handleConnectionsUpdate)
	api.HandleFunc("DELETE /api/v1/connections/{id}", s.handleConnectionsDelete)
	api.HandleFunc("POST /api/v1/connections/{id}/test", s.handleConnectionsTest)

	// Interactive OAuth is keyed by provider type, not hardcoded per
	// provider (these routes used to be /drive/...) - see oauth.go.
	api.HandleFunc("POST /api/v1/connections/{id}/oauth/authorize", s.handleOAuthAuthorizeStart)
	api.HandleFunc("GET /api/v1/connections/{id}/oauth/authorize", s.handleOAuthAuthorizeStatus)
	api.HandleFunc("GET /api/v1/provider-types/{type}/oauth-credentials", s.handleOAuthCredentialsGet)
	api.HandleFunc("PUT /api/v1/provider-types/{type}/oauth-credentials", s.handleOAuthCredentialsSet)

	api.HandleFunc("POST /api/v1/connections/{id}/uploads", s.handleUploadCreate)
	api.HandleFunc("GET /api/v1/tasks", s.handleTasksList)
	api.HandleFunc("GET /api/v1/tasks/{id}", s.handleTasksGet)
	api.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.handleTaskCancel)
	api.HandleFunc("POST /api/v1/connections/{id}/pause", s.handleQueuePause)
	api.HandleFunc("POST /api/v1/connections/{id}/resume", s.handleQueueResume)
	api.HandleFunc("POST /api/v1/connections/{id}/cancel-all", s.handleQueueCancelAll)

	api.HandleFunc("GET /api/v1/history", s.handleHistoryList)
	api.HandleFunc("GET /api/v1/history/{id}", s.handleHistoryGet)
	api.HandleFunc("POST /api/v1/history/{id}/verify", s.handleHistoryVerify)
	api.HandleFunc("DELETE /api/v1/history/{id}", s.handleHistoryDelete)

	api.HandleFunc("GET /api/v1/settings", s.handleSettingsGet)
	api.HandleFunc("PUT /api/v1/settings", s.handleSettingsSet)

	api.HandleFunc("GET /api/v1/watches", s.handleWatchesList)
	api.HandleFunc("POST /api/v1/watches", s.handleWatchesCreate)
	api.HandleFunc("PUT /api/v1/watches/{id}", s.handleWatchesUpdate)
	api.HandleFunc("DELETE /api/v1/watches/{id}", s.handleWatchesDelete)

	api.HandleFunc("GET /api/v1/languages", s.handleLanguagesList)
	api.HandleFunc("GET /api/v1/languages/{code}", s.handleLanguagesGet)

	api.HandleFunc("GET /api/v1/updates/check", s.handleUpdatesCheck)

	mux.Handle("/api/v1/", s.authenticate(api))

	if s.StaticDir != "" {
		mux.Handle("/", s.staticHandler())
	}

	var handler http.Handler = mux
	if s.CORSOrigin != "" {
		handler = withCORS(s.CORSOrigin, handler)
	}
	return withLogging(handler)
}

// staticHandler serves the built frontend from StaticDir. Because the
// reference frontend uses vue-router's hash history (see frontend/src/
// router.js), every real route lives under the "#" fragment the server
// never sees - so there is no SPA-fallback routing to implement here, only
// plain static file serving.
//
// index.html is special-cased: it is read from disk and re-served with a
// small injected script setting window.__CLOUDUP_TOKEN__/__CLOUDUP_BASE_URL__,
// so opening the browser cmd/server launches lands the frontend already
// authenticated against this same server - no copy-pasting the token
// printed to the log. Any frontend served separately (e.g. `npm run dev`)
// simply won't see that global and falls back to asking on the Settings
// page, exactly as before this static-serving mode existed.
func (s *Server) staticHandler() http.Handler {
	fileServer := http.FileServer(http.Dir(s.StaticDir))
	indexPath := filepath.Join(s.StaticDir, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			fileServer.ServeHTTP(w, r)
			return
		}

		data, err := os.ReadFile(indexPath)
		if err != nil {
			http.Error(w, "frontend not built - run `npm run build` in frontend/", http.StatusNotFound)
			return
		}

		injected := fmt.Sprintf(`<script>window.__CLOUDUP_TOKEN__=%q;window.__CLOUDUP_BASE_URL__="";</script></head>`, s.Token)
		data = bytes.Replace(data, []byte("</head>"), []byte(injected), 1)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
}

// withCORS answers cross-origin preflights and tags responses for the one
// origin CORSOrigin names. It is only ever installed when CORSOrigin is
// set (see Handler), and CORSOrigin defaults to empty - because CORS is
// needed in exactly one situation, and is pure attack surface everywhere
// else.
//
// Where it is NOT needed: the normal run, where cmd/server serves the
// built frontend on the same port as the API (see staticHandler), so the
// browser sees same-origin requests and never performs a CORS check at
// all; and every non-browser client (curl, another backend calling this
// service), since CORS is a browser mechanism that nothing else
// implements or consults.
//
// Where it IS needed: running the frontend from Vite's dev server on its
// own port, or hosting a third-party frontend elsewhere. The same-origin
// policy compares scheme+host+port, so localhost:5173 and 127.0.0.1:3000
// are different origins even on one machine, and the browser refuses to
// let the page read the response without these headers.
//
// Note that CORS never protected this API: a hostile page can always
// *send* a request to a loopback port - the same-origin policy only stops
// it from *reading* the reply. What protects the API is the bearer token
// (see authenticate), which such a page has no way to obtain. That is also
// why echoing one configured origin is safe: there is no session cookie
// the browser would attach automatically. Echoing a specific origin rather
// than "*" keeps the door open to Access-Control-Allow-Credentials later,
// which the wildcard form forbids outright, and Vary: Origin keeps caches
// from serving one origin's response to another.
func withCORS(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if origin != "*" {
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestBaseURL reconstructs the origin the browser used to reach this
// request - scheme + host, no path - so handleOAuthAuthorizeStart (oauth.go)
// can build a redirect_uri that points back at this same server. This is
// guaranteed reachable by the browser doing the OAuth consent by
// construction: it's exactly how that browser is talking to us right now,
// whether that's 127.0.0.1:3000 on a desktop run or a real domain behind a
// reverse proxy for a remote deployment. X-Forwarded-Proto/-Host are
// honored when present (the standard convention reverse proxies use to
// tell the backend what the client-facing origin actually was, since the
// backend's own r.Host/r.TLS only describe the hop from the proxy).
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}

	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}

	return scheme + "://" + host
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// authenticate requires a matching "Authorization: Bearer <token>" header
// on every request it wraps. Constant-time comparison isn't used here - a
// single-user local-loopback token isn't a realistic timing-attack target,
// unlike a shared multi-tenant secret - so plain equality is enough.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if len(auth) <= len(prefix) || auth[:len(prefix)] != prefix || auth[len(prefix):] != s.Token {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, lw.status, time.Since(start))
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func serveFile(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ctxWithTimeout is a small helper so every handler applies the same bound
// to provider network calls (TestConnection, Verify, ...) instead of
// running unbounded against a potentially hung remote.
func ctxWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
