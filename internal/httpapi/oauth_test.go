package httpapi

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloudup/internal/provider"
	"cloudup/internal/registry"
	"cloudup/internal/secrets"
)

// callbackURL extracts the state cloudup's own fake AuthURL embedded in
// authUrl and builds a GET .../oauth/callback request to it, the way the
// real provider's redirect would - tests use this instead of talking to
// any real listener, since the callback is now a plain route on the same
// server under test.
func callbackURL(t *testing.T, authURL string, extra url.Values) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing authURL %q: %v", authURL, err)
	}
	q := url.Values{"state": {u.Query().Get("state")}}
	maps.Copy(q, extra)
	return "/api/v1/oauth/callback?" + q.Encode()
}

// clearOAuthAppCredentials removes a flow's app-wide client credentials
// before a test observes the "not configured" state, and again afterwards.
// The mocked keychain is process-global, so without this a test that
// configures credentials would leak into the next one (and into the next
// -count run).
func clearOAuthAppCredentials(t *testing.T, sec *secrets.Store, flow provider.OAuthFlow) {
	t.Helper()
	clear := func() {
		_ = sec.Delete(flow.AppCredentialsID, flow.ClientIDKey)
		_ = sec.Delete(flow.AppCredentialsID, flow.ClientSecretKey)
	}
	clear()
	t.Cleanup(clear)
}

// TestEveryOAuthProviderTypeIsReachableThroughTheAPI is the regression test
// for the audit finding that prompted this suite: the Dropbox provider had
// a complete OAuth flow that no API route led to, so a Dropbox connection
// could be created and never authorized.
//
// It loops over the registry rather than naming googledrive/dropbox on
// purpose: a future provider that registers a flow but forgets the wiring -
// or whose flow is missing a secret-store key - fails here without anyone
// remembering to extend this test.
func TestEveryOAuthProviderTypeIsReachableThroughTheAPI(t *testing.T) {
	env := newTestEnv(t)

	oauthTypes := 0
	for _, providerType := range registry.Types() {
		if !registry.RequiresOAuth(providerType) {
			continue
		}
		oauthTypes++

		t.Run(providerType, func(t *testing.T) {
			flow, ok := registry.OAuth(providerType)
			if !ok {
				t.Fatalf("RequiresOAuth(%q) is true but OAuth(%q) reports no flow", providerType, providerType)
			}
			// A flow with a blank key would write secrets under a
			// nonsense account name, silently.
			if flow.AppCredentialsID == "" || flow.ClientIDKey == "" || flow.ClientSecretKey == "" || flow.RefreshTokenKey == "" {
				t.Fatalf("flow for %q has empty secret-store keys: %+v", providerType, flow)
			}
			if flow.AuthURL == nil || flow.Exchange == nil {
				t.Fatalf("flow for %q is missing AuthURL and/or Exchange", providerType)
			}

			clearOAuthAppCredentials(t, env.Secrets, flow)

			// 1. The credentials endpoint must answer for this type.
			body := decodeBody[map[string]bool](t,
				env.do(http.MethodGet, "/api/v1/provider-types/"+providerType+"/oauth-credentials", nil),
				http.StatusOK)
			if body["configured"] {
				t.Fatalf("credentials for %q report configured before anything was stored", providerType)
			}

			// 2. Starting authorization for a connection of this type must
			// be reachable and must fail with a clear 4xx (not a 404, not a
			// panic, and above all not a real network call) while the app
			// credentials are missing.
			conn := env.createConnection(providerType, "Needs auth", nil, nil)
			rec := env.do(http.MethodPost, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil)
			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("authorize without credentials: status = %d, want a 4xx (body: %s)", rec.Code, rec.Body.String())
			}
			msg := errorMessage(t, rec, rec.Code)
			if !strings.Contains(msg, "not configured") {
				t.Errorf("authorize without credentials: error = %q, want it to say the client is not configured", msg)
			}

			// 3. Storing credentials must work through the API, and be
			// visible to the next GET.
			rec = env.doJSON(http.MethodPut, "/api/v1/provider-types/"+providerType+"/oauth-credentials",
				oauthCredentialsRequest{ClientID: "client-id", ClientSecret: "client-secret"})
			if rec.Code != http.StatusNoContent {
				t.Fatalf("PUT credentials status = %d, want 204 (body: %s)", rec.Code, rec.Body.String())
			}
			body = decodeBody[map[string]bool](t,
				env.do(http.MethodGet, "/api/v1/provider-types/"+providerType+"/oauth-credentials", nil),
				http.StatusOK)
			if !body["configured"] {
				t.Errorf("credentials for %q still report not configured after a successful PUT", providerType)
			}

			// And they landed where the flow says they should.
			if got, _ := env.Secrets.Get(flow.AppCredentialsID, flow.ClientIDKey); got != "client-id" {
				t.Errorf("stored client ID = %q, want it under %s/%s", got, flow.AppCredentialsID, flow.ClientIDKey)
			}
			if got, _ := env.Secrets.Get(flow.AppCredentialsID, flow.ClientSecretKey); got != "client-secret" {
				t.Errorf("stored client secret = %q, want it under %s/%s", got, flow.AppCredentialsID, flow.ClientSecretKey)
			}
		})
	}

	// Sanity check on the loop itself: if the registry reported no OAuth
	// types at all the loop above would pass vacuously.
	if oauthTypes < 3 {
		t.Fatalf("only %d OAuth provider types found; expected the two real ones plus the test fake", oauthTypes)
	}
}

