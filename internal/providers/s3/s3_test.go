package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"cloudup/internal/provider"
	"cloudup/internal/provider/providertest"
)

const testBucket = "test-bucket"

func newTestProvider(t *testing.T) *Provider {
	t.Helper()

	backend := s3mem.New()
	if err := backend.CreateBucket(testBucket); err != nil {
		t.Fatalf("CreateBucket() error = %v", err)
	}
	faker := gofakes3.New(backend)
	server := httptest.NewServer(faker.Server())
	t.Cleanup(server.Close)

	secrets := providertest.NewMemSecretStore()
	secrets.Set("conn1", secretAccessKeyID, "FAKE_ACCESS_KEY")
	secrets.Set("conn1", secretSecretAccessKey, "FAKE_SECRET_KEY")

	rawCfg, err := json.Marshal(rawConfig{
		ConnectionID: "conn1",
		Bucket:       testBucket,
		Region:       "us-east-1",
		Endpoint:     server.URL,
		UsePathStyle: "true",
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	p, err := New(rawCfg, secrets)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return p.(*Provider)
}

func TestUploadDownloadListDelete(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	content := []byte("hello, s3")
	result, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "greeting.txt",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo || result.Checksum == "" {
		t.Fatalf("Upload() result missing checksum: %+v", result)
	}

	entries, err := p.List(ctx, "")
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
		}
	}
	if !found {
		t.Fatal("List() did not return uploaded file")
	}

	var buf bytes.Buffer
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: "greeting.txt", Writer: &buf}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if buf.String() != string(content) {
		t.Fatalf("Download() content = %q, want %q", buf.String(), content)
	}

	ok, err := p.VerifyChecksum(ctx, "greeting.txt", result.ChecksumAlgo, result.Checksum)
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if !ok {
		t.Fatal("VerifyChecksum() = false, want true for untouched upload")
	}

	ok, err = p.VerifyChecksum(ctx, "greeting.txt", result.ChecksumAlgo, "deadbeef")
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if ok {
		t.Fatal("VerifyChecksum() = true, want false for mismatched checksum")
	}

	exists, err := p.Exists(ctx, "greeting.txt")
	if err != nil || !exists {
		t.Fatalf("Exists() = %v, %v, want true, nil", exists, err)
	}

	if err := p.Delete(ctx, "greeting.txt"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	exists, err = p.Exists(ctx, "greeting.txt")
	if err != nil {
		t.Fatalf("Exists() after delete error = %v", err)
	}
	if exists {
		t.Fatal("Exists() = true after Delete(), want false")
	}
}

func TestUploadMultipart(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	// Small part size so a short payload still spans several parts,
	// exercising the multi-part loop without needing a multi-megabyte file.
	content := []byte(strings.Repeat("0123456789", 5)) // 50 bytes
	result, err := p.UploadMultipart(ctx, provider.UploadTask{
		RemotePath: "multipart.bin",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	}, 7)
	if err != nil {
		t.Fatalf("UploadMultipart() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo || result.Checksum == "" {
		t.Fatalf("UploadMultipart() result missing checksum: %+v", result)
	}

	var buf bytes.Buffer
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: "multipart.bin", Writer: &buf}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if buf.String() != string(content) {
		t.Fatalf("Download() content = %q, want %q", buf.String(), content)
	}

	ok, err := p.VerifyChecksum(ctx, "multipart.bin", result.ChecksumAlgo, result.Checksum)
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if !ok {
		t.Fatal("VerifyChecksum() = false, want true for untouched multipart upload")
	}
}

