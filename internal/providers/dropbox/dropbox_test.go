package dropbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
	if got := p.DisplayName(); got != "Dropbox" {
		t.Fatalf("DisplayName() = %q, want %q", got, "Dropbox")
	}

	p.cfg.DisplayName = "Work Dropbox"
	if got := p.DisplayName(); got != "Work Dropbox" {
		t.Fatalf("DisplayName() = %q, want %q", got, "Work Dropbox")
	}
}

func TestJoinDropboxPath(t *testing.T) {
	cases := []struct {
		root, path string
		want       string
	}{
		{"", "", ""},
		{"", "file.txt", "/file.txt"},
		{"root", "", "/root"},
		{"root", "file.txt", "/root/file.txt"},
		{"/root/", "/a/b.txt/", "/root/a/b.txt"},
	}
	for _, c := range cases {
		if got := joinDropboxPath(c.root, c.path); got != c.want {
			t.Errorf("joinDropboxPath(%q, %q) = %q, want %q", c.root, c.path, got, c.want)
		}
	}
}

// fakeDropboxServer is a minimal fake of the Dropbox JSON + content APIs,
// backed by an in-memory file map, good enough to exercise this package's
// Upload/Download/Delete/Exists/VerifyChecksum against real HTTP
// round-trips without touching the real Dropbox API. List pagination is
// covered separately in TestListPagination with its own dedicated fake,
// since it needs a fixed two-page cursor sequence rather than a live file
// map.
type fakeDropboxServer struct {
	files map[string][]byte // path -> content

	// sessions holds the bytes accumulated so far per upload_session ID, and
	// sessionSeq numbers newly created sessions. chunkSizes records the size
	// of every chunk each session received, in arrival order, so tests can
	// assert how the payload was actually split.
	sessions   map[string][]byte
	chunkSizes map[string][]int
	sessionSeq int
}

func newFakeDropboxServer(t *testing.T) (*fakeDropboxServer, *httptest.Server) {
	t.Helper()
	fake := &fakeDropboxServer{
		files:      map[string][]byte{},
		sessions:   map[string][]byte{},
		chunkSizes: map[string][]int{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/2/users/get_current_account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"account_id":"dbid:fake"}`)
	})
	mux.HandleFunc("/2/files/upload", func(w http.ResponseWriter, r *http.Request) {
		var arg uploadAPIArg
		if err := json.Unmarshal([]byte(r.Header.Get("Dropbox-API-Arg")), &arg); err != nil {
			http.Error(w, "bad arg", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		fake.files[arg.Path] = body
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"id:fake","path_display":%q,"size":%d}`, arg.Path, len(body))
	})
	// The three upload_session/* endpoints below deliberately validate the
	// cursor offset against the bytes actually received so far and reject a
	// mismatch, because an off-by-one in those offsets is the most likely
	// real bug in chunked uploading - and one the real Dropbox API would
	// reject mid-session, after part of the file was already sent.
	mux.HandleFunc("/2/files/upload_session/start", func(w http.ResponseWriter, r *http.Request) {
		var arg uploadSessionStartArg
		if err := json.Unmarshal([]byte(r.Header.Get("Dropbox-API-Arg")), &arg); err != nil {
			http.Error(w, "bad arg", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		fake.sessionSeq++
		id := fmt.Sprintf("session-%d", fake.sessionSeq)
		fake.sessions[id] = body
		fake.chunkSizes[id] = []int{len(body)}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"session_id":%q}`, id)
	})
	mux.HandleFunc("/2/files/upload_session/append_v2", func(w http.ResponseWriter, r *http.Request) {
		var arg uploadSessionAppendArg
		if err := json.Unmarshal([]byte(r.Header.Get("Dropbox-API-Arg")), &arg); err != nil {
			http.Error(w, "bad arg", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		if !fake.checkCursor(w, arg.Cursor) {
			return
		}
		fake.sessions[arg.Cursor.SessionID] = append(fake.sessions[arg.Cursor.SessionID], body...)
		fake.chunkSizes[arg.Cursor.SessionID] = append(fake.chunkSizes[arg.Cursor.SessionID], len(body))
		// append_v2 answers 200 with an empty body.
	})
	mux.HandleFunc("/2/files/upload_session/finish", func(w http.ResponseWriter, r *http.Request) {
		var arg uploadSessionFinishArg
		if err := json.Unmarshal([]byte(r.Header.Get("Dropbox-API-Arg")), &arg); err != nil {
			http.Error(w, "bad arg", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		if !fake.checkCursor(w, arg.Cursor) {
			return
		}
		content := append(fake.sessions[arg.Cursor.SessionID], body...)
		fake.chunkSizes[arg.Cursor.SessionID] = append(fake.chunkSizes[arg.Cursor.SessionID], len(body))
		delete(fake.sessions, arg.Cursor.SessionID)
		if arg.Commit.Path == "" || arg.Commit.Mode != "overwrite" {
			http.Error(w, "bad commit info", http.StatusBadRequest)
			return
		}
		fake.files[arg.Commit.Path] = content
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"id:fake","path_display":%q,"size":%d}`, arg.Commit.Path, len(content))
	})
	mux.HandleFunc("/2/files/download", func(w http.ResponseWriter, r *http.Request) {
		var arg downloadAPIArg
		if err := json.Unmarshal([]byte(r.Header.Get("Dropbox-API-Arg")), &arg); err != nil {
			http.Error(w, "bad arg", http.StatusBadRequest)
			return
		}
		content, ok := fake.files[arg.Path]
		if !ok {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error_summary":"path/not_found/...","error":{".tag":"path"}}`)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Write(content)
	})
	mux.HandleFunc("/2/files/list_folder", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		writeListPage(w, fake.listImmediateChildren(req.Path), "", false)
	})
	mux.HandleFunc("/2/files/list_folder/continue", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown cursor", http.StatusBadRequest)
	})
	mux.HandleFunc("/2/files/delete_v2", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := fake.files[req.Path]; !ok {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error_summary":"path_lookup/not_found/...","error":{".tag":"path_lookup"}}`)
			return
		}
		delete(fake.files, req.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"metadata":{".tag":"file","path_display":%q}}`, req.Path)
	})
	mux.HandleFunc("/2/files/get_metadata", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := fake.files[req.Path]; !ok {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error_summary":"path/not_found/...","error":{".tag":"path"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{".tag":"file","path_display":%q}`, req.Path)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return fake, server
}