// TestOAuthCredentialsGetNeverReturnsTheValues: the credentials are
// write-only through this API. A stored client secret has no reason to
// travel back to a browser, and the response schema is a bare
// {"configured": bool}.
func TestOAuthCredentialsGetNeverReturnsTheValues(t *testing.T) {
	env := newTestEnv(t)
	flow, _ := registry.OAuth(fakeOAuthType)
	clearOAuthAppCredentials(t, env.Secrets, flow)

	const clientID, clientSecret = "very-identifiable-client-id", "very-identifiable-client-secret"
	if rec := env.doJSON(http.MethodPut, "/api/v1/provider-types/"+fakeOAuthType+"/oauth-credentials",
		oauthCredentialsRequest{ClientID: clientID, ClientSecret: clientSecret}); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT credentials status = %d, want 204", rec.Code)
	}

	rec := env.do(http.MethodGet, "/api/v1/provider-types/"+fakeOAuthType+"/oauth-credentials", nil)
	raw := rec.Body.String()
	if strings.Contains(raw, clientID) || strings.Contains(raw, clientSecret) {
		t.Fatalf("GET oauth-credentials leaked a credential value: %s", raw)
	}
	body := decodeBody[map[string]bool](t, rec, http.StatusOK)
	if len(body) != 1 || !body["configured"] {
		t.Errorf("body = %v, want exactly {\"configured\": true}", body)
	}
}

// TestOAuthCredentialsRejectIncompletePayloads - half a credential pair is
// worse than none, since it would make "configured" report true while the
// flow could never run.
func TestOAuthCredentialsRejectIncompletePayloads(t *testing.T) {
	env := newTestEnv(t)
	flow, _ := registry.OAuth(fakeOAuthType)
	clearOAuthAppCredentials(t, env.Secrets, flow)

	for _, body := range []oauthCredentialsRequest{
		{ClientID: "only-id"},
		{ClientSecret: "only-secret"},
		{},
	} {
		rec := env.doJSON(http.MethodPut, "/api/v1/provider-types/"+fakeOAuthType+"/oauth-credentials", body)
		errorMessage(t, rec, http.StatusBadRequest)
	}
	if got, _ := env.Secrets.Get(flow.AppCredentialsID, flow.ClientIDKey); got != "" {
		t.Errorf("a rejected request still stored a client ID: %q", got)
	}
}

// TestOAuthCredentialsRejectNonOAuthProviderType: asking about the OAuth
// credentials of a type that has no interactive step is a client mistake
// (400 with an explanation), while an unknown type is a 404 - two different
// situations a client must be able to tell apart.
func TestOAuthCredentialsRejectNonOAuthProviderType(t *testing.T) {
	env := newTestEnv(t)

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := env.doJSON(method, "/api/v1/provider-types/webdav/oauth-credentials",
			oauthCredentialsRequest{ClientID: "a", ClientSecret: "b"})
		msg := errorMessage(t, rec, http.StatusBadRequest)
		if !strings.Contains(msg, "webdav") || !strings.Contains(msg, "no interactive authorization") {
			t.Errorf("%s webdav credentials: error = %q, want a clear \"needs no interactive authorization\" message", method, msg)
		}

		rec = env.doJSON(method, "/api/v1/provider-types/no-such-type/oauth-credentials",
			oauthCredentialsRequest{ClientID: "a", ClientSecret: "b"})
		msg = errorMessage(t, rec, http.StatusNotFound)
		if !strings.Contains(msg, "no-such-type") {
			t.Errorf("%s unknown type credentials: error = %q, want it to name the type", method, msg)
		}
	}
}

