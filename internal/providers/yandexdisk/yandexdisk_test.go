package yandexdisk

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"cloudup/internal/provider"
	"cloudup/internal/provider/providertest"
)

const testAccessToken = "fake-access-token"

// fakeYandexFile is one file or folder stored in the fake Disk, keyed by its
// full absolute path (e.g. "/a/b/file.txt").
type fakeYandexFile struct {
	isDir    bool
	content  []byte
	md5      string
	modified time.Time
}

// fakeYandexServer implements just enough of the Disk API v1 REST surface -
// disk info, resources (get/list/mkdir/delete), the two-step upload/download
// href indirection - to exercise Provider's CRUD methods against real wire
// traffic, the same way fakeB2Server/the dropbox fake do for their
// providers.
type fakeYandexServer struct {
	mu             sync.Mutex
	files          map[string]*fakeYandexFile
	pendingUploads map[string]string // upload session id -> target path
	nextID         int

	srv *httptest.Server
}

func newFakeYandexServer(t *testing.T) *fakeYandexServer {
	t.Helper()
	f := &fakeYandexServer{
		files:          map[string]*fakeYandexFile{},
		pendingUploads: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/disk/", f.handleDiskInfo)
	mux.HandleFunc("GET /v1/disk/resources", f.handleGetResources)
	mux.HandleFunc("PUT /v1/disk/resources", f.handleMkdir)
	mux.HandleFunc("DELETE /v1/disk/resources", f.handleDeleteResource)
	mux.HandleFunc("GET /v1/disk/resources/upload", f.handleUploadURL)
	mux.HandleFunc("GET /v1/disk/resources/download", f.handleDownloadURL)
	mux.HandleFunc("PUT /upload-href/{id}", f.handleUploadHref)
	mux.HandleFunc("GET /download-href", f.handleDownloadHref)

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// checkAuth requires the exact "OAuth <token>" scheme the real Disk API
// documents - not "Bearer", which is what golang.org/x/oauth2's standard
// Transport would send by trusting the token endpoint's own "token_type"
// field. This is the single most important thing this fake verifies (see
// oauthTransport's doc comment): every test in this file that reaches the
// server at all implicitly re-checks it.
func (f *fakeYandexServer) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	want := "OAuth " + testAccessToken
	if got := r.Header.Get("Authorization"); got != want {
		writeYandexError(w, http.StatusUnauthorized, "UnauthorizedError", fmt.Sprintf("want Authorization %q, got %q", want, got))
		return false
	}
	return true
}

func (f *fakeYandexServer) handleDiskInfo(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_space": 1 << 40, "used_space": 0})
}

func nameOf(path string) string {
	trimmed := strings.TrimSuffix(path, "/")
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		return trimmed[idx+1:]
	}
	return trimmed
}

