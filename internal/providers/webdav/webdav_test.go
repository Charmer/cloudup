package webdav

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/webdav"

	"cloudup/internal/provider"
	"cloudup/internal/provider/providertest"
)

func newTestProvider(t *testing.T) *Provider {
	t.Helper()

	handler := &webdav.Handler{
		FileSystem: webdav.NewMemFS(),
		LockSystem: webdav.NewMemLS(),
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	secrets := providertest.NewMemSecretStore()
	secrets.Set("conn1", secretUsername, "")
	secrets.Set("conn1", secretPassword, "")

	cfg := Config{ConnectionID: "conn1", URL: server.URL}
	rawCfg, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	p, err := New(rawCfg, secrets)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p.(*Provider)
}

// TestUploadSendsBasicAuthPreemptively locks in the fix in
// preemptive_auth.go: credentials must go out on the very first request,
// not after an unauthenticated probe and a 401 challenge - the
// negotiate-then-retry path in gowebdav's default auth races the retry's
// buffered-body replay against the still-in-flight write of the original
// streaming body (see that file's doc comment), which silently lost
// uploads against a real server (Yandex.Disk) that challenges with 401.
func TestUploadSendsBasicAuthPreemptively(t *testing.T) {
	var requests int
	var firstAuthHeader string

	inner := &webdav.Handler{FileSystem: webdav.NewMemFS(), LockSystem: webdav.NewMemLS()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			firstAuthHeader = r.Header.Get("Authorization")
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	secrets := providertest.NewMemSecretStore()
	secrets.Set("conn1", secretUsername, "alice")
	secrets.Set("conn1", secretPassword, "s3cret")

	cfg := Config{ConnectionID: "conn1", URL: server.URL}
	rawCfg, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	p, err := New(rawCfg, secrets)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := p.(*Provider).Upload(context.Background(), provider.UploadTask{
		RemotePath: "a.txt",
		Size:       5,
		Reader:     bytes.NewReader([]byte("hello")),
	}); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if firstAuthHeader != wantAuth {
		t.Fatalf("first request Authorization header = %q, want %q (auth must be sent preemptively, not after a 401 challenge)", firstAuthHeader, wantAuth)
	}
	if requests != 1 {
		t.Fatalf("server saw %d requests for a single PUT, want exactly 1 (no negotiate-then-retry round trip)", requests)
	}
}

func TestUploadDownloadListDelete(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	content := []byte("hello, webdav")
	result, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "/greeting.txt",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo || result.Checksum == "" {
		t.Fatalf("Upload() result missing checksum: %+v", result)
	}

	entries, err := p.List(ctx, "/")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "greeting.txt" {
			found = true
			if e.Size != int64(len(content)) {
				t.Errorf("entry size = %d, want %d", e.Size, len(content))
			}
			if e.Path != "/greeting.txt" {
				t.Errorf("entry path = %q, want %q", e.Path, "/greeting.txt")
			}
		}
	}
	if !found {
		t.Fatal("List() did not return uploaded file")
	}

	var buf bytes.Buffer
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: "/greeting.txt", Writer: &buf}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if buf.String() != string(content) {
		t.Fatalf("Download() content = %q, want %q", buf.String(), content)
	}

	ok, err := p.VerifyChecksum(ctx, "/greeting.txt", result.ChecksumAlgo, result.Checksum)
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if !ok {
		t.Fatal("VerifyChecksum() = false, want true for untouched upload")
	}

	ok, err = p.VerifyChecksum(ctx, "/greeting.txt", result.ChecksumAlgo, "deadbeef")
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if ok {
		t.Fatal("VerifyChecksum() = true, want false for mismatched checksum")
	}

	exists, err := p.Exists(ctx, "/greeting.txt")
	if err != nil || !exists {
		t.Fatalf("Exists() = %v, %v, want true, nil", exists, err)
	}

	if err := p.Delete(ctx, "/greeting.txt"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	exists, err = p.Exists(ctx, "/greeting.txt")
	if err != nil {
		t.Fatalf("Exists() after delete error = %v", err)
	}
	if exists {
		t.Fatal("Exists() = true after Delete(), want false")
	}
}

// TestUploadCreatesMissingParentCollections locks in ensureParentDir:
// without it, PUT to a path whose collections don't exist yet fails
// against a standards-conformant WebDAV server (409 Conflict) - this is
// what "preserve folder structure" uploads need, since the very first file
// under a newly picked local folder always has a brand new remote path.
func TestUploadCreatesMissingParentCollections(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	content := []byte("nested")
	if _, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "/photos/2024/summer/pic1.jpg",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	}); err != nil {
		t.Fatalf("Upload() to a path with missing parents error = %v", err)
	}

	var buf bytes.Buffer
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: "/photos/2024/summer/pic1.jpg", Writer: &buf}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if buf.String() != string(content) {
		t.Fatalf("Download() content = %q, want %q", buf.String(), content)
	}
}

