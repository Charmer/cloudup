// Package oauthflow owns the OAuth2 authorization-code mechanics shared by
// every provider that needs an interactive consent step (currently
// internal/providers/googledrive, dropbox, yandexdisk and onedrive).
//
// It does NOT run its own HTTP server. Earlier versions did - a temporary
// listener on 127.0.0.1:<random port>, in the RFC 8252 "loopback redirect"
// style native/CLI apps use. That broke two ways once cloudup grew real
// deployment modes beyond a desktop app talking to itself: Dropbox (and
// almost certainly Yandex) require an exact, pre-registered redirect URI
// including the port, so a fresh random port every attempt could never
// match; and a random 127.0.0.1 port only means anything on the machine
// that opened it, which isn't the machine running cloudup-server at all
// when the operator is driving a remote/headless deployment's web UI from
// their own laptop. cloudup already runs a permanent HTTP server -
// internal/httpapi - so the redirect URI now points back at that (see
// GET /api/v1/oauth/callback in internal/httpapi/oauth.go), fixed and
// always reachable by construction, instead of a throwaway process.
//
// That split shows up here as two independent functions instead of one
// blocking Run: AuthURL builds the consent URL the browser is sent to,
// Exchange trades the resulting code for a refresh token once httpapi's
// callback handler receives it. Neither touches internal/secrets or
// internal/config - the caller decides where to persist the result, same
// boundary the provider-level Authorize functions always had.
package oauthflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"golang.org/x/oauth2"
)

// Params is what both AuthURL and Exchange need to know about one
// provider's OAuth client.
type Params struct {
	// Name is the provider type (e.g. "googledrive"), used only as the
	// prefix of the errors this package returns.
	Name string

	ClientID     string
	ClientSecret string
	Scopes       []string
	Endpoint     oauth2.Endpoint

	// RedirectURL is cloudup's own callback endpoint
	// (<base URL>/api/v1/oauth/callback) - fixed per server, built by the
	// caller from the incoming request, not by this package.
	RedirectURL string

	// AuthCodeOpts are extra options for the authorization URL. This is
	// where the per-provider "please issue a refresh token" knob goes -
	// oauth2.AccessTypeOffline for Google, a token_access_type=offline
	// query parameter for Dropbox - since there is no cross-provider
	// spelling of it. Only read by AuthURL.
	AuthCodeOpts []oauth2.AuthCodeOption
}

func (p Params) config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Scopes:       p.Scopes,
		Endpoint:     p.Endpoint,
		RedirectURL:  p.RedirectURL,
	}
}

// AuthURL builds the URL to send the user's browser to, along with the
// random state value embedded in it. The caller is responsible for
// remembering which state belongs to which pending authorization, so it
// can route the eventual callback (see internal/httpapi/oauth.go).
func AuthURL(p Params) (authURL, state string, err error) {
	state, err = randomState(p.Name)
	if err != nil {
		return "", "", err
	}
	return p.config().AuthCodeURL(state, p.AuthCodeOpts...), state, nil
}

// Exchange trades an authorization code for a refresh token. p must carry
// the same RedirectURL that was used to build the AuthURL this code came
// from - OAuth2 requires it to match on both calls.
func Exchange(ctx context.Context, p Params, code string) (string, error) {
	token, err := p.config().Exchange(ctx, code)
	if err != nil {
		return "", fmt.Errorf("%s: exchanging authorization code: %w", p.Name, err)
	}
	if token.RefreshToken == "" {
		return "", fmt.Errorf("%s: no refresh token returned - revoke this app's access in your account settings and try again", p.Name)
	}
	return token.RefreshToken, nil
}

func randomState(name string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("%s: generating oauth state: %w", name, err)
	}
	return hex.EncodeToString(buf), nil
}
