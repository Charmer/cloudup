package sftp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"testing"

	sftpclient "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"cloudup/internal/provider"
	"cloudup/internal/provider/providertest"
)

const (
	testUser = "tester"
	testPass = "s3cr3t"
)

// newTestServer starts a minimal real SSH server on the loopback interface,
// serving one "sftp" subsystem session via pkg/sftp's in-memory handler.
// This exercises the exact same ssh.Dial + sftpclient.NewClient(sshConn)
// path New (sftp.go) uses in production - not a fake protocol shortcut.
func newTestServer(t *testing.T) (addr string) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if c.User() == testUser && string(password) == testPass {
				return nil, nil
			}
			return nil, fmt.Errorf("invalid credentials")
		},
	}
	cfg.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go serveOneConn(listener, cfg)

	return listener.Addr().String()
}

func serveOneConn(listener net.Listener, cfg *ssh.ServerConfig) {
	nConn, err := listener.Accept()
	if err != nil {
		return // listener closed by t.Cleanup
	}

	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go handleSessionRequests(channel, requests)
	}
}

// handleSessionRequests answers only the one request an SFTP client actually
// sends on a session channel: "subsystem" naming "sftp".
func handleSessionRequests(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		isSFTP := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
		if req.WantReply {
			_ = req.Reply(isSFTP, nil)
		}
		if isSFTP {
			// InMemHandler backs the whole filesystem with an in-process
			// map (request-example.go), not the host OS's real filesystem -
			// unlike the plain sftp.NewServer, which serves the actual
			// local disk and would be the wrong thing to point a test at.
			server := sftpclient.NewRequestServer(channel, sftpclient.InMemHandler())
			_ = server.Serve()
			return
		}
	}
}

func newTestProvider(t *testing.T) *Provider {
	t.Helper()

	addr := newTestServer(t)

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
	return p.(*Provider)
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	p := newTestProvider(t)
	content := []byte("hello from cloudup over SFTP")

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

func TestNewRejectsWrongCredentials(t *testing.T) {
	addr := newTestServer(t)

	secrets := providertest.WithSecrets(map[string]map[string]string{
		"conn1": {secretUsername: testUser, secretPassword: "wrong"},
	})
	cfg := Config{ConnectionID: "conn1", Host: addr}
	rawCfg, _ := json.Marshal(cfg)

	if _, err := New(rawCfg, secrets); err == nil {
		t.Fatal("New() with wrong password should error, got nil")
	}
}