// checkCursor mimics Dropbox's own validation: the cursor must name a live
// session and its offset must equal exactly the number of bytes that session
// has already accepted (the position this request's body is written at). It
// writes Dropbox's error shape and returns false on a mismatch.
func (f *fakeDropboxServer) checkCursor(w http.ResponseWriter, cursor uploadSessionCursor) bool {
	got, ok := f.sessions[cursor.SessionID]
	if !ok {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error_summary":"not_found/...","error":{".tag":"not_found"}}`)
		return false
	}
	if cursor.Offset != int64(len(got)) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `{"error_summary":"incorrect_offset/... (got %d, want %d)","error":{".tag":"incorrect_offset"}}`, cursor.Offset, len(got))
		return false
	}
	return true
}

// listImmediateChildren returns the direct children of path (root is "")
// present in f.files, deduplicated by name - enough to fake a single,
// non-paginated /files/list_folder response.
func (f *fakeDropboxServer) listImmediateChildren(path string) []listFolderEntry {
	var entries []listFolderEntry
	seen := map[string]bool{}
	for p, content := range f.files {
		rest := strings.TrimPrefix(p, path)
		if rest == p && path != "" {
			continue // p doesn't have path as a prefix at all
		}
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" || strings.Contains(rest, "/") {
			continue // not a direct child
		}
		if seen[rest] {
			continue
		}
		seen[rest] = true
		entries = append(entries, listFolderEntry{
			Tag:         "file",
			Name:        rest,
			PathDisplay: path + "/" + rest,
			Size:        int64(len(content)),
		})
	}
	return entries
}

func writeListPage(w http.ResponseWriter, entries []listFolderEntry, cursor string, hasMore bool) {
	resp := listFolderResponse{Entries: entries, Cursor: cursor, HasMore: hasMore}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// withPathsOverridden points apiBaseURL/contentBaseURL at serverURL for the
// duration of the test, restoring the originals afterwards.
func withPathsOverridden(t *testing.T, serverURL string) {
	t.Helper()
	prevAPI, prevContent := apiBaseURL, contentBaseURL
	apiBaseURL = serverURL + "/2"
	contentBaseURL = serverURL + "/2"
	t.Cleanup(func() {
		apiBaseURL = prevAPI
		contentBaseURL = prevContent
	})
}

// newTestProvider builds a Provider directly (bypassing New/OAuth) with a
// plain *http.Client wired to server - the simplest way to inject an HTTP
// double for these tests without adding OAuth-specific test scaffolding.
func newTestProvider(server *httptest.Server) *Provider {
	return &Provider{
		cfg:        rawConfig{ConnectionID: "conn1"},
		httpClient: server.Client(),
	}
}

func uploadTask(remotePath string, content []byte) provider.UploadTask {
	return provider.UploadTask{RemotePath: remotePath, Size: int64(len(content)), Reader: bytes.NewReader(content)}
}

func downloadTask(remotePath string, w io.Writer) provider.DownloadTask {
	return provider.DownloadTask{RemotePath: remotePath, Writer: w}
}

func TestTestConnection(t *testing.T) {
	_, server := newFakeDropboxServer(t)
	withPathsOverridden(t, server.URL)
	p := newTestProvider(server)

	if err := p.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection() error = %v", err)
	}
}

