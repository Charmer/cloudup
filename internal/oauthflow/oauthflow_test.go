package oauthflow

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

// fakeTokenServer stands in for a provider's token endpoint so Exchange can
// be tested without any real network access. refreshToken == "" simulates
// the "provider issued no refresh token" case.
func fakeTokenServer(t *testing.T, refreshToken string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		}
		if refreshToken != "" {
			resp["refresh_token"] = refreshToken
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)
	return server
}

func fakeEndpoint(tokenURL string) oauth2.Endpoint {
	return oauth2.Endpoint{AuthURL: "http://unused.invalid/authorize", TokenURL: tokenURL}
}

func TestAuthURLIncludesStateRedirectAndOpts(t *testing.T) {
	authURL, state, err := AuthURL(Params{
		Name:         "faketype",
		ClientID:     "client-id",
		Endpoint:     fakeEndpoint("http://unused.invalid/token"),
		RedirectURL:  "http://127.0.0.1:3000/api/v1/oauth/callback",
		AuthCodeOpts: []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("marker", "present")},
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
	if got := u.Query().Get("state"); got != state {
		t.Errorf("authURL state = %q, want the returned state %q", got, state)
	}
	if got := u.Query().Get("redirect_uri"); got != "http://127.0.0.1:3000/api/v1/oauth/callback" {
		t.Errorf("authURL redirect_uri = %q, want the configured RedirectURL", got)
	}
	if got := u.Query().Get("marker"); got != "present" {
		t.Errorf("authURL marker = %q, want %q (AuthCodeOpts pass-through)", got, "present")
	}
	if got := u.Query().Get("client_id"); got != "client-id" {
		t.Errorf("authURL client_id = %q, want %q", got, "client-id")
	}
}

func TestAuthURLStateIsRandomEachCall(t *testing.T) {
	p := Params{Name: "faketype", Endpoint: fakeEndpoint("http://unused.invalid/token")}
	_, state1, err := AuthURL(p)
	if err != nil {
		t.Fatalf("AuthURL() error = %v", err)
	}
	_, state2, err := AuthURL(p)
	if err != nil {
		t.Fatalf("AuthURL() error = %v", err)
	}
	if state1 == state2 {
		t.Fatalf("AuthURL() returned the same state twice: %q", state1)
	}
}

func TestExchange(t *testing.T) {
	tests := []struct {
		name               string
		serverRefreshToken string
		want               string
		wantErr            bool
	}{
		{name: "success", serverRefreshToken: "test-refresh-token", want: "test-refresh-token"},
		{name: "no refresh token returned", serverRefreshToken: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := fakeTokenServer(t, tc.serverRefreshToken)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := Exchange(ctx, Params{
				Name:         "faketype",
				ClientID:     "client-id",
				ClientSecret: "client-secret",
				Endpoint:     fakeEndpoint(server.URL),
				RedirectURL:  "http://127.0.0.1:3000/api/v1/oauth/callback",
			}, "test-code")
			if tc.wantErr {
				if err == nil {
					t.Fatal("Exchange(): expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Exchange() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("Exchange() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExchangeContextCancelled(t *testing.T) {
	server := fakeTokenServer(t, "test-refresh-token")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Exchange even starts

	_, err := Exchange(ctx, Params{
		Name:     "faketype",
		Endpoint: fakeEndpoint(server.URL),
	}, "test-code")
	if err == nil {
		t.Fatal("Exchange() with a cancelled context: expected error, got nil")
	}
}
