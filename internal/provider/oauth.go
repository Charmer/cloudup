package provider

import "context"

// OAuthFlow describes a provider type whose connections require an
// interactive authorization step before a Provider can be constructed.
//
// This is deliberately *not* an optional interface on Provider (unlike
// everything in features.go): the whole point is that no Provider instance
// exists yet at the time the flow has to run - New cannot build one without
// a refresh token, and the refresh token is what the flow produces. So the
// seam is a value registered per provider type (registry.RegisterOAuth,
// called from the provider's init() next to registry.Register /
// registry.RegisterSchema), which the core resolves by type name exactly
// like it resolves a factory or a schema. Callers (internal/httpapi)
// therefore never import a concrete provider package to start an
// authorization - the same rule that keeps the core free of switch/case
// over provider types.
//
// Two functions, not one blocking Authorize: AuthURL and Exchange mirror
// the two HTTP requests a browser-driven OAuth flow actually is (GET
// .../oauth/authorize starts it, GET .../oauth/callback finishes it) -
// see internal/oauthflow's package doc comment for why this replaced an
// earlier design where a provider ran its own temporary HTTP listener.
type OAuthFlow struct {
	// AppCredentialsID is the pseudo-connection-ID under which this
	// provider type's app-wide OAuth client credentials live in the
	// SecretStore. Not a real config.Connection ID - it never appears in
	// config.json, only as a SecretStore key namespace. It is app-wide
	// because one registered OAuth client covers every connection of that
	// type this server ever makes.
	AppCredentialsID string

	// ClientIDKey, ClientSecretKey and RefreshTokenKey are SecretStore
	// keys. The first two live under AppCredentialsID (shared by every
	// connection of this type); RefreshTokenKey is per-connection, written
	// under the real connection's ID once Exchange has succeeded for it.
	ClientIDKey     string
	ClientSecretKey string
	RefreshTokenKey string

	// AuthURL builds the consent URL to send the user's browser to, plus
	// the state value the caller must remember to route the eventual
	// callback back to the right connection.
	AuthURL func(params AuthURLParams) (authURL, state string, err error)

	// Exchange trades an authorization code (received on the callback
	// route) for a refresh token, which the caller persists under
	// RefreshTokenKey.
	Exchange func(ctx context.Context, params ExchangeParams, code string) (refreshToken string, err error)
}

// AuthURLParams is what a caller starting an interactive flow supplies.
// Intentionally the same shape for every provider type: anything a
// specific provider needs beyond this (scopes, endpoints, the vendor-
// specific "issue a refresh token" request parameter) is baked into its
// own AuthURL adapter, not asked of the caller.
type AuthURLParams struct {
	ClientID     string
	ClientSecret string

	// RedirectURL is cloudup's own callback endpoint - see
	// internal/oauthflow.Params.RedirectURL.
	RedirectURL string
}

// ExchangeParams is what a caller finishing an interactive flow supplies.
// ClientID/ClientSecret/RedirectURL must be the same values AuthURLParams
// used to build the AuthURL the code came from.
type ExchangeParams struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}