func TestUploadDownloadRoundTripAndVerifyChecksum(t *testing.T) {
	_, server := newFakeDropboxServer(t)
	withPathsOverridden(t, server.URL)
	p := newTestProvider(server)

	content := []byte("hello dropbox")
	result, err := p.Upload(context.Background(), uploadTask("dir/file.txt", content))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo {
		t.Fatalf("ChecksumAlgo = %q, want %q", result.ChecksumAlgo, checksumAlgo)
	}
	wantSum := sha256.Sum256(content)
	if result.Checksum != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("Checksum = %q, want %q", result.Checksum, hex.EncodeToString(wantSum[:]))
	}

	var buf bytes.Buffer
	if err := p.Download(context.Background(), downloadTask("dir/file.txt", &buf)); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if buf.String() != string(content) {
		t.Fatalf("Download() content = %q, want %q", buf.String(), string(content))
	}

	ok, err := p.VerifyChecksum(context.Background(), "dir/file.txt", checksumAlgo, result.Checksum)
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if !ok {
		t.Fatal("VerifyChecksum() = false, want true for a matching checksum")
	}

	ok, err = p.VerifyChecksum(context.Background(), "dir/file.txt", checksumAlgo, "deadbeef")
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if ok {
		t.Fatal("VerifyChecksum() = true, want false for a mismatched checksum")
	}
}

