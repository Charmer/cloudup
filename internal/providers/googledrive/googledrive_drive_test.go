package googledrive

// This file is the wire-level fake this package previously lacked: a small
// in-memory HTTP server that speaks just enough of the real Drive v3 REST
// surface for
// the CRUD methods in googledrive.go to be exercised end to end, the same
// way fakeB2Server/the WebDAV httptest server/the S3 fake do for the other
// providers - as opposed to a hand-rolled stand-in for *drive.Service, which
// would only prove this package calls the Go SDK the way the test author
// expected, not that the SDK's actual bytes-on-the-wire behavior is handled
// correctly.
//
// The one deliberate wire-protocol subtlety worth calling out: Provider.
// Upload calls Files.Create(...).Media(reader) with no MediaOption, so the
// Drive SDK (google.golang.org/api/internal/gensupport) picks the upload
// flavor itself based on content size relative to googleapi.
// DefaultUploadChunkSize (16 MiB) - simple "multipart" for anything smaller,
// a genuine multi-request "resumable" session (POST to initiate + get a
// Location header, then repeated chunk POSTs with a Content-Range header)
// for anything at or above it. Both flavors are implemented below because
// both are real production behavior, not a hypothetical: any file uploaded
// through this provider that happens to be >= 16 MiB goes through the
// resumable path today, silently, whether or not it is ever tested.

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"cloudup/internal/provider"
)

// fakeDriveFile is one object or folder stored in the fake Drive.
type fakeDriveFile struct {
	id           string
	name         string
	mimeType     string
	parents      []string
	content      []byte
	modifiedTime time.Time
}

// toDriveFile builds the *drive.File JSON representation the real API would
// serve for this object - reusing the SDK's own type (rather than a
// hand-rolled map) is what guarantees field names/JSON tags can't drift from
// what Provider's Do() calls actually unmarshal into.
func (ff *fakeDriveFile) toDriveFile() *drive.File {
	df := &drive.File{
		Id:           ff.id,
		Name:         ff.name,
		MimeType:     ff.mimeType,
		Parents:      ff.parents,
		Size:         int64(len(ff.content)),
		ModifiedTime: ff.modifiedTime.UTC().Format(time.RFC3339),
		WebViewLink:  "https://drive.example/view/" + ff.id,
	}
	if ff.mimeType != driveFolderMimeType {
		sum := md5.Sum(ff.content)
		df.Md5Checksum = hex.EncodeToString(sum[:])
	}
	return df
}

// fakeUploadSession is one in-progress resumable upload, from the initial
// b2_start_large_file-equivalent POST (see handleResumableInit) through
// however many chunk POSTs handleUploadChunk needs to fully receive the
// content.
type fakeUploadSession struct {
	meta drive.File
	buf  []byte
	done bool
}

// fakeDriveServer implements just enough of the Drive v3 REST surface -
// files.create (plain metadata, multipart, and resumable), files.get
// (metadata and alt=media download), files.list, files.delete, about.get -
// to exercise Provider's CRUD methods against real wire traffic instead of a
// Go-level stand-in.
type fakeDriveServer struct {
	mu       sync.Mutex
	files    map[string]*fakeDriveFile
	sessions map[string]*fakeUploadSession
	nextID   int

	srv *httptest.Server
}

