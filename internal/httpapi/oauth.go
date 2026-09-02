package httpapi

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"cloudup/internal/provider"
	"cloudup/internal/registry"
)

// oauthSession tracks one in-flight (or finished) interactive
// authorization. Unlike an earlier design, nothing blocks on it anymore:
// handleOAuthAuthorizeStart builds the consent URL and returns
// synchronously, and handleOAuthCallback (hit by the browser's redirect
// from the provider, not by the original API caller) finishes the flow
// whenever the user gets there - the client polls
// GET /connections/{id}/oauth/authorize for the outcome, same polling
// approach as task progress (see server.go).
//
// These endpoints are type-generic, not hardcoded per provider: which
// provider types have an interactive step at all is a registry lookup
// (registry.OAuth), so adding a third/fourth/fifth OAuth-based provider
// needs no change here - only the provider's own registry.RegisterOAuth
// call.
type oauthSession struct {
	// state is the value embedded in authURL and echoed back by the
	// provider's redirect - how handleOAuthCallback finds this session
	// from just the query string it receives.
	state string
	// clientID/clientSecret/redirectURL are what Exchange needs once the
	// callback arrives; stashed here since handleOAuthCallback has no
	// other way to know them (the browser's redirect carries only
	// code/state/error).
	clientID, clientSecret, redirectURL string
	flow                                provider.OAuthFlow

	createdAt time.Time

	authURL string
	done    bool
	err     error
}

// sessionTTL bounds how long a started-but-never-finished authorization
// (the user closed the tab, walked away, denied in a way that never
// reached us, ...) stays in memory. Both maps are swept lazily - on the
// next Start or Callback call - rather than by a background ticker: at
// most a handful of these ever exist at once (interactive, one human
// driving it), unlike taskTracker's map, which a long-running server can
// accumulate thousands of entries in under real upload traffic and so
// genuinely needs its own ticker (see tasks.go).
const sessionTTL = 5 * time.Minute

type oauthSessions struct {
	mu       sync.Mutex
	sessions map[string]*oauthSession // connectionID -> session
	byState  map[string]string        // state -> connectionID
}

func newOAuthSessions() *oauthSessions {
	return &oauthSessions{
		sessions: make(map[string]*oauthSession),
		byState:  make(map[string]string),
	}
}

// sweepLocked drops sessions older than sessionTTL. Caller must hold mu.
func (s *oauthSessions) sweepLocked() {
	cutoff := time.Now().Add(-sessionTTL)
	for connectionID, sess := range s.sessions {
		if sess.createdAt.Before(cutoff) {
			delete(s.byState, sess.state)
			delete(s.sessions, connectionID)
		}
	}
}

// resolveFlowForType looks up a provider type's interactive flow, writing
// the HTTP error itself when there is none - "this type needs no
// authorization" is a client mistake (400), not a server condition.
func resolveFlowForType(w http.ResponseWriter, providerType string) (provider.OAuthFlow, bool) {
	if _, err := registry.Schema(providerType); err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return provider.OAuthFlow{}, false
	}
	flow, ok := registry.OAuth(providerType)
	if !ok {
		writeError(w, http.StatusBadRequest, "provider type %q needs no interactive authorization", providerType)
		return provider.OAuthFlow{}, false
	}
	return flow, true
}

// handleOAuthCredentialsGet reports only whether the app-wide client
// credentials for a provider type are present - deliberately never the
// values themselves. They are write-only from the API's point of view: the
// server has no reason to hand a stored client secret back out, and a
// frontend has no reason to display one.
func (s *Server) handleOAuthCredentialsGet(w http.ResponseWriter, r *http.Request) {
	flow, ok := resolveFlowForType(w, r.PathValue("type"))
	if !ok {
		return
	}

	clientID, err := s.Secrets.Get(flow.AppCredentialsID, flow.ClientIDKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"configured": clientID != ""})
}

type oauthCredentialsRequest struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

func (s *Server) handleOAuthCredentialsSet(w http.ResponseWriter, r *http.Request) {
	flow, ok := resolveFlowForType(w, r.PathValue("type"))
	if !ok {
		return
	}

	var req oauthCredentialsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: %s", err)
		return
	}
	if req.ClientID == "" || req.ClientSecret == "" {
		writeError(w, http.StatusBadRequest, "clientId and clientSecret are required")
		return
	}
	if err := s.Secrets.Set(flow.AppCredentialsID, flow.ClientIDKey, req.ClientID); err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	if err := s.Secrets.Set(flow.AppCredentialsID, flow.ClientSecretKey, req.ClientSecret); err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleOAuthAuthorizeStart builds the consent URL and returns immediately