// TestOAuthAuthorizeFullFlow drives start -> callback -> stored token: the
// consent URL comes back immediately (202, no blocking - AuthURL only
// builds a string), and the token exchange happens when the provider's
// redirect hits GET /api/v1/oauth/callback, exactly as it would coming
// from a real browser. The refresh token lands under the *connection's*
// ID, not the app-wide credentials ID.
func TestOAuthAuthorizeFullFlow(t *testing.T) {
	env := newTestEnv(t)
	flow, _ := registry.OAuth(fakeOAuthType)
	clearOAuthAppCredentials(t, env.Secrets, flow)

	if rec := env.doJSON(http.MethodPut, "/api/v1/provider-types/"+fakeOAuthType+"/oauth-credentials",
		oauthCredentialsRequest{ClientID: "cid", ClientSecret: "csecret"}); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT credentials status = %d, want 204", rec.Code)
	}
	setFakeExchange(t, func(ctx context.Context, params provider.ExchangeParams, code string) (string, error) {
		if params.ClientID != "cid" || params.ClientSecret != "csecret" {
			return "", errors.New("exchange received the wrong credentials")
		}
		if code != "test-code" {
			return "", errors.New("exchange received the wrong code")
		}
		return "the-refresh-token", nil
	})

	conn := env.createConnection(fakeOAuthType, "Authorizing", nil, nil)
	t.Cleanup(func() { _ = env.Secrets.Delete(conn.ID, flow.RefreshTokenKey) })

	started := decodeBody[map[string]string](t,
		env.do(http.MethodPost, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil),
		http.StatusAccepted)
	authURL := started["authUrl"]
	if authURL == "" {
		t.Fatal("authUrl is empty")
	}

	// Still running: the poll endpoint reports the URL and done=false.
	status := decodeBody[map[string]any](t,
		env.do(http.MethodGet, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil),
		http.StatusOK)
	if status["done"] != false {
		t.Errorf("status while running = %v, want done false", status)
	}
	if status["authUrl"] != authURL {
		t.Errorf("status authUrl = %v, want %q", status["authUrl"], authURL)
	}
	if _, hasErr := status["error"]; hasErr {
		t.Errorf("status while running carries an error: %v", status)
	}

	// This is the request the provider's redirect - not the frontend -
	// would actually send.
	rec := env.do(http.MethodGet, callbackURL(t, authURL, url.Values{"code": {"test-code"}}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("callback Content-Type = %q, want text/html (it's rendered directly in the user's browser)", ct)
	}

	final := decodeBody[map[string]any](t,
		env.do(http.MethodGet, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil),
		http.StatusOK)
	if final["done"] != true {
		t.Fatalf("status after callback = %v, want done true", final)
	}
	if _, hasErr := final["error"]; hasErr {
		t.Fatalf("finished status carries an error: %v", final)
	}
	stored, err := env.Secrets.Get(conn.ID, flow.RefreshTokenKey)
	if err != nil {
		t.Fatalf("secrets.Get() error = %v", err)
	}
	if stored != "the-refresh-token" {
		t.Errorf("stored refresh token = %q, want the one the flow produced", stored)
	}
}

// TestOAuthCallbackReportsExchangeFailure: a token exchange that fails must
// surface through the polling endpoint, since the browser hitting the
// callback is not the original API caller polling for the result.
func TestOAuthCallbackReportsExchangeFailure(t *testing.T) {
	env := newTestEnv(t)
	flow, _ := registry.OAuth(fakeOAuthType)
	clearOAuthAppCredentials(t, env.Secrets, flow)

	if rec := env.doJSON(http.MethodPut, "/api/v1/provider-types/"+fakeOAuthType+"/oauth-credentials",
		oauthCredentialsRequest{ClientID: "cid", ClientSecret: "csecret"}); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT credentials status = %d, want 204", rec.Code)
	}
	setFakeExchange(t, func(ctx context.Context, params provider.ExchangeParams, code string) (string, error) {
		return "", errors.New("token endpoint rejected the code")
	})

	conn := env.createConnection(fakeOAuthType, "Will fail", nil, nil)
	t.Cleanup(func() { _ = env.Secrets.Delete(conn.ID, flow.RefreshTokenKey) })

	started := decodeBody[map[string]string](t,
		env.do(http.MethodPost, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil),
		http.StatusAccepted)

	rec := env.do(http.MethodGet, callbackURL(t, started["authUrl"], url.Values{"code": {"test-code"}}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200 (the browser still gets a readable page on failure)", rec.Code)
	}

	final := decodeBody[map[string]any](t,
		env.do(http.MethodGet, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil),
		http.StatusOK)
	msg, _ := final["error"].(string)
	if !strings.Contains(msg, "token endpoint rejected the code") {
		t.Errorf("status error = %q, want the exchange's failure message", msg)
	}
	if stored, _ := env.Secrets.Get(conn.ID, flow.RefreshTokenKey); stored != "" {
		t.Errorf("a failed authorization stored a refresh token: %q", stored)
	}
}