func newFakeDriveServer(t *testing.T) *fakeDriveServer {
	t.Helper()
	f := &fakeDriveServer{
		files:    map[string]*fakeDriveFile{},
		sessions: map[string]*fakeUploadSession{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /drive/v3/about", f.handleAbout)
	mux.HandleFunc("GET /drive/v3/files", f.handleList)
	mux.HandleFunc("POST /drive/v3/files", f.handleCreateMetadataOnly)
	mux.HandleFunc("GET /drive/v3/files/{id}", f.handleGetOrDownload)
	mux.HandleFunc("DELETE /drive/v3/files/{id}", f.handleDelete)
	mux.HandleFunc("POST /upload/drive/v3/files", f.handleUploadCreate)
	mux.HandleFunc("POST /upload/session/{id}", f.handleUploadChunk)

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// newIDLocked returns a fresh sequential file ID. Callers must already hold
// f.mu.
func (f *fakeDriveServer) newIDLocked() string {
	f.nextID++
	return fmt.Sprintf("file-%04d", f.nextID)
}

func (f *fakeDriveServer) handleAbout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, &drive.About{User: &drive.User{DisplayName: "Test User"}})
}

// handleCreateMetadataOnly serves plain "POST files" requests - i.e.
// Files.Create calls with no .Media() attached, which is how
// Provider.createFolder creates a folder (see the FilesCreateCall.doRequest
// doc comment in the SDK: mediaInfo_ is nil, so no "/upload" prefix and no
// uploadType is ever added to the request).
func (f *fakeDriveServer) handleCreateMetadataOnly(w http.ResponseWriter, r *http.Request) {
	var meta drive.File
	if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
		http.Error(w, "decoding metadata: "+err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	id := f.newIDLocked()
	ff := &fakeDriveFile{id: id, name: meta.Name, mimeType: meta.MimeType, parents: meta.Parents, modifiedTime: time.Now()}
	f.files[id] = ff
	f.mu.Unlock()

	writeJSON(w, ff.toDriveFile())
}

// handleUploadCreate serves "POST /upload/drive/v3/files", routing on the
// uploadType query parameter the SDK sets based on content size relative to
// googleapi.DefaultUploadChunkSize - see the package-level doc comment.
func (f *fakeDriveServer) handleUploadCreate(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("uploadType") {
	case "multipart":
		f.handleMultipartCreate(w, r)
	case "resumable":
		f.handleResumableInit(w, r)
	default:
		http.Error(w, "unsupported uploadType: "+r.URL.Query().Get("uploadType"), http.StatusBadRequest)
	}
}

// handleMultipartCreate serves the single-request upload path taken when
// the content is smaller than googleapi.DefaultUploadChunkSize: one
// multipart/related body with a JSON metadata part followed by a raw media
// part.
func (f *fakeDriveServer) handleMultipartCreate(w http.ResponseWriter, r *http.Request) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		http.Error(w, "expected a multipart/related body", http.StatusBadRequest)
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])

	metaPart, err := mr.NextPart()
	if err != nil {
		http.Error(w, "reading metadata part: "+err.Error(), http.StatusBadRequest)
		return
	}
	var meta drive.File
	if err := json.NewDecoder(metaPart).Decode(&meta); err != nil {
		http.Error(w, "decoding metadata part: "+err.Error(), http.StatusBadRequest)
		return
	}

	mediaPart, err := mr.NextPart()
	if err != nil {
		http.Error(w, "reading media part: "+err.Error(), http.StatusBadRequest)
		return
	}
	content, err := io.ReadAll(mediaPart)
	if err != nil {
		http.Error(w, "reading media content: "+err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	id := f.newIDLocked()
	ff := &fakeDriveFile{id: id, name: meta.Name, mimeType: meta.MimeType, parents: meta.Parents, content: content, modifiedTime: time.Now()}
	f.files[id] = ff
	f.mu.Unlock()

	writeJSON(w, ff.toDriveFile())
}

// handleResumableInit serves the first request of a resumable upload: a
// JSON-only body (no media yet) that creates a session and, per the
// protocol, is acknowledged with a Location header the client then sends
// every chunk to - see handleUploadChunk.
func (f *fakeDriveServer) handleResumableInit(w http.ResponseWriter, r *http.Request) {
	var meta drive.File
	if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
		http.Error(w, "decoding metadata: "+err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("session-%04d", f.nextID)
	f.sessions[id] = &fakeUploadSession{meta: meta}
	f.mu.Unlock()

	w.Header().Set("Location", f.srv.URL+"/upload/session/"+id)
	w.WriteHeader(http.StatusOK)
}

// contentRange* match the three shapes gensupport.ResumableUpload.
// doUploadRequest actually sends: a non-final chunk (total still unknown), a
// final chunk that carries real bytes, and a final chunk carrying zero bytes
// (sent when the content length is an exact multiple of the chunk size, so
// the previous chunk already delivered every byte and this call exists only
// to tell the server the upload is complete).
var (
	contentRangeNonFinal   = regexp.MustCompile(`^bytes (\d+)-\d+/\*$`)
	contentRangeFinal      = regexp.MustCompile(`^bytes (\d+)-\d+/\d+$`)
	contentRangeFinalEmpty = regexp.MustCompile(`^bytes \*/\d+$`)
)

// handleUploadChunk serves every request after the first one in a resumable
// session: it accepts a slice of the object's bytes (or none, for the
// exact-multiple-of-chunk-size close-out request) and either asks for more
// via the X-Http-Status-Code-Override: 308 convention
// (gensupport.statusResumeIncomplete) or, on the final chunk, creates the
// finished object.
func (f *fakeDriveServer) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	f.mu.Lock()
	sess, ok := f.sessions[id]
	f.mu.Unlock()
	if !ok || sess.done {
		writeDriveError(w, http.StatusNotFound, "no such upload session: "+id)
		return
	}

	cr := r.Header.Get("Content-Range")
	var (
		final    bool
		wantOff  int64
		haveWant bool
	)
	switch {
	case contentRangeFinal.MatchString(cr):
		m := contentRangeFinal.FindStringSubmatch(cr)
		wantOff, _ = strconv.ParseInt(m[1], 10, 64)
		final, haveWant = true, true
	case contentRangeNonFinal.MatchString(cr):
		m := contentRangeNonFinal.FindStringSubmatch(cr)
		wantOff, _ = strconv.ParseInt(m[1], 10, 64)
		final, haveWant = false, true
	case contentRangeFinalEmpty.MatchString(cr):
		final, haveWant = true, false
	default:
		http.Error(w, "malformed Content-Range: "+cr, http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if haveWant && wantOff != int64(len(sess.buf)) {
		http.Error(w, fmt.Sprintf("out-of-order chunk: offset %d, have %d bytes buffered", wantOff, len(sess.buf)), http.StatusBadRequest)
		return
	}
	sess.buf = append(sess.buf, body...)

	if !final {
		// See gensupport.statusResumeIncomplete: the client asked for this
		// non-standard 200-with-header convention via X-GUploader-No-308.
		w.Header().Set("X-Http-Status-Code-Override", "308")
		w.WriteHeader(http.StatusOK)
		return
	}

	sess.done = true
	id2 := f.newIDLocked()
	ff := &fakeDriveFile{id: id2, name: sess.meta.Name, mimeType: sess.meta.MimeType, parents: sess.meta.Parents, content: sess.buf, modifiedTime: time.Now()}
	f.files[id2] = ff
	delete(f.sessions, id)

	writeJSON(w, ff.toDriveFile())
}

func (f *fakeDriveServer) handleGetOrDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	ff, ok := f.files[id]
	f.mu.Unlock()
	if !ok {
		writeDriveError(w, http.StatusNotFound, "File not found: "+id)
		return
	}

	if r.URL.Query().Get("alt") == "media" {
		w.Header().Set("Content-Length", strconv.Itoa(len(ff.content)))
		w.WriteHeader(http.StatusOK)
		w.Write(ff.content)
		return
	}
	writeJSON(w, ff.toDriveFile())
}

func (f *fakeDriveServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	_, ok := f.files[id]
	delete(f.files, id)
	f.mu.Unlock()
	if !ok {
		writeDriveError(w, http.StatusNotFound, "File not found: "+id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// driveQueryInParents, driveQueryName and driveQueryMimeType parse exactly
// the query shapes findChild/List (googledrive.go) build via Q(...) -
// "'<id>' in parents [and name = '<n>'] [and mimeType = '<m>'] and trashed =
// false. This is not a general Drive query-language parser, only enough of
// one to interpret what this package's own quoteQueryValue ever produces.
var (
	driveQueryInParents = regexp.MustCompile(`^'((?:[^'\\]|\\.)*)' in parents`)
	driveQueryName      = regexp.MustCompile(`name = '((?:[^'\\]|\\.)*)'`)
	driveQueryMimeType  = regexp.MustCompile(`mimeType = '((?:[^'\\]|\\.)*)'`)
)

func unescapeDriveQueryValue(s string) string {
	return strings.ReplaceAll(s, `\'`, "'")
}

func (f *fakeDriveServer) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	m := driveQueryInParents.FindStringSubmatch(q)
	if m == nil {
		http.Error(w, "unsupported query: "+q, http.StatusBadRequest)
		return
	}
	parentID := unescapeDriveQueryValue(m[1])

	wantName, hasName := "", false
	if nm := driveQueryName.FindStringSubmatch(q); nm != nil {
		wantName, hasName = unescapeDriveQueryValue(nm[1]), true
	}
	wantMime, hasMime := "", false
	if mm := driveQueryMimeType.FindStringSubmatch(q); mm != nil {
		wantMime, hasMime = unescapeDriveQueryValue(mm[1]), true
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var out []*drive.File
	for _, ff := range f.files {
		if !slices.Contains(ff.parents, parentID) {
			continue
		}
		if hasName && ff.name != wantName {
			continue
		}
		if hasMime && ff.mimeType != wantMime {
			continue
		}
		out = append(out, ff.toDriveFile())
	}
	writeJSON(w, &drive.FileList{Files: out})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeDriveError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": status, "message": message},
	})
}

// newTestDriveProvider builds a Provider whose *drive.Service is pointed at
// fake instead of the real Google API - option.WithEndpoint retargets the
// SDK's base URL (both the plain "files"/"about" paths and the "/upload/..."
// paths resolve relative to it, since Drive serves both from the same host
// in production), and option.WithHTTPClient supplies a plain client with no
// OAuth wrapping, since the fake doesn't check Authorization at all - that
// wiring is already covered separately by TestAuthedHTTPClientCarriesDebugLog.
func newTestDriveProvider(t *testing.T, fake *fakeDriveServer, folderID string) *Provider {
	t.Helper()
	srv, err := drive.NewService(context.Background(),
		option.WithHTTPClient(fake.srv.Client()),
		option.WithEndpoint(fake.srv.URL+"/drive/v3/"),
	)
	if err != nil {
		t.Fatalf("drive.NewService() error = %v", err)
	}
	return &Provider{cfg: rawConfig{ConnectionID: "conn1", FolderID: folderID}, srv: srv}
}

func TestDriveTestConnection(t *testing.T) {
	p := newTestDriveProvider(t, newFakeDriveServer(t), "")
	if err := p.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection() error = %v", err)
	}
}

func TestDriveUploadDownloadRoundTrip(t *testing.T) {
	fake := newFakeDriveServer(t)
	p := newTestDriveProvider(t, fake, "")
	ctx := context.Background()

	content := []byte("hello, google drive")
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
	if result.RemoteURL == "" {
		t.Fatal("RemoteURL is empty")
	}

	var buf strings.Builder
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: "greeting.txt", Writer: &buf}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if buf.String() != string(content) {
		t.Fatalf("Download() content = %q, want %q", buf.String(), content)
	}
}

func TestDriveUploadCreatesIntermediateFolders(t *testing.T) {
	fake := newFakeDriveServer(t)
	p := newTestDriveProvider(t, fake, "")
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

	// Uploading a second file under the same nested path must reuse the
	// existing folders rather than creating duplicates.
	if _, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "a/b/second.txt",
		Size:       int64(len("more")),
		Reader:     bytes.NewReader([]byte("more")),
	}); err != nil {
		t.Fatalf("second Upload() error = %v", err)
	}
	entries, err = p.List(ctx, "a")
	if err != nil {
		t.Fatalf("List(a) after second upload error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("List(a) after second upload = %+v, want exactly one folder entry (no duplicate %q)", entries, "b")
	}
}

