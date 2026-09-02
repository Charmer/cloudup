package onedrive

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

// The shared mechanics (state generation, token exchange, "no refresh
// token returned") are tested once in internal/oauthflow, which now owns
// them. What is left to test here is only what makes OneDrive's flow
// different: the offline_access scope, which is what makes Microsoft's v2
// endpoint issue a refresh token on the initial exchange (see auth.go's
// doc comment on AuthURL for why, unlike Google/Dropbox, no special
// AuthCodeOption is needed for this).

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

func TestAuthURLRequestsOfflineAccessScope(t *testing.T) {
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
	scopes := u.Query().Get("scope")
	if scopes == "" {
		t.Fatal("authURL carries no scope parameter")
	}
	if !slices.Contains(strings.Fields(scopes), "offline_access") {
		t.Fatalf("authURL scope = %q, want it to include offline_access", scopes)
	}
}

func TestExchangeReturnsRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fake-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
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