func TestUploadMultipartMatchesSinglePartChecksum(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()
	content := []byte(strings.Repeat("abcde", 4)) // 20 bytes

	single, err := p.Upload(ctx, provider.UploadTask{
		RemotePath: "single.bin",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	multi, err := p.UploadMultipart(ctx, provider.UploadTask{
		RemotePath: "multi.bin",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	}, 6)
	if err != nil {
		t.Fatalf("UploadMultipart() error = %v", err)
	}

	if single.Checksum != multi.Checksum {
		t.Fatalf("checksum mismatch between single-part (%q) and multipart (%q) upload of the same content", single.Checksum, multi.Checksum)
	}
}

func TestUploadMultipartParallelRejectsMissingReaderAt(t *testing.T) {
	p := newTestProvider(t)
	_, err := p.UploadMultipartParallel(context.Background(), provider.UploadTask{
		RemotePath: "x.bin",
		Size:       20,
		Reader:     bytes.NewReader(make([]byte, 20)),
		// ReaderAt intentionally left nil.
	}, 7, 4)
	if err == nil {
		t.Fatal("UploadMultipartParallel() without a ReaderAt should fail")
	}
}

func TestUploadMultipartParallelRejectsNonPositivePartSize(t *testing.T) {
	p := newTestProvider(t)
	r := bytes.NewReader(make([]byte, 20))
	_, err := p.UploadMultipartParallel(context.Background(), provider.UploadTask{
		RemotePath: "x.bin",
		Size:       20,
		Reader:     r,
		ReaderAt:   r,
	}, 0, 4)
	if err == nil {
		t.Fatal("UploadMultipartParallel() with partSize <= 0 should fail")
	}
}

func TestUploadMultipartParallelRoundTrip(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	content := []byte(strings.Repeat("0123456789", 50)) // 500 bytes
	r := bytes.NewReader(content)
	result, err := p.UploadMultipartParallel(ctx, provider.UploadTask{
		RemotePath: "parallel.bin",
		Size:       int64(len(content)),
		Reader:     r,
		ReaderAt:   r,
	}, 17, 4)
	if err != nil {
		t.Fatalf("UploadMultipartParallel() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo || result.Checksum == "" {
		t.Fatalf("UploadMultipartParallel() result missing checksum: %+v", result)
	}

	var buf bytes.Buffer
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: "parallel.bin", Writer: &buf}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("downloaded content mismatch: got %d bytes, want %d", buf.Len(), len(content))
	}

	ok, err := p.VerifyChecksum(ctx, "parallel.bin", result.ChecksumAlgo, result.Checksum)
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if !ok {
		t.Fatal("VerifyChecksum() = false, want true for untouched parallel multipart upload")
	}
}

// TestUploadMultipartParallelMatchesSequentialChecksum is the parallel-path
// counterpart to TestUploadMultipartMatchesSinglePartChecksum: the same
// content must produce the same checksum whether it goes through Upload,
// the sequential UploadMultipart, or the concurrent
// UploadMultipartParallel, since internal/history's verification cannot
// tell any of the three paths apart.
func TestUploadMultipartParallelMatchesSequentialChecksum(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()
	content := []byte(strings.Repeat("checksum-parity-", 20))

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
	}, 13, 4)
	if err != nil {
		t.Fatalf("UploadMultipartParallel() error = %v", err)
	}

	if single.Checksum != parallel.Checksum {
		t.Fatalf("checksum mismatch: single-part %q, parallel multipart %q", single.Checksum, parallel.Checksum)
	}
}

func TestProviderImplementsParallelMultipartUploader(t *testing.T) {
	var _ provider.ParallelMultipartUploader = (*Provider)(nil)
}

func TestTestConnection(t *testing.T) {
	p := newTestProvider(t)
	if err := p.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection() error = %v", err)
	}
}

func TestConfigFieldsMarksSecretsAsPassword(t *testing.T) {
	p := newTestProvider(t)
	fields := p.ConfigFields()
	for _, f := range fields {
		if (f.Key == secretAccessKeyID || f.Key == secretSecretAccessKey) && f.Type != provider.FieldPassword {
			t.Fatalf("field %q has type %q, want %q", f.Key, f.Type, provider.FieldPassword)
		}
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	secrets := providertest.NewMemSecretStore()
	if _, err := New(json.RawMessage(`{"region":"us-east-1"}`), secrets); err == nil {
		t.Fatal("New() with missing bucket: expected error, got nil")
	}
	if _, err := New(json.RawMessage(`{"bucket":"x"}`), secrets); err == nil {
		t.Fatal("New() with missing region: expected error, got nil")
	}
	if _, err := New(json.RawMessage(`not json`), secrets); err == nil {
		t.Fatal("New() with malformed json: expected error, got nil")
	}
}

func TestObjectURLUsesEndpointWhenSet(t *testing.T) {
	p := newTestProvider(t)
	got := p.objectURL("greeting.txt")
	if !strings.HasSuffix(got, "/"+testBucket+"/greeting.txt") {
		t.Fatalf("objectURL() = %q, want suffix /%s/greeting.txt", got, testBucket)
	}
}