// TestOAuthCallbackHandlesProviderDenial: the provider redirects with an
// "error" query param (no code at all) when the user declines consent -
// this must never reach Exchange.
func TestOAuthCallbackHandlesProviderDenial(t *testing.T) {
	env := newTestEnv(t)
	flow, _ := registry.OAuth(fakeOAuthType)
	clearOAuthAppCredentials(t, env.Secrets, flow)

	if rec := env.doJSON(http.MethodPut, "/api/v1/provider-types/"+fakeOAuthType+"/oauth-credentials",
		oauthCredentialsRequest{ClientID: "cid", ClientSecret: "csecret"}); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT credentials status = %d, want 204", rec.Code)
	}
	setFakeExchange(t, func(ctx context.Context, params provider.ExchangeParams, code string) (string, error) {
		t.Fatal("Exchange must not be called when the callback carries an error param")
		return "", nil
	})

	conn := env.createConnection(fakeOAuthType, "Denied", nil, nil)

	started := decodeBody[map[string]string](t,
		env.do(http.MethodPost, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil),
		http.StatusAccepted)

	rec := env.do(http.MethodGet, callbackURL(t, started["authUrl"], url.Values{"error": {"access_denied"}}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", rec.Code)
	}

	final := decodeBody[map[string]any](t,
		env.do(http.MethodGet, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil),
		http.StatusOK)
	msg, _ := final["error"].(string)
	if !strings.Contains(msg, "access_denied") {
		t.Errorf("status error = %q, want it to mention the denial reason", msg)
	}
}

// TestOAuthCallbackRejectsUnknownState: a request to the callback route
// carrying a state cloudup never issued (forged, or simply never started)
// is a plain 400, not a panic or a 500 - this endpoint is reachable by
// anyone, unauthenticated.
func TestOAuthCallbackRejectsUnknownState(t *testing.T) {
	env := newTestEnv(t)

	msg := errorMessage(t, env.do(http.MethodGet, "/api/v1/oauth/callback?state=never-issued&code=x", nil), http.StatusBadRequest)
	if !strings.Contains(msg, "unknown or expired") {
		t.Errorf("error = %q, want it to say the attempt is unknown or expired", msg)
	}
}

// TestOAuthCallbackRejectsExpiredState: a session older than sessionTTL is
// swept before being looked up, so a very late callback behaves exactly
// like an unknown one rather than silently succeeding long after the user
// gave up.
func TestOAuthCallbackRejectsExpiredState(t *testing.T) {
	env := newTestEnv(t)
	clearOAuthAppCredentials(t, env.Secrets, mustOAuth(t, fakeOAuthType))
	if rec := env.doJSON(http.MethodPut, "/api/v1/provider-types/"+fakeOAuthType+"/oauth-credentials",
		oauthCredentialsRequest{ClientID: "cid", ClientSecret: "csecret"}); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT credentials status = %d, want 204", rec.Code)
	}

	conn := env.createConnection(fakeOAuthType, "Will expire", nil, nil)
	started := decodeBody[map[string]string](t,
		env.do(http.MethodPost, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil),
		http.StatusAccepted)

	// Reach into the session directly (white-box, same package) to
	// backdate it past sessionTTL instead of actually waiting 5 minutes.
	env.Server.oauth.mu.Lock()
	env.Server.oauth.sessions[conn.ID].createdAt = time.Now().Add(-sessionTTL - time.Minute)
	env.Server.oauth.mu.Unlock()

	msg := errorMessage(t, env.do(http.MethodGet, callbackURL(t, started["authUrl"], url.Values{"code": {"x"}}), nil), http.StatusBadRequest)
	if !strings.Contains(msg, "unknown or expired") {
		t.Errorf("error = %q, want it to say the attempt is unknown or expired", msg)
	}
}

