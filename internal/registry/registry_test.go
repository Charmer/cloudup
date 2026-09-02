package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"cloudup/internal/provider"
)

type fakeProvider struct{}

func (fakeProvider) Type() string                             { return "fake" }
func (fakeProvider) DisplayName() string                      { return "Fake" }
func (fakeProvider) TestConnection(ctx context.Context) error { return nil }
func (fakeProvider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	return provider.UploadResult{RemotePath: task.RemotePath}, nil
}
func (fakeProvider) Download(ctx context.Context, task provider.DownloadTask) error { return nil }
func (fakeProvider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	return nil, nil
}
func (fakeProvider) Delete(ctx context.Context, remotePath string) error { return nil }

// typeName returns a provider-type identifier unique to this call, so the
// tests survive `go test -count=N`.
//
// The registry is package-global state and Register/RegisterSchema/
// RegisterOAuth deliberately panic on a duplicate type (that panic is itself
// under test below). With fixed names, a second run in the same process
// re-registered the same types and blew up - which meant -count could not be
// used to check this package for flakiness or state leakage.
func typeName(t *testing.T, base string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", base, typeSeq.Add(1))
}

var typeSeq atomic.Uint64

func TestRegisterAndCreate(t *testing.T) {
	fakeType := typeName(t, "fake-test")
	Register(fakeType, func(cfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
		return fakeProvider{}, nil
	})

	p, err := Create(fakeType, nil, nil)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if p.Type() != "fake" {
		t.Fatalf("Type() = %q, want %q", p.Type(), "fake")
	}

	if _, err := Create("does-not-exist", nil, nil); err == nil {
		t.Fatal("Create() with unknown type: expected error, got nil")
	}
}

func TestRegisterSchemaAndSchema(t *testing.T) {
	want := []provider.FieldSpec{{Key: "url", Label: "Server URL", Type: provider.FieldText, Required: true}}
	schemaType := typeName(t, "schema-test")
	RegisterSchema(schemaType, func() []provider.FieldSpec { return want })

	got, err := Schema(schemaType)
	if err != nil {
		t.Fatalf("Schema() error = %v", err)
	}
	if len(got) != 1 || got[0].Key != "url" {
		t.Fatalf("Schema() = %+v, want %+v", got, want)
	}

	if _, err := Schema("does-not-exist"); err == nil {
		t.Fatal("Schema() with unknown type: expected error, got nil")
	}
}

func TestRegisterSchemaDuplicatePanics(t *testing.T) {
	dupSchemaType := typeName(t, "dup-schema-test")
	RegisterSchema(dupSchemaType, func() []provider.FieldSpec { return nil })

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate schema registration")
		}
	}()
	RegisterSchema(dupSchemaType, func() []provider.FieldSpec { return nil })
}

func TestRegisterOAuthAndOAuth(t *testing.T) {
	want := provider.OAuthFlow{
		AppCredentialsID: "oauth-test-app-credentials",
		ClientIDKey:      "clientId",
		ClientSecretKey:  "clientSecret",
		RefreshTokenKey:  "refreshToken",
		AuthURL: func(params provider.AuthURLParams) (string, string, error) {
			return "https://consent.invalid/for-" + params.ClientID, "state-for-" + params.ClientID, nil
		},
		Exchange: func(ctx context.Context, params provider.ExchangeParams, code string) (string, error) {
			return "token-for-" + params.ClientID + "-" + code, nil
		},
	}
	oauthType := typeName(t, "oauth-test")
	RegisterOAuth(oauthType, want)

	got, ok := OAuth(oauthType)
	if !ok {
		t.Fatalf("OAuth(%q) = _, false; want true", oauthType)
	}
	if got.AppCredentialsID != want.AppCredentialsID || got.RefreshTokenKey != want.RefreshTokenKey {
		t.Fatalf("OAuth() = %+v, want %+v", got, want)
	}
	authURL, state, err := got.AuthURL(provider.AuthURLParams{ClientID: "abc"})
	if err != nil {
		t.Fatalf("AuthURL() error = %v", err)
	}
	if authURL != "https://consent.invalid/for-abc" || state != "state-for-abc" {
		t.Fatalf("AuthURL() = (%q, %q), want (%q, %q)", authURL, state, "https://consent.invalid/for-abc", "state-for-abc")
	}
	token, err := got.Exchange(context.Background(), provider.ExchangeParams{ClientID: "abc"}, "code123")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if token != "token-for-abc-code123" {
		t.Fatalf("Exchange() = %q, want %q", token, "token-for-abc-code123")
	}

	if !RequiresOAuth(oauthType) {
		t.Fatalf("RequiresOAuth(%q) = false, want true", oauthType)
	}

	// A type with no registered flow is the normal case, not an error.
	if _, ok := OAuth("does-not-exist"); ok {
		t.Fatal("OAuth() for an unregistered type: expected ok = false")
	}
	if RequiresOAuth("does-not-exist") {
		t.Fatal("RequiresOAuth() for an unregistered type = true, want false")
	}
}

func TestRegisterOAuthDuplicatePanics(t *testing.T) {
	dupOAuthType := typeName(t, "dup-oauth-test")
	RegisterOAuth(dupOAuthType, provider.OAuthFlow{})

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate oauth registration")
		}
	}()
	RegisterOAuth(dupOAuthType, provider.OAuthFlow{})
}

func TestRegisterDuplicatePanics(t *testing.T) {
	dupType := typeName(t, "dup-test")
	Register(dupType, func(cfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
		return fakeProvider{}, nil
	})

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	Register(dupType, func(cfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
		return fakeProvider{}, nil
	})
}