func parentOf(path string) string {
	trimmed := strings.TrimSuffix(path, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx <= 0 {
		return "/"
	}
	return trimmed[:idx]
}

func resourceTypeOf(isDir bool) string {
	if isDir {
		return "dir"
	}
	return "file"
}

func (f *fakeYandexServer) handleGetResources(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	path := r.URL.Query().Get("path")

	f.mu.Lock()
	defer f.mu.Unlock()

	var ff *fakeYandexFile
	if path == "/" {
		ff = &fakeYandexFile{isDir: true}
	} else {
		var ok bool
		ff, ok = f.files[path]
		if !ok {
			writeYandexError(w, http.StatusNotFound, "DiskNotFoundError", "resource not found")
			return
		}
	}

	resp := map[string]any{
		"name": nameOf(path),
		"path": "disk:" + path,
		"type": resourceTypeOf(ff.isDir),
	}
	if !ff.isDir {
		resp["size"] = len(ff.content)
		resp["md5"] = ff.md5
		resp["mime_type"] = "application/octet-stream"
		resp["modified"] = ff.modified.UTC().Format(time.RFC3339)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 20 // the real API's own default, deliberately not overridden here
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	prefix := path
	if prefix != "/" {
		prefix += "/"
	}
	var children []string
	for p := range f.files {
		if p == path {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if rest == p || strings.Contains(rest, "/") {
			continue // not a direct child
		}
		children = append(children, p)
	}
	sort.Strings(children)

	total := len(children)
	end := offset + limit
	if end > total {
		end = total
	}
	items := []map[string]any{}
	if offset < total {
		for _, p := range children[offset:end] {
			cf := f.files[p]
			item := map[string]any{
				"name": nameOf(p),
				"path": "disk:" + p,
				"type": resourceTypeOf(cf.isDir),
			}
			if !cf.isDir {
				item["size"] = len(cf.content)
				item["md5"] = cf.md5
				item["modified"] = cf.modified.UTC().Format(time.RFC3339)
			}
			items = append(items, item)
		}
	}
	resp["_embedded"] = map[string]any{
		"items": items, "limit": limit, "offset": offset, "total": total,
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMkdir mirrors the two documented 409 cases: the target already
// exists (DiskPathPointsToExistentDirectoryError, which mkdirIdempotent
// must swallow) and the parent is missing (a different, real error).
func (f *fakeYandexServer) handleMkdir(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	path := r.URL.Query().Get("path")

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.files[path]; ok {
		writeYandexError(w, http.StatusConflict, "DiskPathPointsToExistentDirectoryError", "path already exists")
		return
	}
	if parent := parentOf(path); parent != "/" {
		pf, ok := f.files[parent]
		if !ok || !pf.isDir {
			writeYandexError(w, http.StatusConflict, "DiskPathDoesNotExistError", "parent folder does not exist")
			return
		}
	}
	f.files[path] = &fakeYandexFile{isDir: true, modified: time.Now()}
	writeJSON(w, http.StatusCreated, map[string]any{"href": f.srv.URL + "/v1/disk/resources", "method": "GET"})
}

func (f *fakeYandexServer) handleDeleteResource(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	path := r.URL.Query().Get("path")

	f.mu.Lock()
	_, ok := f.files[path]
	delete(f.files, path)
	f.mu.Unlock()

	if !ok {
		writeYandexError(w, http.StatusNotFound, "DiskNotFoundError", "resource not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeYandexServer) handleUploadURL(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	if r.URL.Query().Get("overwrite") != "true" {
		http.Error(w, "expected overwrite=true", http.StatusBadRequest)
		return
	}
	path := r.URL.Query().Get("path")

	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("upload-%04d", f.nextID)
	f.pendingUploads[id] = path
	f.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"href": f.srv.URL + "/upload-href/" + id, "method": "PUT", "templated": false,
	})
}

// handleUploadHref intentionally does not call checkAuth: the real API
// documents that this href needs no additional OAuth token.
func (f *fakeYandexServer) handleUploadHref(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	f.mu.Lock()
	path, ok := f.pendingUploads[id]
	delete(f.pendingUploads, id)
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unknown upload session", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sum := md5.Sum(body)

	f.mu.Lock()
	f.files[path] = &fakeYandexFile{content: body, md5: hex.EncodeToString(sum[:]), modified: time.Now()}
	f.mu.Unlock()

	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeYandexServer) handleDownloadURL(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	path := r.URL.Query().Get("path")

	f.mu.Lock()
	_, ok := f.files[path]
	f.mu.Unlock()
	if !ok {
		writeYandexError(w, http.StatusNotFound, "DiskNotFoundError", "resource not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"href": f.srv.URL + "/download-href?path=" + url.QueryEscape(path), "method": "GET",
	})
}

// handleDownloadHref intentionally does not call checkAuth - see
// handleUploadHref.
func (f *fakeYandexServer) handleDownloadHref(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")

	f.mu.Lock()
	ff, ok := f.files[path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(ff.content)))
	w.WriteHeader(http.StatusOK)
	w.Write(ff.content)
}

func writeYandexError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "description": description})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// newTestProvider builds a Provider whose httpClient is pointed at fake
// instead of the real Yandex API, using a static token source rather than a
// real OAuth exchange - the OAuth mechanics themselves (loopback flow,
// refresh-token default) are covered by auth_test.go and internal/oauthflow.
func newTestProvider(t *testing.T, fake *fakeYandexServer, rootPath string) *Provider {
	t.Helper()
	old := apiBaseURL
	apiBaseURL = fake.srv.URL + "/v1/disk"
	t.Cleanup(func() { apiBaseURL = old })

	client := &http.Client{Transport: &oauthTransport{
		Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: testAccessToken}),
		Base:   fake.srv.Client().Transport,
	}}
	return &Provider{cfg: rawConfig{ConnectionID: "conn1", RootPath: rootPath}, httpClient: client}
}

