package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"cloudup/internal/provider"
	"cloudup/internal/provider/providertest"
)

const (
	testBucketName = "test-bucket"
	testBucketID   = "bucket-0001"
	testAccountID  = "account-0001"
	testKeyID      = "fake-key-id"
	testAppKey     = "fake-app-key"
)

// fakeFile is one object stored in the fake bucket.
type fakeFile struct {
	fileID          string
	fileName        string
	content         []byte
	contentSha1     string
	uploadTimestamp int64
}

// fakeLargeFile is one in-progress or finished large-file session started
// by b2_start_large_file.
type fakeLargeFile struct {
	fileID   string
	fileName string
	parts    map[int][]byte // partNumber -> body, as accepted by b2_upload_part
	finished bool
	canceled bool
}

// fakeB2Server implements just enough of the b2api v2 surface to exercise
// the Provider end to end: authorize, upload-url issuance + the actual
// upload endpoint (verifying the SHA-1 trailer trick), download,
// list/pagination, delete, and the large-file (multipart) API.
type fakeB2Server struct {
	mu         sync.Mutex
	files      map[string]*fakeFile      // keyed by fileName
	largeFiles map[string]*fakeLargeFile // keyed by fileId
	nextID     int
	pageSize   int // if >0, caps files returned per b2_list_file_names call regardless of maxFileCount

	// sessionToken is the only token b2api calls currently accept; each
	// b2_authorize_account issues a fresh one and authCount counts them,
	// which is how the re-authorization tests observe that the Provider
	// really did re-authorize rather than silently succeeding.
	sessionToken string
	authCount    int

	// partUploadToken is the only token handleUploadPart accepts.
	partUploadToken string

	// failPartUploadsRemaining, when > 0, makes the next that many calls to
	// handleUploadPart come back 401 without touching the part's data -
	// simulating the part-specific upload token going stale mid-session,
	// which a real client cannot reliably reproduce by juggling token
	// strings since it never sees the token rotate itself. See
	// failNextPartUpload.
	failPartUploadsRemaining int

	// partUploadURLRequests counts calls to handleGetUploadPartURL - used
	// to confirm the parallel upload path asks for a fresh URL/token per
	// concurrent worker rather than sharing one, which real B2 would reject
	// with auth_token_limit (see UploadMultipartParallel's doc comment).
	partUploadURLRequests int

	srv *httptest.Server
}