// - no goroutine, no blocking: AuthURL only builds a string, it makes no
// network call. redirectURL is derived from this very request (see
// requestBaseURL), which is what lets the same code work unmodified for a
// desktop run (redirects back to 127.0.0.1) and a remote deployment
// (redirects back to whatever public URL the browser is actually using).
func (s *Server) handleOAuthAuthorizeStart(w http.ResponseWriter, r *http.Request) {
	connectionID := r.PathValue("id")

	conn, err := s.Config.Get(connectionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}
	flow, ok := registry.OAuth(conn.ProviderType)
	if !ok {
		writeError(w, http.StatusBadRequest, "connection %s is a %q connection, which needs no interactive authorization", connectionID, conn.ProviderType)
		return
	}

	clientID, err := s.Secrets.Get(flow.AppCredentialsID, flow.ClientIDKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	clientSecret, err := s.Secrets.Get(flow.AppCredentialsID, flow.ClientSecretKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	if clientID == "" || clientSecret == "" {
		writeError(w, http.StatusBadRequest, "%s OAuth client not configured - PUT /api/v1/provider-types/%s/oauth-credentials first", conn.ProviderType, conn.ProviderType)
		return
	}

	redirectURL := requestBaseURL(r) + "/api/v1/oauth/callback"
	authURL, state, err := flow.AuthURL(provider.AuthURLParams{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}

	s.oauth.mu.Lock()
	s.oauth.sweepLocked()
	s.oauth.sessions[connectionID] = &oauthSession{
		state:        state,
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:  redirectURL,
		flow:         flow,
		createdAt:    time.Now(),
		authURL:      authURL,
	}
	s.oauth.byState[state] = connectionID
	s.oauth.mu.Unlock()

	writeJSON(w, http.StatusAccepted, map[string]string{"authUrl": authURL})
}

func (s *Server) handleOAuthAuthorizeStatus(w http.ResponseWriter, r *http.Request) {
	connectionID := r.PathValue("id")

	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()
	sess, ok := s.oauth.sessions[connectionID]
	if !ok {
		writeError(w, http.StatusNotFound, "no authorization in progress for connection %q", connectionID)
		return
	}

	resp := map[string]any{"authUrl": sess.authURL, "done": sess.done}
	if sess.err != nil {
		resp["error"] = sess.err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleOAuthCallback is what the provider's redirect actually hits, so
// unlike every other handler in this package it is unauthenticated (see
// server.go's Handler) and reached by a browser that just came from
// Google/Dropbox/Yandex/Microsoft, not by an API client holding a bearer
// token. state is looked up in byState rather than trusted as a
// connectionID directly, so a request here can only ever resolve to a
// session this server itself started.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state := query.Get("state")

	s.oauth.mu.Lock()
	s.oauth.sweepLocked()
	connectionID, ok := s.oauth.byState[state]
	var sess *oauthSession
	if ok {
		sess = s.oauth.sessions[connectionID]
		// One-time use: a replayed or manually re-hit callback URL must
		// not re-run Exchange (the provider's authorization code is
		// single-use anyway and would just fail there, but failing here
		// with a clear message is friendlier than an opaque upstream
		// error).
		delete(s.oauth.byState, state)
	}
	s.oauth.mu.Unlock()

	if !ok || sess == nil {
		writeError(w, http.StatusBadRequest, "unknown or expired authorization attempt - go back and click Authorize again")
		return
	}

	finish := func(err error) {
		s.oauth.mu.Lock()
		sess.done = true
		sess.err = err
		s.oauth.mu.Unlock()
	}

	if reason := query.Get("error"); reason != "" {
		finish(fmt.Errorf("authorization denied: %s", reason))
		writeCallbackPage(w, http.StatusOK, "Authorization was not completed. You can close this window and try again.")
		return
	}
	code := query.Get("code")
	if code == "" {
		finish(fmt.Errorf("oauth callback missing code"))
		writeError(w, http.StatusBadRequest, "missing code")
		return
	}

	refreshToken, err := sess.flow.Exchange(r.Context(), provider.ExchangeParams{
		ClientID:     sess.clientID,
		ClientSecret: sess.clientSecret,
		RedirectURL:  sess.redirectURL,
	}, code)
	if err != nil {
		finish(err)
		writeCallbackPage(w, http.StatusOK, "Authorization failed: "+err.Error())
		return
	}

	if err := s.Secrets.Set(connectionID, sess.flow.RefreshTokenKey, refreshToken); err != nil {
		finish(fmt.Errorf("storing refresh token: %w", err))
		writeCallbackPage(w, http.StatusOK, "Authorization succeeded, but saving the result failed. Check the server log and try again.")
		return
	}

	finish(nil)
	writeCallbackPage(w, http.StatusOK, "Authorization complete. You can close this window and return to the application.")
}

func writeCallbackPage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html><body style="font-family:sans-serif;max-width:32rem;margin:4rem auto;text-align:center">%s</body></html>`, message)
}
