// Package ftp implements provider.Provider over the FTP protocol (RFC 959),
// with optional explicit TLS (FTPS, "AUTH TLS") for servers that require an
// encrypted control/data channel.
package ftp

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"path"
	"strings"
	"sync"

	"github.com/jlaffaye/ftp"

	"cloudup/internal/provider"
	"cloudup/internal/registry"
	"cloudup/internal/streamio"
)

const Type = "ftp"

const (
	secretUsername = "username"
	secretPassword = "password"
)

const (
	encryptionNone        = "None"
	encryptionExplicitTLS = "Explicit TLS (FTPS)"
)

// checksumAlgo is the label stored in UploadResult/upload_log. FTP has no
// standardized, widely-supported command a client can rely on to ask the
// server for a file's content hash (the unofficial XCRC/XSHA1/XMD5/HASH
// commands are not implemented consistently across servers), so this
// provider computes its own SHA-256 while streaming the upload and verifies
// later by re-downloading and re-hashing - the same strategy
// internal/providers/webdav uses, and for the same reason.
const checksumAlgo = provider.ChecksumSHA256SelfComputed

func init() {
	registry.Register(Type, New)
	registry.RegisterSchema(Type, (&Provider{}).ConfigFields)
}

// Config is the non-secret part of an FTP connection, persisted in the JSON
// config file. Credentials live in the secret store.
type Config struct {
	ConnectionID string `json:"connectionId"`
	Host         string `json:"host"`
	Encryption   string `json:"encryption"`
	DisplayName  string `json:"displayName"`
}

// Provider implements provider.Provider (and several optional feature
// interfaces) over an FTP connection.
//
// A single *ftp.ServerConn "is not safe to be called concurrently" and
// supports only one in-flight data connection (see the client library's own
// doc comment) - internal/queue.Manager can nonetheless issue several
// uploads to the same connection back to back (or, if the operator raises
// "max concurrent uploads per connection" above its default of 1, even
// concurrently), so every method here serializes access through mu.
type Provider struct {
	cfg      Config
	addr     string
	username string
	password string
	opts     []ftp.DialOption

	mu   sync.Mutex
	conn *ftp.ServerConn
}

// New is the registry.Factory for the "ftp" provider type.
func New(rawCfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
	var cfg Config
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return nil, fmt.Errorf("ftp: invalid config: %w", err)
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("ftp: host is required")
	}

	username, err := secrets.Get(cfg.ConnectionID, secretUsername)
	if err != nil {
		return nil, fmt.Errorf("ftp: reading username secret: %w", err)
	}
	password, err := secrets.Get(cfg.ConnectionID, secretPassword)
	if err != nil {
		return nil, fmt.Errorf("ftp: reading password secret: %w", err)
	}

	addr := cfg.Host
	if !strings.Contains(addr, ":") {
		addr += ":21"
	}

	var opts []ftp.DialOption
	if cfg.Encryption == encryptionExplicitTLS {
		opts = append(opts, ftp.DialWithExplicitTLS(&tls.Config{}))
	}

	p := &Provider{cfg: cfg, addr: addr, username: username, password: password, opts: opts}
	if err := p.ensureConn(); err != nil {
		return nil, fmt.Errorf("ftp: %w", err)
	}
	return p, nil
}

func (p *Provider) Type() string { return Type }

func (p *Provider) DisplayName() string {
	if p.cfg.DisplayName != "" {
		return p.cfg.DisplayName
	}
	return "FTP (" + p.cfg.Host + ")"
}

func (p *Provider) TestConnection(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureConn(); err != nil {
		return fmt.Errorf("ftp: connection test failed: %w", err)
	}
	return nil
}

// Upload streams task.Reader to task.RemotePath, computing a SHA-256
// checksum while streaming so no second read of the source is needed.
func (p *Provider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	if err := p.ensureParentDir(task.RemotePath); err != nil {
		return provider.UploadResult{}, fmt.Errorf("ftp: upload %q: %w", task.RemotePath, err)
	}

	h := sha256.New()
	reader := io.TeeReader(&streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}, h)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureConn(); err != nil {
		return provider.UploadResult{}, fmt.Errorf("ftp: upload %q: %w", task.RemotePath, err)
	}
	if err := p.conn.Stor(task.RemotePath, reader); err != nil {
		return provider.UploadResult{}, fmt.Errorf("ftp: upload %q: %w", task.RemotePath, err)
	}

	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    p.remoteURL(task.RemotePath),
		ChecksumAlgo: checksumAlgo,
		Checksum:     hex.EncodeToString(h.Sum(nil)),
	}, nil
}

func (p *Provider) Download(ctx context.Context, task provider.DownloadTask) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureConn(); err != nil {
		return fmt.Errorf("ftp: download %q: %w", task.RemotePath, err)
	}

	// FileSize (a SIZE command on the control connection) must happen
	// before Retr opens its data connection - issuing another control
	// command while a transfer is in flight, before the final 226 reply,
	// is not something every FTP server tolerates.
	var total int64
	if task.Progress != nil {
		if size, err := p.conn.FileSize(task.RemotePath); err == nil {
			total = size
		}
	}

	resp, err := p.conn.Retr(task.RemotePath)
	if err != nil {
		return fmt.Errorf("ftp: download %q: %w", task.RemotePath, err)
	}
	defer resp.Close()

	writer := task.Writer
	if task.Progress != nil {
		writer = &streamio.ProgressWriter{W: writer, Total: total, OnProgress: task.Progress}
	}

	if _, err := io.Copy(writer, resp); err != nil {
		return fmt.Errorf("ftp: download %q: %w", task.RemotePath, err)
	}
	return nil
}