// TestOAuthCallbackStateIsSingleUse: replaying (or manually re-hitting) a
// callback URL a second time must not re-run Exchange - the provider's own
// code is single-use anyway, but failing here with a clear message is
// friendlier than an opaque upstream error.
func TestOAuthCallbackStateIsSingleUse(t *testing.T) {
	env := newTestEnv(t)
	flow, _ := registry.OAuth(fakeOAuthType)
	clearOAuthAppCredentials(t, env.Secrets, flow)
	if rec := env.doJSON(http.MethodPut, "/api/v1/provider-types/"+fakeOAuthType+"/oauth-credentials",
		oauthCredentialsRequest{ClientID: "cid", ClientSecret: "csecret"}); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT credentials status = %d, want 204", rec.Code)
	}

	var exchangeCalls int
	setFakeExchange(t, func(ctx context.Context, params provider.ExchangeParams, code string) (string, error) {
		exchangeCalls++
		return "the-refresh-token", nil
	})

	conn := env.createConnection(fakeOAuthType, "Replay", nil, nil)
	t.Cleanup(func() { _ = env.Secrets.Delete(conn.ID, flow.RefreshTokenKey) })

	started := decodeBody[map[string]string](t,
		env.do(http.MethodPost, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil),
		http.StatusAccepted)
	cbURL := callbackURL(t, started["authUrl"], url.Values{"code": {"test-code"}})

	if rec := env.do(http.MethodGet, cbURL, nil); rec.Code != http.StatusOK {
		t.Fatalf("first callback status = %d, want 200", rec.Code)
	}
	errorMessage(t, env.do(http.MethodGet, cbURL, nil), http.StatusBadRequest)

	if exchangeCalls != 1 {
		t.Errorf("Exchange was called %d times, want exactly 1", exchangeCalls)
	}
}

// TestOAuthCallbackRequiresNoAuth: unlike every other /api/v1/* route, the
// callback is reached by the provider's redirect, which cannot attach a
// bearer token - a request with no Authorization header must not 401.
func TestOAuthCallbackRequiresNoAuth(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/oauth/callback?state=whatever&code=x", nil)
	rec := env.serve(req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("callback status = 401, want it reachable without a bearer token (it got %d instead, which is fine - just not 401)", rec.Code)
	}
}

func mustOAuth(t *testing.T, providerType string) provider.OAuthFlow {
	t.Helper()
	flow, ok := registry.OAuth(providerType)
	if !ok {
		t.Fatalf("registry.OAuth(%q): no flow registered", providerType)
	}
	return flow
}

// TestOAuthAuthorizeRejectsNonOAuthConnection: starting authorization for a
// connection whose type has no interactive step is a 400 naming the type,
// not a hang or a 500.
func TestOAuthAuthorizeRejectsNonOAuthConnection(t *testing.T) {
	env := newTestEnv(t)

	conn := env.createConnection(fakeType, "No auth needed", map[string]string{"url": "u"}, nil)
	msg := errorMessage(t, env.do(http.MethodPost, "/api/v1/connections/"+conn.ID+"/oauth/authorize", nil), http.StatusBadRequest)
	if !strings.Contains(msg, fakeType) {
		t.Errorf("error = %q, want it to name the provider type", msg)
	}
}

// TestOAuthAuthorizeStatusUnknownConnection: polling with no authorization
// in progress is a 404 - distinct from "in progress, not done yet".
func TestOAuthAuthorizeStatusUnknownConnection(t *testing.T) {
	env := newTestEnv(t)

	msg := errorMessage(t, env.do(http.MethodGet, "/api/v1/connections/ghost/oauth/authorize", nil), http.StatusNotFound)
	if !strings.Contains(msg, "ghost") {
		t.Errorf("error = %q, want it to name the connection", msg)
	}
}

// TestOAuthAuthorizeUnknownConnectionIs404 - the start endpoint resolves the
// connection first, so a bad ID is a 404 before any flow lookup.
func TestOAuthAuthorizeUnknownConnectionIs404(t *testing.T) {
	env := newTestEnv(t)
	errorMessage(t, env.do(http.MethodPost, "/api/v1/connections/ghost/oauth/authorize", nil), http.StatusNotFound)
}