// TestUploadIntoExistingFolderStaysIdempotent pins the fix for gowebdav's
// MkdirAll surfacing an error when the leaf collection already exists (see
// ensureParentDir's doc comment) - a second file landing in a folder a
// prior file already created must not fail the whole upload.
func TestUploadIntoExistingFolderStaysIdempotent(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	for i, name := range []string{"pic1.jpg", "pic2.jpg"} {
		content := []byte{byte(i)}
		if _, err := p.Upload(ctx, provider.UploadTask{
			RemotePath: "/photos/2024/summer/" + name,
			Size:       int64(len(content)),
			Reader:     bytes.NewReader(content),
		}); err != nil {
			t.Fatalf("Upload(%q) error = %v", name, err)
		}
	}

	entries, err := p.List(ctx, "/photos/2024/summer")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() returned %d entries, want 2", len(entries))
	}
}

func TestTestConnection(t *testing.T) {
	p := newTestProvider(t)
	if err := p.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection() error = %v", err)
	}
}

func TestConfigFieldsMarksPasswordSecret(t *testing.T) {
	p := newTestProvider(t)
	fields := p.ConfigFields()
	for _, f := range fields {
		if f.Key == secretPassword && f.Type != provider.FieldPassword {
			t.Fatalf("password field has type %q, want %q", f.Type, provider.FieldPassword)
		}
		// username must also be FieldPassword: New() reads it via
		// secrets.Get(cfg.ConnectionID, secretUsername) below, not from the
		// JSON config, so a caller that routes fields by FieldSpec.Type
		// (as a UI rendering ConfigSchema generically would) must send it
		// to the secret store or New() silently sees an empty username.
		if f.Key == secretUsername && f.Type != provider.FieldPassword {
			t.Fatalf("username field has type %q, want %q", f.Type, provider.FieldPassword)
		}
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	secrets := providertest.NewMemSecretStore()
	if _, err := New(json.RawMessage(`{"url":""}`), secrets); err == nil {
		t.Fatal("New() with empty url: expected error, got nil")
	}
	if _, err := New(json.RawMessage(`not json`), secrets); err == nil {
		t.Fatal("New() with malformed json: expected error, got nil")
	}
}

func TestRemoteURLJoinsPath(t *testing.T) {
	p := newTestProvider(t)
	got := p.remoteURL("/greeting.txt")
	if !strings.HasSuffix(got, "/greeting.txt") {
		t.Fatalf("remoteURL() = %q, want suffix /greeting.txt", got)
	}
}

// TestRemoteURLDoesNotDoubleSlash locks in the path.Join replacement for
// the old hand-rolled joinPath: a base URL with a trailing slash must not
// produce "//" before remotePath.
func TestRemoteURLDoesNotDoubleSlash(t *testing.T) {
	p := newTestProvider(t)
	p.cfg.URL += "/"
	got := p.remoteURL("/greeting.txt")
	if strings.Contains(got, "//greeting.txt") {
		t.Fatalf("remoteURL() = %q, want no double slash before greeting.txt", got)
	}
	if !strings.HasSuffix(got, "/greeting.txt") {
		t.Fatalf("remoteURL() = %q, want suffix /greeting.txt", got)
	}
}