func (p *Provider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureConn(); err != nil {
		return nil, fmt.Errorf("ftp: list %q: %w", remotePath, err)
	}

	entries, err := p.conn.List(remotePath)
	if err != nil {
		return nil, fmt.Errorf("ftp: list %q: %w", remotePath, err)
	}
	result := make([]provider.RemoteEntry, 0, len(entries))
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		result = append(result, provider.RemoteEntry{
			Path:    path.Join(remotePath, e.Name),
			Name:    e.Name,
			Size:    int64(e.Size),
			IsDir:   e.Type == ftp.EntryTypeFolder,
			ModTime: e.Time,
		})
	}
	return result, nil
}

func (p *Provider) Delete(ctx context.Context, remotePath string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureConn(); err != nil {
		return fmt.Errorf("ftp: delete %q: %w", remotePath, err)
	}
	if err := p.conn.Delete(remotePath); err != nil {
		return fmt.Errorf("ftp: delete %q: %w", remotePath, err)
	}
	return nil
}

// Exists implements provider.ExistenceChecker.
func (p *Provider) Exists(ctx context.Context, remotePath string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureConn(); err != nil {
		return false, fmt.Errorf("ftp: stat %q: %w", remotePath, err)
	}

	_, err := p.conn.GetEntry(remotePath)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("ftp: stat %q: %w", remotePath, err)
	}
	return true, nil
}

// VerifyChecksum implements provider.ChecksumVerifier by re-downloading the
// object and recomputing the same self-computed SHA-256 Upload produced.
// checksumAlgo values produced by other providers are rejected, since they
// were not computed by this code path.
func (p *Provider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	if algo != checksumAlgo {
		return false, fmt.Errorf("ftp: cannot verify checksum of algo %q", algo)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureConn(); err != nil {
		return false, fmt.Errorf("ftp: verify %q: %w", remotePath, err)
	}

	resp, err := p.conn.Retr(remotePath)
	if err != nil {
		return false, fmt.Errorf("ftp: verify %q: %w", remotePath, err)
	}
	defer resp.Close()

	h := sha256.New()
	if _, err := io.Copy(h, resp); err != nil {
		return false, fmt.Errorf("ftp: verify %q: %w", remotePath, err)
	}

	return hex.EncodeToString(h.Sum(nil)) == checksum, nil
}

// ConfigFields implements provider.ConfigSchema.
func (p *Provider) ConfigFields() []provider.FieldSpec {
	return []provider.FieldSpec{
		{Key: "host", Label: "Host[:port] (default port 21)", Type: provider.FieldText, Required: true},
		{Key: "encryption", Label: "Encryption", Type: provider.FieldSelect, Required: true, Options: []string{encryptionNone, encryptionExplicitTLS}},
		{Key: secretUsername, Label: "Username", Type: provider.FieldPassword, Required: true},
		{Key: secretPassword, Label: "Password", Type: provider.FieldPassword, Required: true},
	}
}

func (p *Provider) remoteURL(remotePath string) string {
	return "ftp://" + p.cfg.Host + path.Clean("/"+remotePath)
}

// ensureConn (re)dials and logs in only when there is no live connection yet
// - a cheap NoOp probes the existing one first, since idle FTP servers
// commonly close the control connection after a timeout between uploads
// that the queue's own pacing/backoff can easily exceed.
func (p *Provider) ensureConn() error {
	if p.conn != nil {
		if err := p.conn.NoOp(); err == nil {
			return nil
		}
		_ = p.conn.Quit()
		p.conn = nil
	}

	conn, err := ftp.Dial(p.addr, p.opts...)
	if err != nil {
		return fmt.Errorf("dial %q: %w", p.addr, err)
	}
	if err := conn.Login(p.username, p.password); err != nil {
		_ = conn.Quit()
		return fmt.Errorf("login: %w", err)
	}
	p.conn = conn
	return nil
}

// ensureParentDir creates every intermediate FTP directory in remotePath's
// directory portion. Needed for "preserve folder structure" uploads, since
// STOR does not implicitly create missing parent directories. MakeDir errors
// are deliberately ignored here (most servers report an existing directory
// as a plain 550, indistinguishable at this layer from other causes without
// a further round trip) - a directory that genuinely couldn't be created
// surfaces anyway, as a clear error from the STOR that follows.
func (p *Provider) ensureParentDir(remotePath string) error {
	dir := path.Dir(remotePath)
	if dir == "." || dir == "/" || dir == "" {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureConn(); err != nil {
		return err
	}

	var built strings.Builder
	for seg := range strings.SplitSeq(strings.Trim(dir, "/"), "/") {
		if seg == "" {
			continue
		}
		built.WriteString("/")
		built.WriteString(seg)
		_ = p.conn.MakeDir(built.String())
	}
	return nil
}

func isNotFound(err error) bool {
	protoErr, ok := errors.AsType[*textproto.Error](err)
	return ok && protoErr.Code == ftp.StatusFileUnavailable
}