func newFakeB2Server(t *testing.T) *fakeB2Server {
	t.Helper()
	f := &fakeB2Server{
		files:           map[string]*fakeFile{},
		largeFiles:      map[string]*fakeLargeFile{},
		partUploadToken: "part-upload-token",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/b2api/v2/b2_authorize_account", f.handleAuthorize)
	mux.HandleFunc("/b2api/v2/b2_list_buckets", f.handleListBuckets)
	mux.HandleFunc("/b2api/v2/b2_get_upload_url", f.handleGetUploadURL)
	mux.HandleFunc("/b2_upload/test-bucket", f.handleUpload)
	mux.HandleFunc("/b2api/v2/b2_list_file_names", f.handleListFileNames)
	mux.HandleFunc("/b2api/v2/b2_delete_file_version", f.handleDeleteFileVersion)
	mux.HandleFunc("/file/", f.handleDownload)
	mux.HandleFunc("/b2api/v2/b2_start_large_file", f.handleStartLargeFile)
	mux.HandleFunc("/b2api/v2/b2_get_upload_part_url", f.handleGetUploadPartURL)
	mux.HandleFunc("/b2_upload_part/test-bucket", f.handleUploadPart)
	mux.HandleFunc("/b2api/v2/b2_finish_large_file", f.handleFinishLargeFile)
	mux.HandleFunc("/b2api/v2/b2_cancel_large_file", f.handleCancelLargeFile)

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeB2Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != testKeyID || pass != testAppKey {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(b2ErrorBody{Status: 401, Code: "unauthorized", Message: "bad keyId/applicationKey"})
		return
	}
	f.mu.Lock()
	f.authCount++
	f.sessionToken = fmt.Sprintf("session-token-%d", f.authCount)
	token := f.sessionToken
	f.mu.Unlock()

	writeJSON(w, map[string]any{
		"apiUrl":             f.srv.URL,
		"authorizationToken": token,
		"downloadUrl":        f.srv.URL,
		"accountId":          testAccountID,
		"allowed": map[string]any{
			"bucketId":   testBucketID,
			"bucketName": testBucketName,
		},
	})
}

func (f *fakeB2Server) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	if !f.checkSessionAuth(w, r) {
		return
	}
	var req struct {
		BucketName string `json:"bucketName"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.BucketName != testBucketName {
		writeJSON(w, map[string]any{"buckets": []any{}})
		return
	}
	writeJSON(w, map[string]any{
		"buckets": []map[string]string{
			{"bucketId": testBucketID, "bucketName": testBucketName},
		},
	})
}

func (f *fakeB2Server) handleGetUploadURL(w http.ResponseWriter, r *http.Request) {
	if !f.checkSessionAuth(w, r) {
		return
	}
	writeJSON(w, map[string]any{
		"uploadUrl":          f.srv.URL + "/b2_upload/test-bucket",
		"authorizationToken": "upload-token",
	})
}

func (f *fakeB2Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r, "upload-token") {
		return
	}
	fileName := r.Header.Get("X-Bz-File-Name")
	if fileName == "" {
		http.Error(w, "missing X-Bz-File-Name", http.StatusBadRequest)
		return
	}
	if r.Header.Get("X-Bz-Content-Sha1") != "hex_digits_at_end" {
		http.Error(w, "expected trailing sha1 marker", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.ContentLength >= 0 && int64(len(body)) != r.ContentLength {
		http.Error(w, fmt.Sprintf("content-length mismatch: header=%d body=%d", r.ContentLength, len(body)), http.StatusBadRequest)
		return
	}
	if len(body) < 40 {
		http.Error(w, "body shorter than trailer", http.StatusBadRequest)
		return
	}
	content := body[:len(body)-40]
	trailer := string(body[len(body)-40:])

	sum := sha1.Sum(content)
	wantHex := hex.EncodeToString(sum[:])
	if trailer != wantHex {
		http.Error(w, fmt.Sprintf("sha1 trailer mismatch: got %s want %s", trailer, wantHex), http.StatusBadRequest)
		return
	}

	fileName = unescapeSegments(fileName)

	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("file-%04d", f.nextID)
	ff := &fakeFile{
		fileID:          id,
		fileName:        fileName,
		content:         content,
		contentSha1:     wantHex,
		uploadTimestamp: time.Now().UnixMilli(),
	}
	f.files[fileName] = ff
	f.mu.Unlock()

	writeJSON(w, map[string]any{
		"fileId":          ff.fileID,
		"fileName":        ff.fileName,
		"contentSha1":     ff.contentSha1,
		"contentLength":   len(content),
		"uploadTimestamp": ff.uploadTimestamp,
	})
}

func (f *fakeB2Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !f.checkSessionAuth(w, r) {
		return
	}
	prefix := "/file/" + testBucketName + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	name := unescapeSegments(strings.TrimPrefix(r.URL.Path, prefix))

	f.mu.Lock()
	ff, ok := f.files[name]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(b2ErrorBody{Status: 404, Code: "not_found", Message: "file not found"})
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(ff.content)))
	w.WriteHeader(http.StatusOK)
	w.Write(ff.content)
}

func (f *fakeB2Server) handleListFileNames(w http.ResponseWriter, r *http.Request) {
	if !f.checkSessionAuth(w, r) {
		return
	}
	var req struct {
		Prefix        string `json:"prefix"`
		MaxFileCount  int    `json:"maxFileCount"`
		Delimiter     string `json:"delimiter"`
		StartFileName string `json:"startFileName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	names := make([]string, 0, len(f.files))
	for n := range f.files {
		names = append(names, n)
	}
	sort.Strings(names)
	f.mu.Unlock()

	type item struct {
		fileName string
		isFolder bool
	}
	var candidates []item
	seenFolders := map[string]bool{}
	for _, n := range names {
		if !strings.HasPrefix(n, req.Prefix) {
			continue
		}
		rest := strings.TrimPrefix(n, req.Prefix)
		if req.Delimiter != "" && strings.Contains(rest, req.Delimiter) {
			folder := req.Prefix + rest[:strings.Index(rest, req.Delimiter)+1]
			if !seenFolders[folder] {
				seenFolders[folder] = true
				candidates = append(candidates, item{fileName: folder, isFolder: true})
			}
			continue
		}
		candidates = append(candidates, item{fileName: n})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].fileName < candidates[j].fileName })

	startIdx := 0
	if req.StartFileName != "" {
		for i, c := range candidates {
			if c.fileName >= req.StartFileName {
				startIdx = i
				break
			}
			startIdx = i + 1
		}
	}

	limit := req.MaxFileCount
	if f.pageSize > 0 && (limit == 0 || f.pageSize < limit) {
		limit = f.pageSize
	}
	if limit <= 0 {
		limit = 1000
	}

	end := startIdx + limit
	if end > len(candidates) {
		end = len(candidates)
	}
	page := candidates[startIdx:end]

	files := make([]map[string]any, 0, len(page))
	for _, c := range page {
		if c.isFolder {
			files = append(files, map[string]any{
				"fileId":          "",
				"fileName":        c.fileName,
				"contentLength":   0,
				"contentSha1":     "none",
				"uploadTimestamp": 0,
				"action":          "folder",
			})
			continue
		}
		f.mu.Lock()
		ff := f.files[c.fileName]
		f.mu.Unlock()
		files = append(files, map[string]any{
			"fileId":          ff.fileID,
			"fileName":        ff.fileName,
			"contentLength":   len(ff.content),
			"contentSha1":     ff.contentSha1,
			"uploadTimestamp": ff.uploadTimestamp,
			"action":          "upload",
		})
	}

	resp := map[string]any{"files": files}
	if end < len(candidates) {
		resp["nextFileName"] = candidates[end].fileName
		resp["nextFileId"] = "cursor"
	} else {
		resp["nextFileName"] = nil
		resp["nextFileId"] = nil
	}
	writeJSON(w, resp)
}

