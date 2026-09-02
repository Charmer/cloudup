package onedrive

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"golang.org/x/oauth2"

	"cloudup/internal/debuglog"
	"cloudup/internal/provider"
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

// TestConfigFieldsExcludesAppWideAndSecretCredentials confirms ConfigFields
// never asks for the OAuth Client ID/Secret (app-wide, configured once - see
// the package doc comment) or the refresh token (never typed into a form at
// all).
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
	if got := p.DisplayName(); got != "OneDrive" {
		t.Fatalf("DisplayName() = %q, want %q", got, "OneDrive")
	}

	p.cfg.DisplayName = "Work OneDrive"
	if got := p.DisplayName(); got != "Work OneDrive" {
		t.Fatalf("DisplayName() = %q, want %q", got, "Work OneDrive")
	}
}

func TestJoinOnedrivePath(t *testing.T) {
	cases := []struct {
		root, path string
		want       string
	}{
		{"", "", ""},
		{"", "file.txt", "file.txt"},
		{"root", "", "root"},
		{"root", "file.txt", "root/file.txt"},
		{"/root/", "/a/b.txt/", "root/a/b.txt"},
	}
	for _, c := range cases {
		if got := joinOnedrivePath(c.root, c.path); got != c.want {
			t.Errorf("joinOnedrivePath(%q, %q) = %q, want %q", c.root, c.path, got, c.want)
		}
	}
}

func TestItemSegment(t *testing.T) {
	cases := []struct {
		fullPath string
		want     string
	}{
		{"", "/root"},
		{"file.txt", "/root:/file.txt:"},
		{"dir/file.txt", "/root:/dir/file.txt:"},
	}
	for _, c := range cases {
		if got := itemSegment(c.fullPath); got != c.want {
			t.Errorf("itemSegment(%q) = %q, want %q", c.fullPath, got, c.want)
		}
	}
}

func TestDriveBase(t *testing.T) {
	p := &Provider{}
	if got := p.driveBase(); got != graphBaseURL+"/me/drive" {
		t.Errorf("driveBase() with no DriveID = %q, want %q", got, graphBaseURL+"/me/drive")
	}

	p.cfg.DriveID = "b!abc123"
	if got := p.driveBase(); got != graphBaseURL+"/drives/b!abc123" {
		t.Errorf("driveBase() with DriveID = %q, want %q", got, graphBaseURL+"/drives/b!abc123")
	}
}