func TestDeleteAndIdempotentDelete(t *testing.T) {
	_, server := newFakeDropboxServer(t)
	withPathsOverridden(t, server.URL)
	p := newTestProvider(server)

	if _, err := p.Upload(context.Background(), uploadTask("a.txt", []byte("x"))); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	if err := p.Delete(context.Background(), "a.txt"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	// Deleting again (already missing) must not be an error.
	if err := p.Delete(context.Background(), "a.txt"); err != nil {
		t.Fatalf("Delete() of already-missing object: error = %v, want nil", err)
	}
}

func TestExists(t *testing.T) {
	_, server := newFakeDropboxServer(t)
	withPathsOverridden(t, server.URL)
	p := newTestProvider(server)

	if _, err := p.Upload(context.Background(), uploadTask("a.txt", []byte("x"))); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	ok, err := p.Exists(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if !ok {
		t.Fatal("Exists() = false, want true")
	}

	ok, err = p.Exists(context.Background(), "missing.txt")
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if ok {
		t.Fatal("Exists() = true, want false")
	}
}

// TestListPagination uses a dedicated fake server (rather than
// fakeDropboxServer) since it needs a fixed two-page cursor sequence:
// /files/list_folder always reports has_more=true with cursor "page2",
// and /files/list_folder/continue serves the second page only for that
// cursor.
func TestListPagination(t *testing.T) {
	page1 := []listFolderEntry{{Tag: "file", Name: "a.txt", PathDisplay: "/a.txt", Size: 1}}
	page2 := []listFolderEntry{{Tag: "file", Name: "b.txt", PathDisplay: "/b.txt", Size: 2}}

	mux := http.NewServeMux()
	mux.HandleFunc("/2/files/list_folder", func(w http.ResponseWriter, r *http.Request) {
		writeListPage(w, page1, "page2", true)
	})
	mux.HandleFunc("/2/files/list_folder/continue", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Cursor string `json:"cursor"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Cursor != "page2" {
			http.Error(w, "unknown cursor", http.StatusBadRequest)
			return
		}
		writeListPage(w, page2, "", false)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	withPathsOverridden(t, server.URL)
	p := newTestProvider(server)

	entries, err := p.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() returned %d entries, want 2: %+v", len(entries), entries)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["a.txt"] || !names["b.txt"] {
		t.Fatalf("List() entries = %+v, want a.txt and b.txt", entries)
	}
}

// TestNewWiresDebugLog guards the CLOUDUP_DEBUG wiring.
//
// Regression test for a real gap: debuglog.Transport was originally wired
// into webdav only, so `CLOUDUP_DEBUG=1` produced nothing for this
// provider. The rest of this file constructs Provider directly with an
// httptest client, deliberately bypassing New - which is exactly why the
// wiring inside New needs its own test, or nothing would notice it being
// removed.
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
}

// withChunkGranularity shrinks Dropbox's real 4 MiB chunk granularity for the
// duration of the test, restoring it afterwards. UploadMultipart clamps every
// requested part size to a multiple of this granularity (see clampPartSize),
// so without shrinking it no test payload short of hundreds of megabytes
// could ever span more than one chunk. TestClampPartSize covers the real
// production numbers separately.
func withChunkGranularity(t *testing.T, granularity int64) {
	t.Helper()
	prev := chunkGranularity
	chunkGranularity = granularity
	t.Cleanup(func() { chunkGranularity = prev })
}

// TestClampPartSize pins the real Dropbox constraints: chunks must be whole
// multiples of 4 MiB, and no request body may exceed 150 MB (so the largest
// usable chunk is 35 * 4 MiB = 140 MiB).
func TestClampPartSize(t *testing.T) {
	const miB4 = 4 << 20
	cases := []struct {
		in, want int64
	}{
		{1, miB4},              // below one granule: raised to the minimum
		{miB4, miB4},           // exactly one granule: unchanged
		{miB4 + 1, miB4},       // ragged: rounded down to a whole granule
		{3 * miB4, 3 * miB4},   // already a whole multiple: unchanged
		{150 << 20, 35 * miB4}, // 150 MiB > the 150 MB request cap: clamped
		{1 << 40, 35 * miB4},   // absurdly large: clamped
	}
	for _, c := range cases {
		if got := clampPartSize(c.in); got != c.want {
			t.Errorf("clampPartSize(%d) = %d, want %d", c.in, got, c.want)
		}
		got := clampPartSize(c.in)
		if got%miB4 != 0 {
			t.Errorf("clampPartSize(%d) = %d, which is not a multiple of 4 MiB - Dropbox would reject it", c.in, got)
		}
		if got > 150*1000*1000 {
			t.Errorf("clampPartSize(%d) = %d, which exceeds Dropbox's 150 MB per-request cap", c.in, got)
		}
	}
}

func TestUploadMultipartRejectsNonPositivePartSize(t *testing.T) {
	_, server := newFakeDropboxServer(t)
	withPathsOverridden(t, server.URL)
	p := newTestProvider(server)

	for _, partSize := range []int64{0, -1} {
		if _, err := p.UploadMultipart(context.Background(), uploadTask("a.bin", []byte("x")), partSize); err == nil {
			t.Fatalf("UploadMultipart() with partSize %d: expected error, got nil", partSize)
		}
	}
}

// TestUploadMultipartRoundTrip covers the main contract: a payload spanning
// several chunks arrives intact (proving the cursor offsets and chunk
// slicing are right - the fake rejects a wrong offset), progress is reported
// continuously across the whole file rather than restarting per chunk, and
// the resulting checksum verifies end-to-end through VerifyChecksum, which
// is the exact call internal/history makes.
func TestUploadMultipartRoundTrip(t *testing.T) {
	fake, server := newFakeDropboxServer(t)
	withPathsOverridden(t, server.URL)
	withChunkGranularity(t, 10)
	p := newTestProvider(server)
	ctx := context.Background()

	// 95 bytes in 10-byte chunks: 9 full chunks plus a 5-byte remainder, so
	// the final finish body is partial rather than empty.
	content := []byte(strings.Repeat("0123456789", 9) + "abcde")

	var progress []int64
	task := uploadTask("dir/big.bin", content)
	task.Progress = func(sent, total int64) {
		if total != int64(len(content)) {
			t.Errorf("progress total = %d, want %d", total, len(content))
		}
		progress = append(progress, sent)
	}

	result, err := p.UploadMultipart(ctx, task, 10)
	if err != nil {
		t.Fatalf("UploadMultipart() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo {
		t.Fatalf("ChecksumAlgo = %q, want %q", result.ChecksumAlgo, checksumAlgo)
	}
	wantSum := sha256.Sum256(content)
	if result.Checksum != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("Checksum = %q, want %q", result.Checksum, hex.EncodeToString(wantSum[:]))
	}
	if result.RemoteURL != "/dir/big.bin" {
		t.Fatalf("RemoteURL = %q, want %q", result.RemoteURL, "/dir/big.bin")
	}

	// Progress must be strictly increasing and end at the full size - a
	// per-chunk reader wrapping would make it restart from ~0 repeatedly.
	if len(progress) < 2 {
		t.Fatalf("progress reported %d times, want one report per chunk", len(progress))
	}
	var prev int64
	for i, sent := range progress {
		if sent <= prev {
			t.Fatalf("progress[%d] = %d, not greater than previous %d - progress restarted per chunk", i, sent, prev)
		}
		prev = sent
	}
	if prev != int64(len(content)) {
		t.Fatalf("final progress = %d, want %d", prev, len(content))
	}

	// The fake stores the reassembled file; check both the split it saw and
	// the bytes it ended up with.
	wantChunks := []int{10, 10, 10, 10, 10, 10, 10, 10, 10, 5}
	if got := fake.chunkSizes["session-1"]; !slices.Equal(got, wantChunks) {
		t.Fatalf("session received chunk sizes %v, want %v (9 full chunks plus a 5-byte finish body)", got, wantChunks)
	}

	var buf bytes.Buffer
	if err := p.Download(ctx, downloadTask("dir/big.bin", &buf)); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("Download() returned %d bytes, want the %d uploaded bytes (content mismatch)", buf.Len(), len(content))
	}

	ok, err := p.VerifyChecksum(ctx, "dir/big.bin", result.ChecksumAlgo, result.Checksum)
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if !ok {
		t.Fatal("VerifyChecksum() = false, want true for an untouched multipart upload")
	}
}

// TestUploadMultipartChunkBoundaries covers the two ends of the boundary
// case: a payload that is an exact multiple of the chunk size (the finish
// body is empty) and one that is not (the finish body is the remainder), plus
// a payload smaller than a single chunk and an empty one - all of which still
// have to produce the correct file.
func TestUploadMultipartChunkBoundaries(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		{"exact multiple of chunk size", 40},
		{"not a multiple of chunk size", 43},
		{"smaller than one chunk", 4},
		{"empty file", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, server := newFakeDropboxServer(t)
			withPathsOverridden(t, server.URL)
			withChunkGranularity(t, 10)
			p := newTestProvider(server)
			ctx := context.Background()

			content := []byte(strings.Repeat("x", c.size))
			result, err := p.UploadMultipart(ctx, uploadTask("f.bin", content), 10)
			if err != nil {
				t.Fatalf("UploadMultipart() error = %v", err)
			}
			wantSum := sha256.Sum256(content)
			if result.Checksum != hex.EncodeToString(wantSum[:]) {
				t.Fatalf("Checksum = %q, want %q", result.Checksum, hex.EncodeToString(wantSum[:]))
			}

			var buf bytes.Buffer
			if err := p.Download(ctx, downloadTask("f.bin", &buf)); err != nil {
				t.Fatalf("Download() error = %v", err)
			}
			if !bytes.Equal(buf.Bytes(), content) {
				t.Fatalf("Download() = %d bytes, want %d", buf.Len(), len(content))
			}
		})
	}
}

// TestUploadMultipartMatchesSinglePartChecksum is the test that catches a
// chunking bug which would otherwise corrupt history verification silently:
// if UploadMultipart hashed anything other than the exact whole stream (e.g.
// re-wrapped the reader per chunk, or dropped/duplicated a chunk's bytes in
// the hash), the two checksums would differ even though both uploads
// succeeded.
func TestUploadMultipartMatchesSinglePartChecksum(t *testing.T) {
	_, server := newFakeDropboxServer(t)
	withPathsOverridden(t, server.URL)
	withChunkGranularity(t, 10)
	p := newTestProvider(server)
	ctx := context.Background()

	content := []byte(strings.Repeat("abcde", 13)) // 65 bytes: spans chunks unevenly

	single, err := p.Upload(ctx, uploadTask("single.bin", content))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	multi, err := p.UploadMultipart(ctx, uploadTask("multi.bin", content), 10)
	if err != nil {
		t.Fatalf("UploadMultipart() error = %v", err)
	}

	if single.Checksum != multi.Checksum {
		t.Fatalf("checksum mismatch between single-part (%q) and multipart (%q) upload of identical content", single.Checksum, multi.Checksum)
	}
	if single.ChecksumAlgo != multi.ChecksumAlgo {
		t.Fatalf("ChecksumAlgo differs: single-part %q, multipart %q", single.ChecksumAlgo, multi.ChecksumAlgo)
	}
}

// TestProviderImplementsMultipartUploader documents that the core discovers
// this capability purely by type assertion - nothing is registered for it.
func TestProviderImplementsMultipartUploader(t *testing.T) {
	var p provider.Provider = &Provider{}
	if _, ok := p.(provider.MultipartUploader); !ok {
		t.Fatal("*Provider does not implement provider.MultipartUploader")
	}
}
