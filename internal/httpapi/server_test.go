package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// apiRoutes lists every route Handler registers under /api/v1, with a
// method and a path that is syntactically valid (the referenced connection
// / task / history row need not exist). It is the table behind both the
// auth-middleware sweep and TestEveryAPIRouteIsRegistered.
//
// Keep this in sync with Server.Handler and openapi.yaml: a route added to
// one and not the others is exactly the wiring gap these tests exist to
// surface.
var apiRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v1/provider-types"},
	{http.MethodGet, "/api/v1/provider-types/" + fakeType + "/schema"},
	{http.MethodGet, "/api/v1/provider-types/" + fakeOAuthType + "/oauth-credentials"},
	{http.MethodPut, "/api/v1/provider-types/" + fakeOAuthType + "/oauth-credentials"},

	{http.MethodGet, "/api/v1/connections"},
	{http.MethodPost, "/api/v1/connections"},
	{http.MethodGet, "/api/v1/connections/nope"},
	{http.MethodPut, "/api/v1/connections/nope"},
	{http.MethodDelete, "/api/v1/connections/nope"},
	{http.MethodPost, "/api/v1/connections/nope/test"},
	{http.MethodPost, "/api/v1/connections/nope/oauth/authorize"},
	{http.MethodGet, "/api/v1/connections/nope/oauth/authorize"},
	{http.MethodPost, "/api/v1/connections/nope/uploads"},
	{http.MethodPost, "/api/v1/connections/nope/pause"},
	{http.MethodPost, "/api/v1/connections/nope/resume"},
	{http.MethodPost, "/api/v1/connections/nope/cancel-all"},

	{http.MethodGet, "/api/v1/tasks"},
	{http.MethodGet, "/api/v1/tasks/nope"},
	{http.MethodPost, "/api/v1/tasks/nope/cancel"},

	{http.MethodGet, "/api/v1/history"},
	{http.MethodGet, "/api/v1/history/1"},
	{http.MethodPost, "/api/v1/history/1/verify"},
	{http.MethodDelete, "/api/v1/history/1"},

	{http.MethodGet, "/api/v1/settings"},
	{http.MethodPut, "/api/v1/settings"},

	{http.MethodGet, "/api/v1/watches"},
	{http.MethodPost, "/api/v1/watches"},
	{http.MethodPut, "/api/v1/watches/nope"},
	{http.MethodDelete, "/api/v1/watches/nope"},

	{http.MethodGet, "/api/v1/languages"},
	{http.MethodGet, "/api/v1/languages/en"},
}

// TestAuthRejectsEveryAPIRouteWithoutValidToken sweeps every /api/v1 route
// with every way a bearer token can be wrong. The edge cases mirror what
// authenticate's own length/prefix check implies: an absent header, a bare
// scheme with no token, a wrong scheme, and a wrong value must all be 401
// - and none of them may reach the handler.
func TestAuthRejectsEveryAPIRouteWithoutValidToken(t *testing.T) {
	env := newTestEnv(t)

	headers := map[string]string{
		"absent":           "",
		"empty":            "",
		"scheme only":      "Bearer",
		"scheme and space": "Bearer ",
		"wrong scheme":     "Basic " + env.Token,
		"wrong token":      "Bearer not-the-token",
		"no scheme":        env.Token,
		"lowercase scheme": "bearer " + env.Token,
	}

	for _, route := range apiRoutes {
		for name, header := range headers {
			req := httptest.NewRequest(route.method, route.path, nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := env.serve(req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s Authorization: status = %d, want 401 (body: %s)",
					route.method, route.path, name, rec.Code, rec.Body.String())
				continue
			}
			if msg := errorMessage(t, rec, http.StatusUnauthorized); msg == "" {
				t.Errorf("%s %s with %s Authorization: empty error message", route.method, route.path, name)
			}
		}
	}
}