func (f *fakeB2Server) handleDeleteFileVersion(w http.ResponseWriter, r *http.Request) {
	if !f.checkSessionAuth(w, r) {
		return
	}
	var req struct {
		FileName string `json:"fileName"`
		FileID   string `json:"fileId"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	f.mu.Lock()
	ff, ok := f.files[req.FileName]
	if ok && ff.fileID == req.FileID {
		delete(f.files, req.FileName)
	}
	f.mu.Unlock()

	if !ok {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(b2ErrorBody{Status: 404, Code: "not_found", Message: "file not found"})
		return
	}
	writeJSON(w, map[string]any{"fileId": req.FileID, "fileName": req.FileName})
}

func (f *fakeB2Server) handleStartLargeFile(w http.ResponseWriter, r *http.Request) {
	if !f.checkSessionAuth(w, r) {
		return
	}
	var req struct {
		BucketID    string `json:"bucketId"`
		FileName    string `json:"fileName"`
		ContentType string `json:"contentType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("large-%04d", f.nextID)
	f.largeFiles[id] = &fakeLargeFile{fileID: id, fileName: req.FileName, parts: map[int][]byte{}}
	f.mu.Unlock()

	writeJSON(w, map[string]any{"fileId": id, "fileName": req.FileName})
}

// handleGetUploadPartURL hands back an uploadUrl that embeds fileId as a
// query parameter, which is how this fake routes handleUploadPart back to
// the right in-progress session - real B2's uploadUrl is similarly opaque
// to the client, which never parses it.
func (f *fakeB2Server) handleGetUploadPartURL(w http.ResponseWriter, r *http.Request) {
	if !f.checkSessionAuth(w, r) {
		return
	}
	var req struct {
		FileID string `json:"fileId"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	f.mu.Lock()
	f.partUploadURLRequests++
	lf, ok := f.largeFiles[req.FileID]
	token := f.partUploadToken
	f.mu.Unlock()
	if !ok || lf.finished || lf.canceled {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(b2ErrorBody{Status: 400, Code: "bad_request", Message: "no such large file"})
		return
	}

	writeJSON(w, map[string]any{
		"uploadUrl":          f.srv.URL + "/b2_upload_part/test-bucket?fileId=" + req.FileID,
		"authorizationToken": token,
	})
}

func (f *fakeB2Server) handleUploadPart(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.failPartUploadsRemaining > 0 {
		f.failPartUploadsRemaining--
		f.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(b2ErrorBody{Status: 401, Code: "expired_auth_token", Message: "simulated part-upload token expiry"})
		return
	}
	wantToken := f.partUploadToken
	f.mu.Unlock()
	if !f.checkAuth(w, r, wantToken) {
		return
	}

	fileID := r.URL.Query().Get("fileId")
	partNumber, err := strconv.Atoi(r.Header.Get("X-Bz-Part-Number"))
	if err != nil || partNumber < 1 {
		http.Error(w, "invalid X-Bz-Part-Number", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.ContentLength >= 0 && int64(len(body)) != r.ContentLength {
		http.Error(w, fmt.Sprintf("content-length mismatch: header=%d body=%d", r.ContentLength, len(body)), http.StatusBadRequest)
		return
	}

	sum := sha1.Sum(body)
	wantHex := hex.EncodeToString(sum[:])
	if r.Header.Get("X-Bz-Content-Sha1") != wantHex {
		http.Error(w, fmt.Sprintf("sha1 mismatch: got %s want %s", r.Header.Get("X-Bz-Content-Sha1"), wantHex), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	lf, ok := f.largeFiles[fileID]
	if ok {
		lf.parts[partNumber] = body
	}
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(b2ErrorBody{Status: 400, Code: "bad_request", Message: "no such large file"})
		return
	}

	writeJSON(w, map[string]any{
		"fileId":        fileID,
		"partNumber":    partNumber,
		"contentLength": len(body),
		"contentSha1":   wantHex,
	})
}

func (f *fakeB2Server) handleFinishLargeFile(w http.ResponseWriter, r *http.Request) {
	if !f.checkSessionAuth(w, r) {
		return
	}
	var req struct {
		FileID        string   `json:"fileId"`
		PartSha1Array []string `json:"partSha1Array"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	lf, ok := f.largeFiles[req.FileID]
	f.mu.Unlock()
	if !ok || lf.finished || lf.canceled {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(b2ErrorBody{Status: 400, Code: "bad_request", Message: "no such large file"})
		return
	}

	var content []byte
	for i := 1; i <= len(req.PartSha1Array); i++ {
		part, ok := lf.parts[i]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(b2ErrorBody{Status: 400, Code: "bad_request", Message: fmt.Sprintf("missing part %d", i)})
			return
		}
		sum := sha1.Sum(part)
		if hex.EncodeToString(sum[:]) != req.PartSha1Array[i-1] {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(b2ErrorBody{Status: 400, Code: "bad_request", Message: "part sha1 mismatch"})
			return
		}
		content = append(content, part...)
	}

	f.mu.Lock()
	lf.finished = true
	f.nextID++
	id := fmt.Sprintf("file-%04d", f.nextID)
	ff := &fakeFile{
		fileID:          id,
		fileName:        lf.fileName,
		content:         content,
		contentSha1:     b2NoContentSha1, // real B2 behavior for large files without large_file_sha1 fileInfo
		uploadTimestamp: time.Now().UnixMilli(),
	}
	f.files[lf.fileName] = ff
	f.mu.Unlock()

	writeJSON(w, map[string]any{
		"fileId":      id,
		"fileName":    lf.fileName,
		"contentSha1": b2NoContentSha1,
	})
}

func (f *fakeB2Server) handleCancelLargeFile(w http.ResponseWriter, r *http.Request) {
	if !f.checkSessionAuth(w, r) {
		return
	}
	var req struct {
		FileID string `json:"fileId"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	f.mu.Lock()
	if lf, ok := f.largeFiles[req.FileID]; ok {
		lf.canceled = true
	}
	f.mu.Unlock()

	writeJSON(w, map[string]any{"fileId": req.FileID})
}

// failNextPartUpload makes the next n calls to handleUploadPart come back
// 401 without recording the part - see failPartUploadsRemaining.
func (f *fakeB2Server) failNextPartUpload(n int) {
	f.mu.Lock()
	f.failPartUploadsRemaining = n
	f.mu.Unlock()
}

// checkSessionAuth accepts only the token issued by the most recent
// b2_authorize_account, so expireSession makes every subsequent call from a
// Provider holding an older token come back 401 - exactly what B2 does when
// a session token times out.
func (f *fakeB2Server) checkSessionAuth(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	want := f.sessionToken
	f.mu.Unlock()
	return f.checkAuth(w, r, want)
}

// expireSession invalidates whatever session token is currently valid.
func (f *fakeB2Server) expireSession() {
	f.mu.Lock()
	f.sessionToken = "expired-" + f.sessionToken
	f.mu.Unlock()
}

func (f *fakeB2Server) checkAuth(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Header.Get("Authorization") != want {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(b2ErrorBody{Status: 401, Code: "expired_auth_token", Message: "unauthorized"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// unescapeSegments is the test-side inverse of escapeB2Path, used by the
// fake server to recover the original file name from an incoming request
// path/header.
func unescapeSegments(escaped string) string {
	segments := strings.Split(escaped, "/")
	for i, s := range segments {
		if unescaped, err := url.PathUnescape(s); err == nil {
			segments[i] = unescaped
		}
	}
	return strings.Join(segments, "/")
}

func newTestProvider(t *testing.T, fake *fakeB2Server) *Provider {
	t.Helper()

	oldURL := authorizeURL
	authorizeURL = fake.srv.URL + "/b2api/v2/b2_authorize_account"
	t.Cleanup(func() { authorizeURL = oldURL })

	secrets := providertest.NewMemSecretStore()
	secrets.Set("conn1", secretKeyID, testKeyID)
	secrets.Set("conn1", secretApplicationKey, testAppKey)

	rawCfg, err := json.Marshal(Config{ConnectionID: "conn1", BucketName: testBucketName})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	p, err := New(rawCfg, secrets)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p.(*Provider)
}

// TestEndpointName locks in the path.Base replacement for the old
// hand-rolled pathBase: endpointName strips any query/fragment and a
// trailing slash before taking the final path segment.
func TestEndpointName(t *testing.T) {
	cases := map[string]string{
		"https://api.backblazeb2.com/b2api/v3/b2_list_buckets":      "b2_list_buckets",
		"https://api.backblazeb2.com/b2api/v3/b2_list_buckets/":     "b2_list_buckets",
		"https://api.backblazeb2.com/b2api/v3/b2_list_buckets?a=1":  "b2_list_buckets",
		"https://api.backblazeb2.com/b2api/v3/b2_list_buckets#frag": "b2_list_buckets",
	}
	for rawURL, want := range cases {
		if got := endpointName(rawURL); got != want {
			t.Errorf("endpointName(%q) = %q, want %q", rawURL, got, want)
		}
	}
}

func TestConfigFields(t *testing.T) {
	fields := (&Provider{}).ConfigFields()
	want := map[string]provider.FieldType{
		"bucketName":     provider.FieldText,
		"keyId":          provider.FieldPassword,
		"applicationKey": provider.FieldPassword,
	}
	if len(fields) != len(want) {
		t.Fatalf("ConfigFields() returned %d fields, want %d", len(fields), len(want))
	}
	for _, f := range fields {
		wantType, ok := want[f.Key]
		if !ok {
			t.Errorf("unexpected field %q", f.Key)
			continue
		}
		if f.Type != wantType {
			t.Errorf("field %q type = %v, want %v", f.Key, f.Type, wantType)
		}
		if !f.Required {
			t.Errorf("field %q should be required", f.Key)
		}
	}
}

func TestNewValidation(t *testing.T) {
	secrets := providertest.NewMemSecretStore()

	// Missing bucket name.
	rawCfg, _ := json.Marshal(Config{ConnectionID: "conn1"})
	if _, err := New(rawCfg, secrets); err == nil {
		t.Fatal("New() with missing bucketName should fail")
	}

	// Missing keyId.
	rawCfg, _ = json.Marshal(Config{ConnectionID: "conn1", BucketName: "b"})
	if _, err := New(rawCfg, secrets); err == nil {
		t.Fatal("New() with missing keyId should fail")
	}

	// Missing applicationKey.
	secrets.Set("conn1", secretKeyID, "k")
	if _, err := New(rawCfg, secrets); err == nil {
		t.Fatal("New() with missing applicationKey should fail")
	}

	// All present.
	secrets.Set("conn1", secretApplicationKey, "a")
	if _, err := New(rawCfg, secrets); err != nil {
		t.Fatalf("New() with all fields present should succeed, got %v", err)
	}
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	ctx := context.Background()

	content := []byte("hello, backblaze b2")
	result, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "docs/greeting.txt",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo {
		t.Fatalf("ChecksumAlgo = %q, want %q", result.ChecksumAlgo, checksumAlgo)
	}
	sum := sha1.Sum(content)
	wantHex := hex.EncodeToString(sum[:])
	if result.Checksum != wantHex {
		t.Fatalf("Checksum = %q, want %q", result.Checksum, wantHex)
	}
	if result.RemotePath != "docs/greeting.txt" {
		t.Fatalf("RemotePath = %q", result.RemotePath)
	}
	if !strings.Contains(result.RemoteURL, "docs/greeting.txt") {
		t.Fatalf("RemoteURL = %q, want it to contain the path", result.RemoteURL)
	}

	var buf bytes.Buffer
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: "docs/greeting.txt", Writer: &buf}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if buf.String() != string(content) {
		t.Fatalf("Download() content = %q, want %q", buf.String(), content)
	}
}

func TestListPaginationAndFolders(t *testing.T) {
	fake := newFakeB2Server(t)
	fake.pageSize = 2 // force multiple pages
	p := newTestProvider(t, fake)
	ctx := context.Background()

	files := []string{"a.txt", "b.txt", "sub/c.txt", "sub/d.txt"}
	for _, name := range files {
		content := []byte("content-of-" + name)
		if _, err := p.Upload(ctx, provider.UploadTask{
			RemotePath: name,
			Size:       int64(len(content)),
			Reader:     bytes.NewReader(content),
		}); err != nil {
			t.Fatalf("Upload(%q) error = %v", name, err)
		}
	}

	entries, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	var gotFiles, gotDirs []string
	for _, e := range entries {
		if e.IsDir {
			gotDirs = append(gotDirs, e.Path)
		} else {
			gotFiles = append(gotFiles, e.Path)
			if e.ModTime.IsZero() {
				t.Errorf("entry %q has zero ModTime", e.Path)
			}
		}
	}
	sort.Strings(gotFiles)
	sort.Strings(gotDirs)

	wantFiles := []string{"a.txt", "b.txt"}
	wantDirs := []string{"sub"}
	if fmt.Sprint(gotFiles) != fmt.Sprint(wantFiles) {
		t.Errorf("files = %v, want %v", gotFiles, wantFiles)
	}
	if fmt.Sprint(gotDirs) != fmt.Sprint(wantDirs) {
		t.Errorf("dirs = %v, want %v", gotDirs, wantDirs)
	}

	// List within the sub folder.
	subEntries, err := p.List(ctx, "sub")
	if err != nil {
		t.Fatalf("List(sub) error = %v", err)
	}
	var subFiles []string
	for _, e := range subEntries {
		if !e.IsDir {
			subFiles = append(subFiles, e.Name)
		}
	}
	sort.Strings(subFiles)
	want := []string{"c.txt", "d.txt"}
	if fmt.Sprint(subFiles) != fmt.Sprint(want) {
		t.Errorf("sub files = %v, want %v", subFiles, want)
	}
}

func TestDeleteAndExists(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
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

	// Deleting an already-missing file is idempotent, not an error.
	if err := p.Delete(ctx, "to-delete.txt"); err != nil {
		t.Fatalf("Delete() of already-missing file should be nil, got %v", err)
	}
	if err := p.Delete(ctx, "never-existed.txt"); err != nil {
		t.Fatalf("Delete() of never-existing file should be nil, got %v", err)
	}
}

func TestVerifyChecksum(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
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

	if _, err := p.VerifyChecksum(ctx, "verify.txt", "md5", "deadbeef"); err == nil {
		t.Fatal("VerifyChecksum() with wrong algo should fail")
	}
}

// TestReauthorizeAfterExpiredToken covers withReauth on the three control
// paths that go through it: b2_get_upload_url (Upload's first step),
// b2_list_file_names (Exists/List/Delete/VerifyChecksum) and the download
// GET. In each case the cached session token has gone stale, so the first
// attempt must come back 401 and the call must still succeed after a
// transparent re-authorization.
func TestReauthorizeAfterExpiredToken(t *testing.T) {
	upload := func(t *testing.T, p *Provider, name, content string) {
		t.Helper()
		if _, err := p.Upload(context.Background(), provider.UploadTask{
			RemotePath: name,
			Size:       int64(len(content)),
			Reader:     strings.NewReader(content),
		}); err != nil {
			t.Fatalf("Upload(%q) error = %v", name, err)
		}
	}

	t.Run("upload", func(t *testing.T) {
		fake := newFakeB2Server(t)
		p := newTestProvider(t, fake)
		upload(t, p, "first.txt", "first")

		fake.expireSession()
		upload(t, p, "second.txt", "second")

		if fake.authCount != 2 {
			t.Fatalf("authCount = %d, want 2 (one initial, one after expiry)", fake.authCount)
		}
	})

	t.Run("list", func(t *testing.T) {
		fake := newFakeB2Server(t)
		p := newTestProvider(t, fake)
		upload(t, p, "listed.txt", "content")

		fake.expireSession()
		exists, err := p.Exists(context.Background(), "listed.txt")
		if err != nil {
			t.Fatalf("Exists() after token expiry error = %v", err)
		}
		if !exists {
			t.Fatal("Exists() = false after re-authorization, want true")
		}
		if fake.authCount != 2 {
			t.Fatalf("authCount = %d, want 2", fake.authCount)
		}
	})

	t.Run("download", func(t *testing.T) {
		fake := newFakeB2Server(t)
		p := newTestProvider(t, fake)
		upload(t, p, "fetched.txt", "payload")

		fake.expireSession()
		var buf bytes.Buffer
		if err := p.Download(context.Background(), provider.DownloadTask{RemotePath: "fetched.txt", Writer: &buf}); err != nil {
			t.Fatalf("Download() after token expiry error = %v", err)
		}
		if buf.String() != "payload" {
			t.Fatalf("Download() content = %q, want %q", buf.String(), "payload")
		}
		if fake.authCount != 2 {
			t.Fatalf("authCount = %d, want 2", fake.authCount)
		}
	})
}

// TestUploadRejectsUnknownSize pins the guard from exactSizeBody: B2 needs
// an exact Content-Length before the body starts (Size + the 40-byte SHA-1
// trailer), so a Size that cannot be the real length has to fail loudly
// instead of storing a truncated object. A genuinely empty file, which is
// the other thing Size == 0 can legitimately mean, must still work.
func TestUploadRejectsUnknownSize(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	ctx := context.Background()

	_, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "unknown-length.txt",
		Size:       0,
		Reader:     strings.NewReader("this stream is not empty"),
	})
	if err == nil {
		t.Fatal("Upload() with Size 0 and a non-empty reader should fail")
	}
	if !strings.Contains(err.Error(), "size is 0") {
		t.Fatalf("Upload() error = %v, want it to explain the zero size", err)
	}
	if _, ok := fake.files["unknown-length.txt"]; ok {
		t.Fatal("Upload() stored an object despite failing")
	}

	if _, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "negative.txt",
		Size:       -1,
		Reader:     strings.NewReader("x"),
	}); err == nil {
		t.Fatal("Upload() with a negative Size should fail")
	}

	// An empty file is a legitimate Size == 0 and must still upload.
	result, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "empty.txt",
		Size:       0,
		Reader:     strings.NewReader(""),
	})
	if err != nil {
		t.Fatalf("Upload() of an empty file error = %v", err)
	}
	sum := sha1.Sum(nil)
	if want := hex.EncodeToString(sum[:]); result.Checksum != want {
		t.Fatalf("empty file Checksum = %q, want %q", result.Checksum, want)
	}
}

// withSmallB2MinPartSize shrinks b2MinPartSize to n for the duration of the
// calling test, so multipart tests can exercise the chunking loop with
// byte-sized payloads instead of the real 5 MB minimum.
func withSmallB2MinPartSize(t *testing.T, n int64) {
	t.Helper()
	old := b2MinPartSize
	b2MinPartSize = n
	t.Cleanup(func() { b2MinPartSize = old })
}

func TestClampPartSize(t *testing.T) {
	withSmallB2MinPartSize(t, 1000)
	cases := []struct {
		in, want int64
	}{
		{1, 1000},
		{999, 1000},
		{1000, 1000},
		{1001, 1001},
		{1_000_000, 1_000_000},
	}
	for _, c := range cases {
		if got := clampPartSize(c.in); got != c.want {
			t.Errorf("clampPartSize(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestUploadMultipartRejectsNonPositivePartSize(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)

	for _, size := range []int64{0, -1} {
		_, err := p.UploadMultipart(context.Background(), provider.UploadTask{
			RemotePath: "x.bin",
			Size:       10,
			Reader:     bytes.NewReader(make([]byte, 10)),
		}, size)
		if err == nil {
			t.Fatalf("UploadMultipart() with partSize %d should fail", size)
		}
	}
}

func TestUploadMultipartRoundTrip(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	ctx := context.Background()
	withSmallB2MinPartSize(t, 8)

	content := bytes.Repeat([]byte("0123456789"), 5) // 50 bytes -> parts of 8*6 + 2

	result, err := p.UploadMultipart(ctx, provider.UploadTask{
		RemotePath: "large/file.bin",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	}, 8)
	if err != nil {
		t.Fatalf("UploadMultipart() error = %v", err)
	}
	sum := sha1.Sum(content)
	if want := hex.EncodeToString(sum[:]); result.Checksum != want {
		t.Fatalf("Checksum = %q, want %q", result.Checksum, want)
	}
	if result.ChecksumAlgo != checksumAlgo {
		t.Fatalf("ChecksumAlgo = %q, want %q", result.ChecksumAlgo, checksumAlgo)
	}
	if !strings.Contains(result.RemoteURL, "large/file.bin") {
		t.Fatalf("RemoteURL = %q, want it to contain the path", result.RemoteURL)
	}

	var buf bytes.Buffer
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: "large/file.bin", Writer: &buf}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("downloaded content = %q, want %q", buf.Bytes(), content)
	}

	// Confirm this really went through the large-file API with more than
	// one part, not a silent single-request fallback.
	fake.mu.Lock()
	var partCounts []int
	for _, lf := range fake.largeFiles {
		if lf.finished {
			partCounts = append(partCounts, len(lf.parts))
		}
	}
	fake.mu.Unlock()
	if len(partCounts) != 1 || partCounts[0] < 2 {
		t.Fatalf("finished large files with part counts = %v, want exactly one with >= 2 parts", partCounts)
	}
}

func TestUploadMultipartChunkBoundaries(t *testing.T) {
	cases := []struct {
		name      string
		size      int
		wantParts int
	}{
		{"exact multiple", 16, 2},
		{"remainder", 20, 3},
		{"less than one chunk", 5, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeB2Server(t)
			p := newTestProvider(t, fake)
			ctx := context.Background()
			withSmallB2MinPartSize(t, 8)

			content := bytes.Repeat([]byte("x"), c.size)
			result, err := p.UploadMultipart(ctx, provider.UploadTask{
				RemotePath: "chunked.bin",
				Size:       int64(len(content)),
				Reader:     bytes.NewReader(content),
			}, 8)
			if err != nil {
				t.Fatalf("UploadMultipart() error = %v", err)
			}
			sum := sha1.Sum(content)
			if want := hex.EncodeToString(sum[:]); result.Checksum != want {
				t.Fatalf("Checksum = %q, want %q", result.Checksum, want)
			}

			fake.mu.Lock()
			var got int
			for _, lf := range fake.largeFiles {
				if lf.finished {
					got = len(lf.parts)
				}
			}
			fake.mu.Unlock()
			if got != c.wantParts {
				t.Errorf("part count = %d, want %d", got, c.wantParts)
			}
		})
	}
}

// TestUploadMultipartRejectsEmptySource pins the documented direct-call
// guard: internal/queue never reaches UploadMultipart for an empty file (it
// only dispatches here above DefaultMultipartThreshold), but a caller that
// does must get a clear error rather than a large file with zero parts,
// which b2_finish_large_file would reject anyway.
func TestUploadMultipartRejectsEmptySource(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	withSmallB2MinPartSize(t, 8)

	_, err := p.UploadMultipart(context.Background(), provider.UploadTask{
		RemotePath: "empty.bin",
		Size:       0,
		Reader:     bytes.NewReader(nil),
	}, 8)
	if err == nil {
		t.Fatal("UploadMultipart() of an empty source should fail")
	}
}

// TestUploadMultipartMatchesSinglePartChecksum is the test the
// MultipartUploader contract (internal/provider/features.go) explicitly
// requires of every implementor: the same content must produce the same
// checksum whether it goes through Upload or UploadMultipart, since
// internal/history's verification cannot tell the two paths apart.
func TestUploadMultipartMatchesSinglePartChecksum(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	ctx := context.Background()
	withSmallB2MinPartSize(t, 8)

	content := bytes.Repeat([]byte("checksum-parity-"), 4)

	single, err := p.Upload(ctx, provider.UploadTask{RemotePath: "single.bin", Size: int64(len(content)), Reader: bytes.NewReader(content)})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	multi, err := p.UploadMultipart(ctx, provider.UploadTask{RemotePath: "multi.bin", Size: int64(len(content)), Reader: bytes.NewReader(content)}, 8)
	if err != nil {
		t.Fatalf("UploadMultipart() error = %v", err)
	}
	if single.Checksum != multi.Checksum {
		t.Fatalf("checksum mismatch: single-part %q, multipart %q", single.Checksum, multi.Checksum)
	}
}

// TestVerifyChecksumForMultipartUpload covers the fallback VerifyChecksum
// takes for files uploaded via UploadMultipart: B2 has no cheap native
// contentSha1 for these (see b2NoContentSha1), so verification has to
// re-download and rehash instead, unlike the single-part metadata-only path
// TestVerifyChecksum already covers.
func TestVerifyChecksumForMultipartUpload(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	ctx := context.Background()
	withSmallB2MinPartSize(t, 8)

	content := bytes.Repeat([]byte("verify-me-"), 3)
	result, err := p.UploadMultipart(ctx, provider.UploadTask{RemotePath: "verify-multi.bin", Size: int64(len(content)), Reader: bytes.NewReader(content)}, 8)
	if err != nil {
		t.Fatalf("UploadMultipart() error = %v", err)
	}

	_, sha1sum, found, err := p.findFile(ctx, "verify-multi.bin")
	if err != nil || !found {
		t.Fatalf("findFile() = %q, %v, %v", sha1sum, found, err)
	}
	if sha1sum != b2NoContentSha1 {
		t.Fatalf("findFile() contentSha1 = %q, want %q", sha1sum, b2NoContentSha1)
	}

	ok, err := p.VerifyChecksum(ctx, "verify-multi.bin", result.ChecksumAlgo, result.Checksum)
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if !ok {
		t.Fatal("VerifyChecksum() = false, want true for untouched multipart upload")
	}

	ok, err = p.VerifyChecksum(ctx, "verify-multi.bin", result.ChecksumAlgo, "deadbeef")
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if ok {
		t.Fatal("VerifyChecksum() = true, want false for mismatched checksum")
	}
}

// TestUploadMultipartReauthorizesExpiredPartUploadToken covers the retry
// doUploadPart's caller performs on a 401: unlike the single-request
// Upload path, a part's bytes are still fully buffered when its upload
// token turns out to be stale, so a fresh URL/token can simply be fetched
// and the same part resent.
func TestUploadMultipartReauthorizesExpiredPartUploadToken(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	ctx := context.Background()
	withSmallB2MinPartSize(t, 8)

	content := bytes.Repeat([]byte("y"), 24) // 3 parts of 8 bytes at partSize 8
	fake.failNextPartUpload(1)

	result, err := p.UploadMultipart(ctx, provider.UploadTask{RemotePath: "reauth.bin", Size: int64(len(content)), Reader: bytes.NewReader(content)}, 8)
	if err != nil {
		t.Fatalf("UploadMultipart() error = %v", err)
	}
	sum := sha1.Sum(content)
	if want := hex.EncodeToString(sum[:]); result.Checksum != want {
		t.Fatalf("Checksum = %q, want %q", result.Checksum, want)
	}
}

// errReader always fails, letting TestUploadMultipartCancelsLargeFileOnFailure
// force a mid-stream read error after the first part has already been
// accepted by the fake server.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestUploadMultipartCancelsLargeFileOnFailure(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	ctx := context.Background()
	withSmallB2MinPartSize(t, 8)

	firstPart := bytes.Repeat([]byte("a"), 8)
	reader := io.MultiReader(bytes.NewReader(firstPart), errReader{err: errors.New("boom")})

	_, err := p.UploadMultipart(ctx, provider.UploadTask{
		RemotePath: "big.bin",
		Size:       100,
		Reader:     reader,
	}, 8)
	if err == nil {
		t.Fatal("UploadMultipart() should fail when the source errors mid-stream")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	var canceled int
	for _, lf := range fake.largeFiles {
		if lf.canceled {
			canceled++
		}
	}
	if canceled != 1 {
		t.Fatalf("canceled large files = %d, want 1", canceled)
	}
}

func TestProviderImplementsMultipartUploader(t *testing.T) {
	var _ provider.MultipartUploader = (*Provider)(nil)
}

func TestUploadMultipartParallelRejectsMissingReaderAt(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	withSmallB2MinPartSize(t, 8)

	_, err := p.UploadMultipartParallel(context.Background(), provider.UploadTask{
		RemotePath: "x.bin",
		Size:       20,
		Reader:     bytes.NewReader(make([]byte, 20)),
		// ReaderAt intentionally left nil.
	}, 8, 4)
	if err == nil {
		t.Fatal("UploadMultipartParallel() without a ReaderAt should fail")
	}
}

func TestUploadMultipartParallelRoundTrip(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	ctx := context.Background()
	withSmallB2MinPartSize(t, 8)

	content := bytes.Repeat([]byte("0123456789"), 5) // 50 bytes -> several 8-byte parts
	r := bytes.NewReader(content)
	result, err := p.UploadMultipartParallel(ctx, provider.UploadTask{
		RemotePath: "parallel.bin",
		Size:       int64(len(content)),
		Reader:     r,
		ReaderAt:   r,
	}, 8, 4)
	if err != nil {
		t.Fatalf("UploadMultipartParallel() error = %v", err)
	}
	sum := sha1.Sum(content)
	if want := hex.EncodeToString(sum[:]); result.Checksum != want {
		t.Fatalf("Checksum = %q, want %q", result.Checksum, want)
	}
	if result.ChecksumAlgo != checksumAlgo {
		t.Fatalf("ChecksumAlgo = %q, want %q", result.ChecksumAlgo, checksumAlgo)
	}

	var buf bytes.Buffer
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: "parallel.bin", Writer: &buf}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("downloaded content mismatch")
	}

	// Confirm each concurrent worker fetched its own upload-part URL/token
	// instead of sharing one - real B2 rejects concurrent reuse of a single
	// token with auth_token_limit (see UploadMultipartParallel's doc
	// comment).
	fake.mu.Lock()
	requests := fake.partUploadURLRequests
	fake.mu.Unlock()
	var wantParts int
	for size := int64(len(content)); size > 0; size -= 8 {
		wantParts++
	}
	if requests < wantParts {
		t.Fatalf("partUploadURLRequests = %d, want at least %d (one per part)", requests, wantParts)
	}
}

// TestUploadMultipartParallelMatchesSinglePartChecksum is the parallel-path
// counterpart to TestUploadMultipartMatchesSinglePartChecksum: same content
// must produce the same checksum through Upload and through
// UploadMultipartParallel.
func TestUploadMultipartParallelMatchesSinglePartChecksum(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	ctx := context.Background()
	withSmallB2MinPartSize(t, 8)

	content := bytes.Repeat([]byte("checksum-parity-"), 4)

	single, err := p.Upload(ctx, provider.UploadTask{RemotePath: "single.bin", Size: int64(len(content)), Reader: bytes.NewReader(content)})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	r := bytes.NewReader(content)
	parallel, err := p.UploadMultipartParallel(ctx, provider.UploadTask{
		RemotePath: "parallel.bin",
		Size:       int64(len(content)),
		Reader:     r,
		ReaderAt:   r,
	}, 8, 4)
	if err != nil {
		t.Fatalf("UploadMultipartParallel() error = %v", err)
	}
	if single.Checksum != parallel.Checksum {
		t.Fatalf("checksum mismatch: single-part %q, parallel multipart %q", single.Checksum, parallel.Checksum)
	}
}

func TestUploadMultipartParallelCancelsLargeFileOnFailure(t *testing.T) {
	fake := newFakeB2Server(t)
	p := newTestProvider(t, fake)
	ctx := context.Background()
	withSmallB2MinPartSize(t, 8)

	content := bytes.Repeat([]byte("y"), 40) // 5 parts of 8 bytes
	// A count comfortably above any possible number of attempts (5 parts,
	// each retried at most once on a 401 - see UploadMultipartParallel):
	// every single attempt fails, so the whole upload is guaranteed to fail
	// rather than merely likely to.
	fake.failNextPartUpload(1000)

	r := bytes.NewReader(content)
	_, err := p.UploadMultipartParallel(ctx, provider.UploadTask{
		RemotePath: "big.bin",
		Size:       int64(len(content)),
		Reader:     r,
		ReaderAt:   r,
	}, 8, 4)
	if err == nil {
		t.Fatal("UploadMultipartParallel() should fail when every part upload is rejected")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	var canceled int
	for _, lf := range fake.largeFiles {
		if lf.canceled {
			canceled++
		}
	}
	if canceled != 1 {
		t.Fatalf("canceled large files = %d, want 1", canceled)
	}
}

func TestProviderImplementsParallelMultipartUploader(t *testing.T) {
	var _ provider.ParallelMultipartUploader = (*Provider)(nil)
}
