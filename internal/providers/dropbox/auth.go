package dropbox

import (
	"context"

	"golang.org/x/oauth2"

	"cloudup/internal/oauthflow"
	"cloudup/internal/provider"
)

// AuthURLParams/ExchangeParams are aliases of the registry-level
// provider.* types rather than separate structs, so the generic seam and
// this package's own exported functions cannot drift apart.
type AuthURLParams = provider.AuthURLParams
type ExchangeParams = provider.ExchangeParams

// AuthURL builds the consent URL for a new Dropbox connection -
// token_access_type=offline is Dropbox's equivalent of Google's
// access_type=offline: without it the token exchange returns an access
// token only, no refresh token, and unlike Google there is no separate
// option on the exchange itself - it is carried entirely on the
// authorization URL.
func AuthURL(params AuthURLParams) (authURL, state string, err error) {
	cfg := oauthConfig(params.ClientID, params.ClientSecret)
	return oauthflow.AuthURL(oauthflow.Params{
		Name:         Type,
		ClientID:     params.ClientID,
		ClientSecret: params.ClientSecret,
		Scopes:       cfg.Scopes,
		Endpoint:     cfg.Endpoint,
		RedirectURL:  params.RedirectURL,
		AuthCodeOpts: []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("token_access_type", "offline")},
	})
}

// Exchange trades an authorization code (from GET /api/v1/oauth/callback)
// for a refresh token; the caller is responsible for persisting it under
// RefreshTokenKey.
func Exchange(ctx context.Context, params ExchangeParams, code string) (string, error) {
	cfg := oauthConfig(params.ClientID, params.ClientSecret)
	return oauthflow.Exchange(ctx, oauthflow.Params{
		Name:         Type,
		ClientID:     params.ClientID,
		ClientSecret: params.ClientSecret,
		Scopes:       cfg.Scopes,
		Endpoint:     cfg.Endpoint,
		RedirectURL:  params.RedirectURL,
	}, code)
}

// oauthFlow is this provider type's registry-level description of the
// interactive step above - registered in init() (see dropbox.go) alongside
// the factory and the schema, so internal/httpapi can start a Dropbox
// authorization without importing this package.
func oauthFlow() provider.OAuthFlow {
	return provider.OAuthFlow{
		AppCredentialsID: AppCredentialsConnectionID,
		ClientIDKey:      ClientIDKey,
		ClientSecretKey:  ClientSecretKey,
		RefreshTokenKey:  RefreshTokenKey,
		AuthURL:          AuthURL,
		Exchange:         Exchange,
	}
}