func TestConfigFields(t *testing.T) {
	fields := (&Provider{}).ConfigFields()
	if len(fields) != 1 || fields[0].Key != "rootPath" {
		t.Fatalf("ConfigFields() = %+v, want a single rootPath field", fields)
	}
	if fields[0].Required {
		t.Error("rootPath should be optional")
	}
}

func TestNewValidation(t *testing.T) {
	secrets := providertest.NewMemSecretStore()

	rawCfg, _ := json.Marshal(rawConfig{ConnectionID: "conn1"})
	if _, err := New(rawCfg, secrets); err == nil {
		t.Fatal("New() with no app-wide OAuth Client ID/Secret configured: expected error, got nil")
	}

	secrets.Set(AppCredentialsConnectionID, secretClientID, "client-id")
	secrets.Set(AppCredentialsConnectionID, secretClientSecret, "client-secret")
	if _, err := New(rawCfg, secrets); err == nil {
		t.Fatal("New() with no refresh token stored: expected error, got nil")
	}

	secrets.Set("conn1", secretRefreshToken, "refresh-token-value")
	p, err := New(rawCfg, secrets)
	if err != nil {
		t.Fatalf("New() with everything present: error = %v", err)
	}
	if p.Type() != Type {
		t.Fatalf("Type() = %q, want %q", p.Type(), Type)
	}
}

func TestDisplayNameFallsBack(t *testing.T) {
	p := &Provider{}
	if got := p.DisplayName(); got != "Yandex.Disk" {
		t.Fatalf("DisplayName() = %q, want %q", got, "Yandex.Disk")
	}
	p.cfg.DisplayName = "Work Disk"
	if got := p.DisplayName(); got != "Work Disk" {
		t.Fatalf("DisplayName() = %q, want %q", got, "Work Disk")
	}
}

func TestTestConnection(t *testing.T) {
	p := newTestProvider(t, newFakeYandexServer(t), "")
	if err := p.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection() error = %v", err)
	}
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	fake := newFakeYandexServer(t)
	p := newTestProvider(t, fake, "")
	ctx := context.Background()

	content := []byte("hello, yandex disk")
	result, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "greeting.txt",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo {
		t.Fatalf("ChecksumAlgo = %q, want %q", result.ChecksumAlgo, checksumAlgo)
	}
	sum := md5.Sum(content)
	if want := hex.EncodeToString(sum[:]); result.Checksum != want {
		t.Fatalf("Checksum = %q, want %q", result.Checksum, want)
	}
	if result.RemotePath != "greeting.txt" {
		t.Fatalf("RemotePath = %q", result.RemotePath)
	}

	var buf bytes.Buffer
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: "greeting.txt", Writer: &buf}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if buf.String() != string(content) {
		t.Fatalf("Download() content = %q, want %q", buf.String(), content)
	}
}

func TestUploadCreatesIntermediateFolders(t *testing.T) {
	fake := newFakeYandexServer(t)
	p := newTestProvider(t, fake, "")
	ctx := context.Background()

	content := []byte("nested content")
	if _, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "a/b/file.txt",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	}); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	entries, err := p.List(ctx, "a")
	if err != nil {
		t.Fatalf("List(a) error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "b" || !entries[0].IsDir {
		t.Fatalf("List(a) = %+v, want a single folder entry named %q", entries, "b")
	}

	entries, err = p.List(ctx, "a/b")
	if err != nil {
		t.Fatalf("List(a/b) error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "file.txt" || entries[0].IsDir {
		t.Fatalf("List(a/b) = %+v, want a single file entry named %q", entries, "file.txt")
	}

	// A second upload under the same nested path must reuse the existing
	// folders (mkdirIdempotent swallowing DiskPathPointsToExistentDirectoryError)
	// rather than failing.
	if _, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "a/b/second.txt",
		Size:       int64(len("more")),
		Reader:     bytes.NewReader([]byte("more")),
	}); err != nil {
		t.Fatalf("second Upload() into the same folders error = %v", err)
	}
}

// TestListPaginatesBeyondDefaultLimit pins the package doc comment's point
// 2: the real API only returns 20 items per page unless "limit" is passed
// explicitly, so a folder with more than 20 files must not appear
// truncated.
func TestListPaginatesBeyondDefaultLimit(t *testing.T) {
	fake := newFakeYandexServer(t)
	p := newTestProvider(t, fake, "")
	ctx := context.Background()

	const fileCount = 45 // > the API's own 20-item default
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("file-%03d.txt", i)
		if _, err := p.Upload(ctx, provider.UploadTask{
			RemotePath: name,
			Size:       int64(len(name)),
			Reader:     bytes.NewReader([]byte(name)),
		}); err != nil {
			t.Fatalf("Upload(%q) error = %v", name, err)
		}
	}

	entries, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != fileCount {
		t.Fatalf("List() returned %d entries, want %d - pagination must not stop at the API's default page size", len(entries), fileCount)
	}
}