// TestNewWiresDebugLog guards the CLOUDUP_DEBUG wiring - see dropbox's
// identically named test for why this needs its own regression test rather
// than trusting it stays wired.
func TestNewWiresDebugLog(t *testing.T) {
	secrets := providertest.NewMemSecretStore()
	if err := secrets.Set(AppCredentialsConnectionID, ClientIDKey, "id"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set(AppCredentialsConnectionID, ClientSecretKey, "secret"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set("conn-1", RefreshTokenKey, "refresh-token"); err != nil {
		t.Fatal(err)
	}

	p, err := New(json.RawMessage(`{"connectionId":"conn-1"}`), secrets)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	dp, ok := p.(*Provider)
	if !ok {
		t.Fatalf("New returned %T, want *Provider", p)
	}
	ot, ok := dp.httpClient.Transport.(*oauth2.Transport)
	if !ok {
		t.Fatalf("expected an *oauth2.Transport, got %T", dp.httpClient.Transport)
	}
	if _, ok := ot.Base.(debuglog.Transport); !ok {
		t.Errorf("oauth2 transport base is %T, want debuglog.Transport - CLOUDUP_DEBUG would not cover this provider", ot.Base)
	}
	if dp.httpClient.CheckRedirect == nil {
		t.Error("httpClient.CheckRedirect is nil, want stripAuthOnCrossHostRedirect - the download redirect would leak the bearer token")
	}
}

// TestStripAuthOnCrossHostRedirect pins the redirect-header-stripping policy
// itself, independent of any real HTTP round trip.
func TestStripAuthOnCrossHostRedirect(t *testing.T) {
	mustReq := func(url string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer secret-token")
		return req
	}

	original := mustReq("https://graph.microsoft.com/v1.0/me/drive/root:/a.txt:/content")

	sameHost := mustReq("https://graph.microsoft.com/v1.0/somewhere-else")
	if err := stripAuthOnCrossHostRedirect(sameHost, []*http.Request{original}); err != nil {
		t.Fatalf("same-host redirect: unexpected error %v", err)
	}
	if sameHost.Header.Get("Authorization") == "" {
		t.Error("same-host redirect: Authorization header was stripped, want kept")
	}

	crossHost := mustReq("https://public-blob.example.com/download-me")
	if err := stripAuthOnCrossHostRedirect(crossHost, []*http.Request{original}); err != nil {
		t.Fatalf("cross-host redirect: unexpected error %v", err)
	}
	if got := crossHost.Header.Get("Authorization"); got != "" {
		t.Errorf("cross-host redirect: Authorization header = %q, want stripped", got)
	}

	var via []*http.Request
	for range 10 {
		via = append(via, original)
	}
	if err := stripAuthOnCrossHostRedirect(mustReq("https://graph.microsoft.com/v1.0/x"), via); err == nil {
		t.Error("11th redirect: expected the 10-redirect cap to trigger an error")
	}
}

func TestClampPartSize(t *testing.T) {
	const kib320 = 320 * 1024
	cases := []struct {
		in, want int64
	}{
		{1, kib320},               // below one granule: raised to the minimum
		{kib320, kib320},          // exactly one granule: unchanged
		{kib320 + 1, kib320},      // ragged: rounded down to a whole granule
		{3 * kib320, 3 * kib320},  // already a whole multiple: unchanged
		{100 << 20, 192 * kib320}, // above the 60 MiB guidance: clamped
		{1 << 40, 192 * kib320},   // absurdly large: clamped
	}
	for _, c := range cases {
		if got := clampPartSize(c.in); got != c.want {
			t.Errorf("clampPartSize(%d) = %d, want %d", c.in, got, c.want)
		}
		got := clampPartSize(c.in)
		if got%kib320 != 0 {
			t.Errorf("clampPartSize(%d) = %d, which is not a multiple of 320 KiB - Graph would reject it", c.in, got)
		}
		if got > 60<<20 {
			t.Errorf("clampPartSize(%d) = %d, which exceeds the 60 MiB per-request guidance", c.in, got)
		}
	}
}

func TestUploadMultipartRejectsNonPositivePartSize(t *testing.T) {
	p := &Provider{}
	for _, partSize := range []int64{0, -1} {
		task := provider.UploadTask{RemotePath: "a.bin", Size: 1}
		if _, err := p.UploadMultipart(context.Background(), task, partSize); err == nil {
			t.Fatalf("UploadMultipart() with partSize %d: expected error, got nil", partSize)
		}
	}
}

func TestProviderImplementsMultipartUploader(t *testing.T) {
	var p provider.Provider = &Provider{}
	if _, ok := p.(provider.MultipartUploader); !ok {
		t.Fatal("*Provider does not implement provider.MultipartUploader")
	}
	if _, ok := p.(provider.ParallelMultipartUploader); ok {
		t.Fatal("*Provider implements provider.ParallelMultipartUploader - Graph's upload sessions require ordered chunks, this should not be implemented")
	}
}

func TestProviderImplementsOptionalInterfaces(t *testing.T) {
	var p provider.Provider = &Provider{}
	if _, ok := p.(provider.ChecksumVerifier); !ok {
		t.Fatal("*Provider does not implement provider.ChecksumVerifier")
	}
	if _, ok := p.(provider.ExistenceChecker); !ok {
		t.Fatal("*Provider does not implement provider.ExistenceChecker")
	}
	if _, ok := p.(provider.ConfigSchema); !ok {
		t.Fatal("*Provider does not implement provider.ConfigSchema")
	}
}