// TestEveryAPIRouteIsRegistered is the anti-wiring-bug test: with a valid
// token, every route in apiRoutes must be handled by this package (a JSON
// response or a 204), never by ServeMux's own "404 page not found" /
// "405 method not allowed" fallbacks, which come back as text/plain.
//
// This is the shape of the audit finding that motivated this suite: the
// Dropbox OAuth flow existed but no reachable route led to it. A missing or
// mistyped mux pattern fails here regardless of what the handler does.
func TestEveryAPIRouteIsRegistered(t *testing.T) {
	env := newTestEnv(t, func(s *Server) { s.Languages = testCatalog(t) })

	for _, route := range apiRoutes {
		rec := env.do(route.method, route.path, nil)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s: 401 with a valid token", route.method, route.path)
			continue
		}
		if rec.Code == http.StatusNoContent {
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s: status %d, Content-Type %q - looks like an unrouted request (body: %s)",
				route.method, route.path, rec.Code, ct, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// TestHealthzAndOpenAPINeedNoToken pins the two deliberately unauthenticated
// endpoints: a health probe and the API contract itself must be readable by
// a client that does not have the token yet.
func TestHealthzAndOpenAPINeedNoToken(t *testing.T) {
	env := newTestEnv(t)

	rec := env.serve(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	body := decodeBody[map[string]string](t, rec, http.StatusOK)
	if body["status"] != "ok" {
		t.Errorf("healthz body = %v, want status ok", body)
	}

	rec = env.serve(httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.yaml status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "openapi:") {
		t.Errorf("GET /openapi.yaml body = %q, want the spec file's contents", rec.Body.String())
	}
}

// TestNoCORSHeadersWhenOriginUnset locks in that CORS is opt-in. It used to
// be an always-on wildcard; with CORSOrigin empty no middleware may be
// installed at all, so no Access-Control-* header may appear anywhere.
func TestNoCORSHeadersWhenOriginUnset(t *testing.T) {
	env := newTestEnv(t)
	if env.Server.CORSOrigin != "" {
		t.Fatalf("CORSOrigin = %q, want empty by default", env.Server.CORSOrigin)
	}

	targets := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/api/v1/connections"},
		{http.MethodOptions, "/api/v1/connections"},
	}
	for _, target := range targets {
		req := httptest.NewRequest(target.method, target.path, nil)
		req.Header.Set("Origin", "http://evil.example")
		req.Header.Set("Authorization", "Bearer "+env.Token)
		rec := env.serve(req)
		for name := range rec.Header() {
			if strings.HasPrefix(http.CanonicalHeaderKey(name), "Access-Control-") {
				t.Errorf("%s %s: unexpected %s header with CORSOrigin unset", target.method, target.path, name)
			}
		}
	}
}

// TestPreflightNotShortCircuitedWhenOriginUnset: without the CORS
// middleware an OPTIONS request is just another request - it must fall
// through to normal routing and auth rather than being answered 204 by a
// middleware that isn't there.
func TestPreflightNotShortCircuitedWhenOriginUnset(t *testing.T) {
	env := newTestEnv(t)

	rec := env.serve(httptest.NewRequest(http.MethodOptions, "/api/v1/connections", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated OPTIONS status = %d, want 401 (a preflight must not bypass auth)", rec.Code)
	}

	// With a token it still isn't a registered method for that pattern, so
	// the mux answers 405 - the point being that *something routed it*,
	// not that a CORS middleware swallowed it.
	rec = env.do(http.MethodOptions, "/api/v1/connections", nil)
	if rec.Code == http.StatusNoContent {
		t.Errorf("authenticated OPTIONS returned 204 - looks short-circuited by CORS middleware")
	}
}

// TestPreflightEchoesConfiguredOrigin covers the one situation CORS exists
// for (a frontend on Vite's own port): a preflight is answered 204 with the
// configured origin echoed, plus Vary: Origin so a cache cannot serve one
// origin's response to another.
func TestPreflightEchoesConfiguredOrigin(t *testing.T) {
	const origin = "http://localhost:5173"
	env := newTestEnv(t, func(s *Server) { s.CORSOrigin = origin })

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/connections", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	rec := env.serve(req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, origin)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, want it to contain Origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
		t.Errorf("Access-Control-Allow-Headers = %q, want it to allow Authorization", got)
	}
	// A preflight carries no credentials by definition, so it must be
	// answered before the auth middleware - hence 204 above, not 401.
}

// TestWildcardCORSOriginOmitsVary: with "*" there is nothing to vary on,
// and Vary: Origin would only hurt caching.
func TestWildcardCORSOriginOmitsVary(t *testing.T) {
	env := newTestEnv(t, func(s *Server) { s.CORSOrigin = "*" })

	rec := env.do(http.MethodGet, "/api/v1/settings", nil)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Vary"); strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, want no Origin for a wildcard origin", got)
	}
}

// TestStaticIndexInjectsToken pins the mechanism that lets the browser
// cmd/server opens land already authenticated: index.html is rewritten on
// the way out with window.__CLOUDUP_TOKEN__.
func TestStaticIndexInjectsToken(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.html"), "<html><head><title>x</title></head><body>hi</body></html>")
	writeFile(t, filepath.Join(dir, "app.js"), "console.log('asset');")

	env := newTestEnv(t, func(s *Server) { s.StaticDir = dir })

	for _, path := range []string{"/", "/index.html"} {
		rec := env.serve(httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `window.__CLOUDUP_TOKEN__="`+env.Token+`"`) {
			t.Errorf("GET %s body does not carry the injected token: %s", path, body)
		}
		if !strings.Contains(body, `window.__CLOUDUP_BASE_URL__=""`) {
			t.Errorf("GET %s body does not carry the injected base URL: %s", path, body)
		}
		if !strings.Contains(body, "<title>x</title>") {
			t.Errorf("GET %s dropped the original document: %s", path, body)
		}
		if strings.Count(body, "</head>") != 1 {
			t.Errorf("GET %s injected badly (</head> count != 1): %s", path, body)
		}
	}

	// A non-index asset is served byte-for-byte, with no token injected -
	// the token belongs in the HTML entry point only.
	rec := env.serve(httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /app.js status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "console.log('asset');" {
		t.Errorf("GET /app.js body = %q, want the file verbatim", got)
	}
}

// TestStaticServingIsOptional: with StaticDir empty, "/" must not be
// served at all (the API-only mode cmd/server's -static "" selects).
func TestStaticServingIsOptional(t *testing.T) {
	env := newTestEnv(t)
	if env.Server.StaticDir != "" {
		t.Fatalf("StaticDir = %q, want empty by default", env.Server.StaticDir)
	}

	rec := env.serve(httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / status = %d, want 404 with no StaticDir", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "__CLOUDUP_TOKEN__") {
		t.Error("GET / leaked the token with no StaticDir configured")
	}
}

// TestStaticIndexMissingReports404 - a StaticDir pointing at an unbuilt
// frontend must say so rather than serving an empty page or panicking.
func TestStaticIndexMissingReports404(t *testing.T) {
	env := newTestEnv(t, func(s *Server) { s.StaticDir = t.TempDir() })

	rec := env.serve(httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / status = %d, want 404 when index.html is absent", rec.Code)
	}
}
