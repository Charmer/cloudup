package yandexdisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// withFakeOAuthEndpoint points the package's OAuth endpoint at a fake token
// server for the duration of the test, restoring it afterwards - oauthConfig
// is the only thing in this package that reads oauthEndpoint, and both New
// and AuthURL/Exchange go through it.
func withFakeOAuthEndpoint(t *testing.T, tokenURL string) {
	t.Helper()
	original := oauthEndpoint
	oauthEndpoint = oauth2.Endpoint{AuthURL: "http://unused.invalid/authorize", TokenURL: tokenURL}
	t.Cleanup(func() { oauthEndpoint = original })
}

// TestAuthURLCarriesNoExtraOfflineParam covers what makes this provider's
// AuthURL different from dropbox/googledrive's: no extra AuthCodeOption is
// needed to get a refresh token back, because Yandex's authorization_code
// grant issues one by default (see AuthURL's doc comment).
func TestAuthURLCarriesNoExtraOfflineParam(t *testing.T) {
	authURL, state, err := AuthURL(AuthURLParams{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "http://127.0.0.1:3000/api/v1/oauth/callback",
	})
	if err != nil {
		t.Fatalf("AuthURL() error = %v", err)
	}
	if state == "" {
		t.Fatal("AuthURL() returned an empty state")
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing authURL: %v", err)
	}
	if got := u.Query().Get("redirect_uri"); got != "http://127.0.0.1:3000/api/v1/oauth/callback" {
		t.Fatalf("authURL redirect_uri = %q, want the configured RedirectURL", got)
	}
	// Unlike dropbox (token_access_type=offline) or googledrive
	// (access_type=offline&prompt=consent), no such parameter should be
	// present here - see AuthURL's doc comment.
	if got := u.Query().Get("token_access_type"); got != "" {
		t.Errorf("authURL unexpectedly carries token_access_type=%q", got)
	}
	if got := u.Query().Get("access_type"); got != "" {
		t.Errorf("authURL unexpectedly carries access_type=%q", got)
	}
	gotScopes := strings.Fields(u.Query().Get("scope"))
	for _, scope := range oauthScopes {
		if !slices.Contains(gotScopes, scope) {
			t.Errorf("authURL scope %q missing from %q", scope, u.Query().Get("scope"))
		}
	}
}

func TestExchangeSucceedsWithRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fake-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "bearer",
			"expires_in":    3600,
		})
	}))
	defer server.Close()
	withFakeOAuthEndpoint(t, server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	refreshToken, err := Exchange(ctx, ExchangeParams{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "http://127.0.0.1:3000/api/v1/oauth/callback",
	}, "test-code")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if refreshToken != "test-refresh-token" {
		t.Fatalf("Exchange() = %q, want %q", refreshToken, "test-refresh-token")
	}
}

func TestExchangeFailsWithoutRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "bearer",
			"expires_in":   3600,
		})
	}))
	defer server.Close()
	withFakeOAuthEndpoint(t, server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Exchange(ctx, ExchangeParams{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "http://127.0.0.1:3000/api/v1/oauth/callback",
	}, "test-code")
	if err == nil {
		t.Fatal("Exchange() with no refresh_token in the token response should fail, not silently succeed")
	}
}