func TestDeleteAndExists(t *testing.T) {
	fake := newFakeYandexServer(t)
	p := newTestProvider(t, fake, "")
	ctx := context.Background()

	content := []byte("delete me")
	if _, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "to-delete.txt",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	}); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	exists, err := p.Exists(ctx, "to-delete.txt")
	if err != nil || !exists {
		t.Fatalf("Exists() = %v, %v, want true, nil", exists, err)
	}
	exists, err = p.Exists(ctx, "does-not-exist.txt")
	if err != nil || exists {
		t.Fatalf("Exists() for missing file = %v, %v, want false, nil", exists, err)
	}

	if err := p.Delete(ctx, "to-delete.txt"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	exists, err = p.Exists(ctx, "to-delete.txt")
	if err != nil {
		t.Fatalf("Exists() after delete error = %v", err)
	}
	if exists {
		t.Fatal("Exists() = true after Delete(), want false")
	}

	if err := p.Delete(ctx, "to-delete.txt"); err != nil {
		t.Fatalf("Delete() of already-missing file should be nil, got %v", err)
	}
	if err := p.Delete(ctx, "never-existed.txt"); err != nil {
		t.Fatalf("Delete() of never-existing file should be nil, got %v", err)
	}
}

func TestVerifyChecksum(t *testing.T) {
	fake := newFakeYandexServer(t)
	p := newTestProvider(t, fake, "")
	ctx := context.Background()

	content := []byte("verify me")
	result, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "verify.txt",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	ok, err := p.VerifyChecksum(ctx, "verify.txt", result.ChecksumAlgo, result.Checksum)
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if !ok {
		t.Fatal("VerifyChecksum() = false, want true for untouched upload")
	}

	ok, err = p.VerifyChecksum(ctx, "verify.txt", result.ChecksumAlgo, "deadbeef")
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if ok {
		t.Fatal("VerifyChecksum() = true, want false for mismatched checksum")
	}

	if _, err := p.VerifyChecksum(ctx, "verify.txt", "sha256", "deadbeef"); err == nil {
		t.Fatal("VerifyChecksum() with wrong algo should fail")
	}
}

func TestUploadWithCustomRootPath(t *testing.T) {
	fake := newFakeYandexServer(t)
	p := newTestProvider(t, fake, "cloudup-uploads")
	ctx := context.Background()

	content := []byte("rooted")
	if _, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "file.txt",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	}); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	fake.mu.Lock()
	_, ok := fake.files["/cloudup-uploads/file.txt"]
	fake.mu.Unlock()
	if !ok {
		t.Fatal("uploaded file was not created under the configured root path")
	}
}

// TestOAuthTransportSendsOAuthScheme is a focused regression test for the
// package's single most important reliability detail (see oauthTransport's
// doc comment): Yandex requires the literal "Authorization: OAuth <token>"
// scheme, not "Bearer", regardless of what "token_type" the token endpoint
// reports.
func TestOAuthTransportSendsOAuthScheme(t *testing.T) {
	var gotAuth string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})

	rt := &oauthTransport{
		Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok123", TokenType: "bearer"}),
		Base:   base,
	}
	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if want := "OAuth tok123"; gotAuth != want {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderImplementsOptionalInterfaces(t *testing.T) {
	var _ provider.ExistenceChecker = (*Provider)(nil)
	var _ provider.ChecksumVerifier = (*Provider)(nil)
	var _ provider.ConfigSchema = (*Provider)(nil)
}

func TestJoinRemotePath(t *testing.T) {
	if got := joinRemotePath("", "file.txt"); got != "file.txt" {
		t.Errorf("joinRemotePath(%q, %q) = %q, want %q", "", "file.txt", got, "file.txt")
	}
	if got := joinRemotePath("/a/b/", "file.txt"); got != "a/b/file.txt" {
		t.Errorf("joinRemotePath(%q, %q) = %q, want %q", "/a/b/", "file.txt", got, "a/b/file.txt")
	}
}