func TestDriveDeleteAndExists(t *testing.T) {
	fake := newFakeDriveServer(t)
	p := newTestDriveProvider(t, fake, "")
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

	// Deleting an already-missing object is idempotent, not an error - same
	// convention as every other provider (see e.g. b2.Provider.Delete).
	if err := p.Delete(ctx, "to-delete.txt"); err != nil {
		t.Fatalf("Delete() of already-missing file should be nil, got %v", err)
	}
	if err := p.Delete(ctx, "never-existed.txt"); err != nil {
		t.Fatalf("Delete() of never-existing file should be nil, got %v", err)
	}
}

func TestDriveVerifyChecksum(t *testing.T) {
	fake := newFakeDriveServer(t)
	p := newTestDriveProvider(t, fake, "")
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

func TestDriveUploadWithCustomRootFolder(t *testing.T) {
	fake := newFakeDriveServer(t)
	p := newTestDriveProvider(t, fake, "custom-root")
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
	var found bool
	for _, ff := range fake.files {
		if ff.name == "file.txt" && slices.Contains(ff.parents, "custom-root") {
			found = true
		}
	}
	fake.mu.Unlock()
	if !found {
		t.Fatal("uploaded file was not parented under the configured root folder ID")
	}
}

// TestDriveUploadMultipartVsResumable pins the wire-protocol split described
// in this file's package doc comment: content below googleapi.
// DefaultUploadChunkSize (16 MiB) must go through the fake's single-request
// multipart handler, and content at or above it must go through the
// multi-request resumable session handler - both must still produce a file
// whose checksum and downloaded content are correct.
func TestDriveUploadMultipartVsResumable(t *testing.T) {
	const chunkSize = 16 * 1024 * 1024 // googleapi.DefaultUploadChunkSize

	cases := []struct {
		name string
		size int
	}{
		{"below the resumable threshold", 1024},
		{"above the threshold, with a remainder chunk", chunkSize + 1024},
		{"exactly one chunk, exercising the empty final close-out request", chunkSize},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeDriveServer(t)
			p := newTestDriveProvider(t, fake, "")
			ctx := context.Background()

			content := make([]byte, c.size)
			for i := range content {
				content[i] = byte(i)
			}

			result, err := p.Upload(ctx, provider.UploadTask{
				RemotePath: "big.bin",
				Size:       int64(len(content)),
				Reader:     bytes.NewReader(content),
			})
			if err != nil {
				t.Fatalf("Upload() error = %v", err)
			}
			sum := md5.Sum(content)
			if want := hex.EncodeToString(sum[:]); result.Checksum != want {
				t.Fatalf("Checksum = %q, want %q", result.Checksum, want)
			}

			var buf strings.Builder
			if err := p.Download(ctx, provider.DownloadTask{RemotePath: "big.bin", Writer: &buf}); err != nil {
				t.Fatalf("Download() error = %v", err)
			}
			if buf.Len() != len(content) || buf.String() != string(content) {
				t.Fatalf("downloaded content length = %d, want %d (and bytes must match)", buf.Len(), len(content))
			}
		})
	}
}
