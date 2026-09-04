// Package sftp implements provider.Provider over SFTP (SSH File Transfer
// Protocol) - not to be confused with FTPS (FTP over TLS, see
// internal/providers/ftp), an unrelated protocol that happens to share the
// "secure FTP" name.
package sftp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"

	sftpclient "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"cloudup/internal/provider"
	"cloudup/internal/registry"
	"cloudup/internal/streamio"
)

const Type = "sftp"

const (
	secretUsername = "username"
	secretPassword = "password"
)

// checksumAlgo is the label stored in UploadResult/upload_log. The SFTP
// protocol (draft-ietf-secsh-filexfer) has no standard operation for asking
// the server to hash a file's content - a few servers expose one through
// vendor SFTP extensions, but nothing worth relying on across
// implementations - so this provider computes its own SHA-256 while
// streaming the upload and verifies later by re-downloading and re-hashing,
// the same strategy internal/providers/webdav and internal/providers/ftp use.
const checksumAlgo = provider.ChecksumSHA256SelfComputed

func init() {
	registry.Register(Type, New)
	registry.RegisterSchema(Type, (&Provider{}).ConfigFields)
}

// Config is the non-secret part of an SFTP connection, persisted in the
// JSON config file. Credentials live in the secret store.
type Config struct {
	ConnectionID string `json:"connectionId"`
	Host         string `json:"host"`
	DisplayName  string `json:"displayName"`
}

// Provider implements provider.Provider (and several optional feature
// interfaces) over an SFTP connection.
//
// Known limitation: authentication is password-only for now (matching
// internal/providers/webdav's model) - many real-world SFTP servers require
// public-key auth instead/as well, which would need a way to accept a
// multi-line private key through the generic ConfigFields() form
// (ConnectionsView.vue renders every FieldPassword as a single-line input
// today). Left for a follow-up rather than half-implemented here.
//
// Known limitation: host keys are not verified (HostKeyCallback is
// ssh.InsecureIgnoreHostKey - see New) since there is no known_hosts-style
// store or UI to pin/confirm a fingerprint yet. This accepts whatever host
// key the server presents, the same trust-on-first-use gap most GUI SFTP
// clients paper over with a "trust this host?" prompt cloudup does not yet
// have. Worth tightening in a follow-up, not a reason to block FTP/SFTP
// support entirely in the meantime.
type Provider struct {
	cfg    Config
	addr   string
	sshCfg *ssh.ClientConfig

	// *sftpclient.Client is safe for concurrent use by multiple goroutines
	// (unlike internal/providers/ftp's *ftp.ServerConn) - mu only guards
	// (re)connecting, never a call already in flight on client.
	mu     sync.Mutex
	ssh    *ssh.Client
	client *sftpclient.Client
}

// New is the registry.Factory for the "sftp" provider type.
func New(rawCfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
	var cfg Config
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return nil, fmt.Errorf("sftp: invalid config: %w", err)
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("sftp: host is required")
	}

	username, err := secrets.Get(cfg.ConnectionID, secretUsername)
	if err != nil {
		return nil, fmt.Errorf("sftp: reading username secret: %w", err)
	}
	password, err := secrets.Get(cfg.ConnectionID, secretPassword)
	if err != nil {
		return nil, fmt.Errorf("sftp: reading password secret: %w", err)
	}

	addr := cfg.Host
	if !strings.Contains(addr, ":") {
		addr += ":22"
	}

	sshCfg := &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // see Provider's doc comment
		Timeout:         30 * time.Second,
	}

	p := &Provider{cfg: cfg, addr: addr, sshCfg: sshCfg}
	if _, err := p.getClient(); err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return p, nil
}

func (p *Provider) Type() string { return Type }

func (p *Provider) DisplayName() string {
	if p.cfg.DisplayName != "" {
		return p.cfg.DisplayName
	}
	return "SFTP (" + p.cfg.Host + ")"
}

func (p *Provider) TestConnection(ctx context.Context) error {
	client, err := p.getClient()
	if err != nil {
		return fmt.Errorf("sftp: connection test failed: %w", err)
	}
	if _, err := client.Getwd(); err != nil {
		return fmt.Errorf("sftp: connection test failed: %w", err)
	}
	return nil
}

// Upload streams task.Reader to task.RemotePath, computing a SHA-256
// checksum while streaming so no second read of the source is needed.
func (p *Provider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	client, err := p.getClient()
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("sftp: upload %q: %w", task.RemotePath, err)
	}

	if dir := path.Dir(task.RemotePath); dir != "." && dir != "/" && dir != "" {
		if err := client.MkdirAll(dir); err != nil {
			return provider.UploadResult{}, fmt.Errorf("sftp: upload %q: creating %q: %w", task.RemotePath, dir, err)
		}
	}

	f, err := client.Create(task.RemotePath)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("sftp: upload %q: %w", task.RemotePath, err)
	}
	defer f.Close()

	h := sha256.New()
	reader := io.TeeReader(&streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}, h)

	if _, err := io.Copy(f, reader); err != nil {
		return provider.UploadResult{}, fmt.Errorf("sftp: upload %q: %w", task.RemotePath, err)
	}

	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    p.remoteURL(task.RemotePath),
		ChecksumAlgo: checksumAlgo,
		Checksum:     hex.EncodeToString(h.Sum(nil)),
	}, nil
}

