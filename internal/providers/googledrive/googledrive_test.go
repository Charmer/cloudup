package googledrive

import (
	"encoding/json"
	"testing"

	"golang.org/x/oauth2"

	"cloudup/internal/debuglog"
	"cloudup/internal/provider/providertest"
)

func TestNewRejectsInvalidConfig(t *testing.T) {
	secrets := providertest.NewMemSecretStore()
	if _, err := New(json.RawMessage(`not json`), secrets); err == nil {
		t.Fatal("New() with malformed json: expected error, got nil")
	}
	if _, err := New(json.RawMessage(`{}`), secrets); err == nil {
		t.Fatal("New() with no app-wide OAuth Client ID/Secret configured: expected error, got nil")
	}
}

func TestNewRejectsMissingRefreshToken(t *testing.T) {
	secrets := providertest.NewMemSecretStore()
	secrets.Set(AppCredentialsConnectionID, secretClientID, "client-id")
	secrets.Set(AppCredentialsConnectionID, secretClientSecret, "s3cr3t")

	rawCfg, _ := json.Marshal(rawConfig{ConnectionID: "conn1"})
	_, err := New(rawCfg, secrets)
	if err == nil {
		t.Fatal("New() with no refresh token stored: expected error, got nil")
	}
}

func TestNewSucceedsWithStoredRefreshToken(t *testing.T) {
	secrets := providertest.NewMemSecretStore()
	secrets.Set(AppCredentialsConnectionID, secretClientID, "client-id")
	secrets.Set(AppCredentialsConnectionID, secretClientSecret, "s3cr3t")
	secrets.Set("conn1", secretRefreshToken, "refresh-token-value")

	rawCfg, _ := json.Marshal(rawConfig{ConnectionID: "conn1"})
	p, err := New(rawCfg, secrets)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if p.Type() != Type {
		t.Fatalf("Type() = %q, want %q", p.Type(), Type)
	}
}

// TestConfigFieldsExcludesAppWideAndSecretCredentials confirms
// ConfigFields no longer asks for the OAuth Client ID/Secret (moved to an
// app-wide, once-configured location - see the package doc comment) or the
// refresh token (never typed into a form at all).
func TestConfigFieldsExcludesAppWideAndSecretCredentials(t *testing.T) {
	p := &Provider{}
	fields := p.ConfigFields()

	for _, f := range fields {
		switch f.Key {
		case secretClientID, secretClientSecret:
			t.Fatalf("ConfigFields() must not expose %q - it is app-wide, not per-connection", f.Key)
		case secretRefreshToken:
			t.Fatal("ConfigFields() must not expose the refresh token as a form field")
		}
	}
}

func TestDisplayNameFallsBackWhenUnset(t *testing.T) {
	p := &Provider{}
	if got := p.DisplayName(); got != "Google Drive" {
		t.Fatalf("DisplayName() = %q, want %q", got, "Google Drive")
	}

	p.cfg.DisplayName = "Work Drive"
	if got := p.DisplayName(); got != "Work Drive" {
		t.Fatalf("DisplayName() = %q, want %q", got, "Work Drive")
	}
}

func TestSplitRemotePath(t *testing.T) {
	cases := []struct {
		in       string
		wantDirs []string
		wantName string
	}{
		{"file.txt", nil, "file.txt"},
		{"/file.txt", nil, "file.txt"},
		{"a/b/file.txt", []string{"a", "b"}, "file.txt"},
		{"/a/b/", []string{"a"}, "b"},
		{"", nil, ""},
	}
	for _, c := range cases {
		dirs, name := splitRemotePath(c.in)
		if name != c.wantName || !equalStrings(dirs, c.wantDirs) {
			t.Errorf("splitRemotePath(%q) = %v, %q, want %v, %q", c.in, dirs, name, c.wantDirs, c.wantName)
		}
	}
}

func TestJoinRemotePath(t *testing.T) {
	if got := joinRemotePath("", "file.txt"); got != "file.txt" {
		t.Errorf("joinRemotePath(%q, %q) = %q, want %q", "", "file.txt", got, "file.txt")
	}
	if got := joinRemotePath("/a/b/", "file.txt"); got != "a/b/file.txt" {
		t.Errorf("joinRemotePath(%q, %q) = %q, want %q", "/a/b/", "file.txt", got, "a/b/file.txt")
	}
}

func TestQuoteQueryValueEscapesSingleQuotes(t *testing.T) {
	got := quoteQueryValue(`it's a "test"`)
	want := `'it\'s a "test"'`
	if got != want {
		t.Errorf("quoteQueryValue(...) = %q, want %q", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAuthedHTTPClientCarriesDebugLog guards the CLOUDUP_DEBUG wiring.
//
// Regression test for a real gap: debuglog.Transport was originally wired
// into webdav only, so `CLOUDUP_DEBUG=1` produced nothing for this provider
// - exactly the situation where you reach for it, since Drive is the one
// provider with no integration-test coverage of its CRUD methods. The
// transport must sit at the base of the OAuth2 client so it sees both the
// API calls and the token-refresh requests.
func TestAuthedHTTPClientCarriesDebugLog(t *testing.T) {
	client := authedHTTPClient(oauthConfig("id", "secret"), "refresh-token")
	if client == nil {
		t.Fatal("authedHTTPClient returned nil")
	}

	// The oauth2 client's transport wraps ours: oauth2.Transport.Base is the
	// context's HTTPClient transport, which is where debuglog must be.
	ot, ok := client.Transport.(*oauth2.Transport)
	if !ok {
		t.Fatalf("expected an *oauth2.Transport, got %T", client.Transport)
	}
	if _, ok := ot.Base.(debuglog.Transport); !ok {
		t.Errorf("oauth2 transport base is %T, want debuglog.Transport - CLOUDUP_DEBUG would not cover this provider", ot.Base)
	}
}
