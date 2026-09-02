package onedrive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"cloudup/internal/provider"
)

// fakeGraphServer is a minimal fake of the Microsoft Graph drive API,
// backed by an in-memory file map, good enough to exercise this package's
// TestConnection/Upload/Download/List/Delete/Exists/VerifyChecksum and the
// chunked upload-session flow against real HTTP round-trips without
// touching the real Graph API.
type fakeGraphServer struct {
	files map[string][]byte // Graph colon-path (no leading "/root:/" prefix, no trailing ":") -> content

	// sessions holds the bytes accumulated so far per upload session token,
	// and sessionSeq numbers newly created sessions. chunkSizes records the
	// size of every chunk each session received, in arrival order, so tests
	// can assert how the payload was actually split.
	sessions   map[string][]byte
	chunkSizes map[string][]int
	sessionSeq int
}

func newFakeGraphServer(t *testing.T) (*fakeGraphServer, *httptest.Server) {
	t.Helper()
	fake := &fakeGraphServer{
		files:      map[string][]byte{},
		sessions:   map[string][]byte{},
		chunkSizes: map[string][]int{},
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/v1.0/me/drive", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fake-drive"}`)
	})

	mux.HandleFunc("/v1.0/me/drive/root", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not implemented in fake", http.StatusNotImplemented)
	})

	// Everything under /v1.0/me/drive/root:/ is a colon-addressed item.
	// Requests are routed by suffix (":", ":/content", ":/children",
	// ":/createUploadSession") since net/http's mux can't pattern-match the
	// colon form itself.
	mux.HandleFunc("/v1.0/me/drive/root:/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1.0/me/drive/root:/")
		switch {
		case strings.HasSuffix(rest, ":/content"):
			fake.handleContent(w, r, strings.TrimSuffix(rest, ":/content"))
		case strings.HasSuffix(rest, ":/children"):
			fake.handleChildren(w, r, strings.TrimSuffix(rest, ":/children"))
		case strings.HasSuffix(rest, ":/createUploadSession"):
			fake.handleCreateUploadSession(w, r, strings.TrimSuffix(rest, ":/createUploadSession"))
		case strings.HasSuffix(rest, ":"):
			fake.handleItem(w, r, strings.TrimSuffix(rest, ":"))
		default:
			http.Error(w, "unrecognized path", http.StatusNotFound)
		}
	})

	mux.HandleFunc("/upload/", func(w http.ResponseWriter, r *http.Request) {
		fake.handleChunk(w, r, strings.TrimPrefix(r.URL.Path, "/upload/"))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return fake, server
}

func (f *fakeGraphServer) handleContent(w http.ResponseWriter, r *http.Request, path string) {
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		f.files[path] = body
		writeDriveItem(w, path, body)
	case http.MethodGet:
		content, ok := f.files[path]
		if !ok {
			writeNotFound(w)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Write(content)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeGraphServer) handleItem(w http.ResponseWriter, r *http.Request, path string) {
	content, ok := f.files[path]
	switch r.Method {
	case http.MethodGet:
		if !ok {
			writeNotFound(w)
			return
		}
		writeDriveItem(w, path, content)
	case http.MethodDelete:
		if !ok {
			writeNotFound(w)
			return
		}
		delete(f.files, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeGraphServer) handleChildren(w http.ResponseWriter, _ *http.Request, dir string) {
	var value []json.RawMessage
	seen := map[string]bool{}
	for p, content := range f.files {
		rest := strings.TrimPrefix(p, dir)
		if rest == p && dir != "" {
			continue // p doesn't have dir as a prefix at all
		}
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" || strings.Contains(rest, "/") || seen[rest] {
			continue // not a direct child, or already emitted
		}
		seen[rest] = true
		b, _ := json.Marshal(map[string]any{
			"name": rest,
			"size": len(content),
			"file": map[string]any{"hashes": map[string]any{"quickXorHash": fmt.Sprintf("hash-%s", rest)}},
		})
		value = append(value, b)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"value": value})
}

func (f *fakeGraphServer) handleCreateUploadSession(w http.ResponseWriter, r *http.Request, path string) {
	f.sessionSeq++
	token := fmt.Sprintf("session-%d", f.sessionSeq)
	f.sessions[token] = nil
	f.chunkSizes[token] = nil
	// Encode the target path into the upload URL itself, the simplest way
	// for this fake to remember which file a later chunk PUT belongs to.
	uploadURL := fmt.Sprintf("%s/upload/%s?path=%s", "http://"+r.Host, token, path)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"uploadUrl": uploadURL})
}

func (f *fakeGraphServer) handleChunk(w http.ResponseWriter, r *http.Request, token string) {
	got, ok := f.sessions[token]
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	var start, end, total int64
	cr := r.Header.Get("Content-Range")
	if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); err != nil {
		http.Error(w, "bad Content-Range: "+cr, http.StatusBadRequest)
		return
	}
	if start != int64(len(got)) {
		http.Error(w, fmt.Sprintf("unexpected offset: got %d, want %d", start, len(got)), http.StatusRequestedRangeNotSatisfiable)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	f.sessions[token] = append(f.sessions[token], body...)
	f.chunkSizes[token] = append(f.chunkSizes[token], len(body))

	if end+1 < total {
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"nextExpectedRanges": []string{fmt.Sprintf("%d-", end+1)}})
		return
	}

	path := r.URL.Query().Get("path")
	content := f.sessions[token]
	f.files[path] = content
	delete(f.sessions, token)
	writeDriveItem(w, path, content)
}

func writeDriveItem(w http.ResponseWriter, path string, content []byte) {
	name := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		name = path[i+1:]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"name":   name,
		"size":   len(content),
		"webUrl": "https://example.sharepoint.com/" + path,
		"file":   map[string]any{"hashes": map[string]any{"quickXorHash": fmt.Sprintf("hash-of-%d-bytes", len(content))}},
	})
}

func writeNotFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"error":{"code":"itemNotFound","message":"not found"}}`)
}

