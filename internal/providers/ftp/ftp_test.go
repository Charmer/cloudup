package ftp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"testing"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"

	"cloudup/internal/provider"
	"cloudup/internal/provider/providertest"
)

const (
	testUser = "tester"
	testPass = "s3cr3t"
)

// testDriver is the smallest ftpserverlib.MainDriver that can authenticate
// one fixed user and serve an in-memory filesystem - no disk, no real
// network dependency beyond the loopback listener ftpserverlib itself opens.
type testDriver struct {
	fs afero.Fs
}

func (d *testDriver) GetSettings() (*ftpserver.Settings, error) {
	return &ftpserver.Settings{ListenAddr: "127.0.0.1:0"}, nil
}

func (d *testDriver) ClientConnected(cc ftpserver.ClientContext) (string, error) { return "test", nil }
func (d *testDriver) ClientDisconnected(cc ftpserver.ClientContext)              {}

func (d *testDriver) AuthUser(cc ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user != testUser || pass != testPass {
		return nil, fmt.Errorf("invalid credentials")
	}
	return d.fs, nil
}

func (d *testDriver) GetTLSConfig() (*tls.Config, error) { return nil, nil }

func newTestServer(t *testing.T) (addr string, fs afero.Fs) {
	t.Helper()

	fs = afero.NewMemMapFs()
	server := ftpserver.NewFtpServer(&testDriver{fs: fs})
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	go func() {
		_ = server.Serve()
	}()
	t.Cleanup(func() { _ = server.Stop() })

	return server.Addr(), fs
}

func newTestProvider(t *testing.T) *Provider {
	t.Helper()

	addr, _ := newTestServer(t)

	secrets := providertest.WithSecrets(map[string]map[string]string{
		"conn1": {secretUsername: testUser, secretPassword: testPass},
	})

	cfg := Config{ConnectionID: "conn1", Host: addr}
	rawCfg, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	p, err := New(rawCfg, secrets)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { p.(*Provider).mu.Lock(); _ = p.(*Provider).conn.Quit(); p.(*Provider).mu.Unlock() })
	return p.(*Provider)
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	p := newTestProvider(t)
	content := []byte("hello from cloudup over FTP")

	result, err := p.Upload(context.Background(), provider.UploadTask{
		RemotePath: "/dir/sub/file.txt",
		Size:       int64(len(content)),
		Reader:     bytes.NewReader(content),
	})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.ChecksumAlgo != checksumAlgo {
		t.Fatalf("ChecksumAlgo = %q, want %q", result.ChecksumAlgo, checksumAlgo)
	}

	var buf bytes.Buffer
	if err := p.Download(context.Background(), provider.DownloadTask{
		RemotePath: "/dir/sub/file.txt",
		Writer:     &buf,
	}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if buf.String() != string(content) {
		t.Fatalf("downloaded content = %q, want %q", buf.String(), string(content))
	}

	ok, err := p.VerifyChecksum(context.Background(), "/dir/sub/file.txt", result.ChecksumAlgo, result.Checksum)
	if err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if !ok {
		t.Fatal("VerifyChecksum() = false, want true")
	}
}

func TestExists(t *testing.T) {
	p := newTestProvider(t)

	exists, err := p.Exists(context.Background(), "/missing.txt")
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if exists {
		t.Fatal("Exists() = true for a file that was never uploaded")
	}

	if _, err := p.Upload(context.Background(), provider.UploadTask{
		RemotePath: "/present.txt",
		Size:       1,
		Reader:     bytes.NewReader([]byte("x")),
	}); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	exists, err = p.Exists(context.Background(), "/present.txt")
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if !exists {
		t.Fatal("Exists() = false for a file that was just uploaded")
	}
}

func TestListAndDelete(t *testing.T) {
	p := newTestProvider(t)

	for _, name := range []string{"/a.txt", "/b.txt"} {
		if _, err := p.Upload(context.Background(), provider.UploadTask{
			RemotePath: name,
			Size:       1,
			Reader:     bytes.NewReader([]byte("x")),
		}); err != nil {
			t.Fatalf("Upload(%q) error = %v", name, err)
		}
	}

	entries, err := p.List(context.Background(), "/")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() returned %d entries, want 2", len(entries))
	}

	if err := p.Delete(context.Background(), "/a.txt"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	exists, err := p.Exists(context.Background(), "/a.txt")
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if exists {
		t.Fatal("Exists() = true after Delete()")
	}
}

func TestVerifyChecksumRejectsForeignAlgo(t *testing.T) {
	p := newTestProvider(t)
	if _, err := p.VerifyChecksum(context.Background(), "/x", "md5", "deadbeef"); err == nil {
		t.Fatal("VerifyChecksum() with a foreign algo label should error, got nil")
	}
}

func TestTestConnection(t *testing.T) {
	p := newTestProvider(t)
	if err := p.TestConnection(context.Background()); err != nil {
		t.Fatalf("TestConnection() error = %v", err)
	}
}

func TestConfigFieldsRouteCredentialsToSecrets(t *testing.T) {
	p := &Provider{}
	var sawPasswordFields int
	for _, f := range p.ConfigFields() {
		if f.Type == provider.FieldPassword {
			sawPasswordFields++
		}
	}
	if sawPasswordFields != 2 {
		t.Fatalf("expected username and password to be FieldPassword, got %d such fields", sawPasswordFields)
	}
}