func (p *Provider) Download(ctx context.Context, task provider.DownloadTask) error {
	client, err := p.getClient()
	if err != nil {
		return fmt.Errorf("sftp: download %q: %w", task.RemotePath, err)
	}

	f, err := client.Open(task.RemotePath)
	if err != nil {
		return fmt.Errorf("sftp: download %q: %w", task.RemotePath, err)
	}
	defer f.Close()

	writer := task.Writer
	if task.Progress != nil {
		var total int64
		if info, err := f.Stat(); err == nil {
			total = info.Size()
		}
		writer = &streamio.ProgressWriter{W: writer, Total: total, OnProgress: task.Progress}
	}

	if _, err := io.Copy(writer, f); err != nil {
		return fmt.Errorf("sftp: download %q: %w", task.RemotePath, err)
	}
	return nil
}

func (p *Provider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	client, err := p.getClient()
	if err != nil {
		return nil, fmt.Errorf("sftp: list %q: %w", remotePath, err)
	}

	infos, err := client.ReadDir(remotePath)
	if err != nil {
		return nil, fmt.Errorf("sftp: list %q: %w", remotePath, err)
	}
	entries := make([]provider.RemoteEntry, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, provider.RemoteEntry{
			Path:    path.Join(remotePath, info.Name()),
			Name:    info.Name(),
			Size:    info.Size(),
			IsDir:   info.IsDir(),
			ModTime: info.ModTime(),
		})
	}
	return entries, nil
}

func (p *Provider) Delete(ctx context.Context, remotePath string) error {
	client, err := p.getClient()
	if err != nil {
		return fmt.Errorf("sftp: delete %q: %w", remotePath, err)
	}
	if err := client.Remove(remotePath); err != nil {
		return fmt.Errorf("sftp: delete %q: %w", remotePath, err)
	}
	return nil
}

// Exists implements provider.ExistenceChecker.
func (p *Provider) Exists(ctx context.Context, remotePath string) (bool, error) {
	client, err := p.getClient()
	if err != nil {
		return false, fmt.Errorf("sftp: stat %q: %w", remotePath, err)
	}

	if _, err := client.Stat(remotePath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("sftp: stat %q: %w", remotePath, err)
	}
	return true, nil
}

// VerifyChecksum implements provider.ChecksumVerifier by re-downloading the
// object and recomputing the same self-computed SHA-256 Upload produced.
// checksumAlgo values produced by other providers are rejected, since they
// were not computed by this code path.
func (p *Provider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	if algo != checksumAlgo {
		return false, fmt.Errorf("sftp: cannot verify checksum of algo %q", algo)
	}

	client, err := p.getClient()
	if err != nil {
		return false, fmt.Errorf("sftp: verify %q: %w", remotePath, err)
	}

	f, err := client.Open(remotePath)
	if err != nil {
		return false, fmt.Errorf("sftp: verify %q: %w", remotePath, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("sftp: verify %q: %w", remotePath, err)
	}

	return hex.EncodeToString(h.Sum(nil)) == checksum, nil
}

// ConfigFields implements provider.ConfigSchema.
func (p *Provider) ConfigFields() []provider.FieldSpec {
	return []provider.FieldSpec{
		{Key: "host", Label: "Host[:port] (default port 22)", Type: provider.FieldText, Required: true},
		{Key: secretUsername, Label: "Username", Type: provider.FieldPassword, Required: true},
		{Key: secretPassword, Label: "Password", Type: provider.FieldPassword, Required: true},
	}
}

func (p *Provider) remoteURL(remotePath string) string {
	return "sftp://" + p.cfg.Host + path.Clean("/"+remotePath)
}

// getClient returns a live *sftpclient.Client, (re)dialing only when there
// is no existing connection or a cheap round trip (Getwd) shows the old one
// is dead - idle SSH servers commonly close the connection after a timeout
// between uploads that the queue's own pacing/backoff can easily exceed.
func (p *Provider) getClient() (*sftpclient.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client != nil {
		if _, err := p.client.Getwd(); err == nil {
			return p.client, nil
		}
		_ = p.client.Close()
		_ = p.ssh.Close()
		p.client = nil
		p.ssh = nil
	}

	sshConn, err := ssh.Dial("tcp", p.addr, p.sshCfg)
	if err != nil {
		return nil, fmt.Errorf("dial %q: %w", p.addr, err)
	}
	client, err := sftpclient.NewClient(sshConn)
	if err != nil {
		_ = sshConn.Close()
		return nil, fmt.Errorf("open sftp session: %w", err)
	}

	p.ssh = sshConn
	p.client = client
	return client, nil
}
