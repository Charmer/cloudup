package dropbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// The shared mechanics (state generation, token exchange, "no refresh
// token returned") are tested once in internal/oauthflow, which now owns
// them. What is left to test here is only what makes Dropbox's flow
// different: token_access_type=offline, without which Dropbox returns an
// access token and no refresh token, leaving every later New call unable
// to authenticate.

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

func TestAuthURLRequestsOfflineTokenAccessType(t *testing.T) {
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
	if got := u.Query().Get("token_access_type"); got != "offline" {
		t.Fatalf("authURL token_access_type = %q, want %q", got, "offline")
	}
	if got := u.Query().Get("redirect_uri"); got != "http://127.0.0.1:3000/api/v1/oauth/callback" {
		t.Fatalf("authURL redirect_uri = %q, want the configured RedirectURL", got)
	}
}

func TestExchangeReturnsRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fake-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "bearer",
			"expires_in":    14400,
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
