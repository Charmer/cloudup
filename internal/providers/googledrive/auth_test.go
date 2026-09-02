package googledrive

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
// them. What is left to test here is only what makes Google's flow
// different: the two AuthCodeURL options this package's AuthURL adapter
// adds, without which Google returns no refresh token at all
// (access_type=offline) or none on a re-authorization (prompt=consent).

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

func TestAuthURLRequestsOfflineAccess(t *testing.T) {
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
	if got := u.Query().Get("access_type"); got != "offline" {
		t.Fatalf("authURL access_type = %q, want %q", got, "offline")
	}
	if got := u.Query().Get("prompt"); got != "consent" {
		t.Fatalf("authURL prompt = %q, want %q", got, "consent")
	}
	if got := u.Query().Get("redirect_uri"); got != "http://127.0.0.1:3000/api/v1/oauth/callback" {
		t.Fatalf("authURL redirect_uri = %q, want the configured RedirectURL", got)
	}
	// The requested scope must be the same one New's token source uses -
	// they both come from oauthConfig, and this asserts that stays true.
	if got := u.Query().Get("scope"); got != oauthConfig("", "").Scopes[0] {
		t.Fatalf("authURL scope = %q, want %q", got, oauthConfig("", "").Scopes[0])
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