// withGraphBaseURLOverridden points graphBaseURL at serverURL for the
// duration of the test, restoring the original afterwards.
func withGraphBaseURLOverridden(t *testing.T, serverURL string) {
	t.Helper()
	prev := graphBaseURL
	graphBaseURL = serverURL + "/v1.0"
	t.Cleanup(func() { graphBaseURL = prev })
}

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
	_, server := newFakeGraphServer(t)
	withGraphBaseURLOverridden(t, server.URL)
	p := newTestProvider(server)

	if err := p.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection() error = %v", err)
	}
}

func TestUploadDownloadRoundTripAndVerifyChecksum(t *testing.T) {
	_, server := newFakeGraphServer(t)
	withGraphBaseURLOverridden(t, server.URL)
	p := newTestProvider(server)

	content := []byte("hello onedrive")
	result, err := p.Upload(context.Background(), uploadTask("dir/file.txt", content))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo {
		t.Fatalf("ChecksumAlgo = %q, want %q", result.ChecksumAlgo, checksumAlgo)
	}
	if result.Checksum == "" {
		t.Fatal("Checksum is empty")
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
	_, server := newFakeGraphServer(t)
	withGraphBaseURLOverridden(t, server.URL)
	p := newTestProvider(server)

	if _, err := p.Upload(context.Background(), uploadTask("a.txt", []byte("x"))); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	if err := p.Delete(context.Background(), "a.txt"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := p.Delete(context.Background(), "a.txt"); err != nil {
		t.Fatalf("Delete() of already-missing object: error = %v, want nil", err)
	}
}

func TestExists(t *testing.T) {
	_, server := newFakeGraphServer(t)
	withGraphBaseURLOverridden(t, server.URL)
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

func TestList(t *testing.T) {
	_, server := newFakeGraphServer(t)
	withGraphBaseURLOverridden(t, server.URL)
	p := newTestProvider(server)

	if _, err := p.Upload(context.Background(), uploadTask("dir/a.txt", []byte("a"))); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if _, err := p.Upload(context.Background(), uploadTask("dir/b.txt", []byte("bb"))); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	entries, err := p.List(context.Background(), "dir")
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

// withChunkGranularity shrinks Graph's real 320 KiB chunk granularity for
// the duration of the test, restoring it afterwards - see dropbox's
// identically named helper for why.
func withChunkGranularity(t *testing.T, granularity int64) {
	t.Helper()
	prev := chunkGranularity
	chunkGranularity = granularity
	t.Cleanup(func() { chunkGranularity = prev })
}

// TestUploadMultipartRoundTrip covers the main contract: a payload spanning
// several chunks arrives intact (proving the Content-Range offsets and chunk
// slicing are right - the fake rejects a wrong offset), progress is reported
// continuously across the whole file rather than restarting per chunk, and
// the resulting checksum verifies end-to-end through VerifyChecksum.
func TestUploadMultipartRoundTrip(t *testing.T) {
	fake, server := newFakeGraphServer(t)
	withGraphBaseURLOverridden(t, server.URL)
	withChunkGranularity(t, 10)
	p := newTestProvider(server)
	ctx := context.Background()

	// 95 bytes in 10-byte chunks: 9 full chunks plus a 5-byte remainder, so
	// the final chunk is partial rather than empty.
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
	if result.Checksum == "" {
		t.Fatal("Checksum is empty")
	}

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

	wantChunks := []int{10, 10, 10, 10, 10, 10, 10, 10, 10, 5}
	var gotChunks []int
	for _, sizes := range fake.chunkSizes {
		gotChunks = sizes
	}
	if fmt.Sprint(gotChunks) != fmt.Sprint(wantChunks) {
		t.Fatalf("session received chunk sizes %v, want %v (9 full chunks plus a 5-byte final chunk)", gotChunks, wantChunks)
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

// TestUploadMultipartChunkBoundaries covers a payload that is an exact
// multiple of the chunk size and one that is not, plus one smaller than a
// single chunk - all of which still have to produce the correct file.
func TestUploadMultipartChunkBoundaries(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		{"exact multiple of chunk size", 40},
		{"not a multiple of chunk size", 43},
		{"smaller than one chunk", 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, server := newFakeGraphServer(t)
			withGraphBaseURLOverridden(t, server.URL)
			withChunkGranularity(t, 10)
			p := newTestProvider(server)
			ctx := context.Background()

			content := []byte(strings.Repeat("x", c.size))
			result, err := p.UploadMultipart(ctx, uploadTask("f.bin", content), 10)
			if err != nil {
				t.Fatalf("UploadMultipart() error = %v", err)
			}
			if result.Checksum == "" {
				t.Fatal("Checksum is empty")
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

// TestUploadMultipartEmptyFileDelegatesToUpload confirms a 0-byte file goes
// through Upload rather than the chunked-session protocol (see
// UploadMultipart's doc comment).
func TestUploadMultipartEmptyFileDelegatesToUpload(t *testing.T) {
	_, server := newFakeGraphServer(t)
	withGraphBaseURLOverridden(t, server.URL)
	p := newTestProvider(server)

	result, err := p.UploadMultipart(context.Background(), uploadTask("empty.bin", nil), 10)
	if err != nil {
		t.Fatalf("UploadMultipart() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo {
		t.Fatalf("ChecksumAlgo = %q, want %q", result.ChecksumAlgo, checksumAlgo)
	}
}
