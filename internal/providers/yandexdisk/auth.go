package yandexdisk

import (
	"context"

	"cloudup/internal/oauthflow"
	"cloudup/internal/provider"
)

// AuthURLParams/ExchangeParams are aliases of the registry-level
// provider.* types rather than separate structs, so the generic seam and
// this package's own exported functions cannot drift apart.
type AuthURLParams = provider.AuthURLParams
type ExchangeParams = provider.ExchangeParams

// AuthURL builds the consent URL for a new Yandex.Disk connection. Unlike
// Dropbox (token_access_type=offline) or Google (access_type=offline&
// prompt=consent), no extra AuthCodeOpts are passed here: Yandex's
// authorization_code grant issues a refresh token by default (confirmed
// independently of the official reference, which is silent on this
// specific point, by observed real token responses). If that ever turns
// out wrong for some app configuration, oauthflow.Exchange already fails
// with a clear "no refresh token returned" error.
func AuthURL(params AuthURLParams) (authURL, state string, err error) {
	cfg := oauthConfig(params.ClientID, params.ClientSecret)
	return oauthflow.AuthURL(oauthflow.Params{
		Name:         Type,
		ClientID:     params.ClientID,
		ClientSecret: params.ClientSecret,
		Scopes:       cfg.Scopes,
		Endpoint:     cfg.Endpoint,
		RedirectURL:  params.RedirectURL,
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
// interactive step above - registered in init() (see yandexdisk.go)
// alongside the factory and the schema, so internal/httpapi can start a
// Yandex.Disk authorization without importing this package.
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
