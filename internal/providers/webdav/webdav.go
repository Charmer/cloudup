// Package webdav implements provider.Provider over the WebDAV protocol.
// It also covers Yandex.Disk, Nextcloud, ownCloud and box.com, which all
// expose a standard WebDAV endpoint - no dedicated SDK needed for them.
package webdav

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"

	"github.com/studio-b12/gowebdav"

	"cloudup/internal/debuglog"
	"cloudup/internal/provider"
	"cloudup/internal/registry"
	"cloudup/internal/streamio"
)

const Type = "webdav"

const (
	secretUsername = "username"
	secretPassword = "password"
)

// checksumAlgo is the label stored in UploadResult/upload_log. WebDAV
// servers do not reliably expose a content hash, so verification is done
// by this package itself (re-download + rehash) rather than trusting a
// server-provided value - see VerifyChecksum.
const checksumAlgo = provider.ChecksumSHA256SelfComputed

func init() {
	registry.Register(Type, New)
	registry.RegisterSchema(Type, (&Provider{}).ConfigFields)
}

// Config is the non-secret part of a WebDAV connection, persisted in the
// JSON config file. Credentials live in the secret store.
type Config struct {
	ConnectionID string `json:"connectionId"`
	URL          string `json:"url"`
	DisplayName  string `json:"displayName"`
}

// Provider implements provider.Provider (and several optional feature
// interfaces) over a WebDAV endpoint.
type Provider struct {
	cfg    Config
	client *gowebdav.Client
}

// New is the registry.Factory for the "webdav" provider type.
func New(rawCfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
	var cfg Config
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return nil, fmt.Errorf("webdav: invalid config: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("webdav: url is required")
	}
	if _, err := url.Parse(cfg.URL); err != nil {
		return nil, fmt.Errorf("webdav: invalid url: %w", err)
	}

	username, err := secrets.Get(cfg.ConnectionID, secretUsername)
	if err != nil {
		return nil, fmt.Errorf("webdav: reading username secret: %w", err)
	}
	password, err := secrets.Get(cfg.ConnectionID, secretPassword)
	if err != nil {
		return nil, fmt.Errorf("webdav: reading password secret: %w", err)
	}

	client := gowebdav.NewAuthClient(cfg.URL, newPreemptiveBasicAuth(username, password))
	// debuglog.Transport is a no-op unless CLOUDUP_DEBUG is set, so this is
	// safe to wire in unconditionally rather than branching on
	// debuglog.Enabled() here too.
	client.SetTransport(debuglog.Transport{})
	return &Provider{cfg: cfg, client: client}, nil
}

func (p *Provider) Type() string { return Type }

func (p *Provider) DisplayName() string {
	if p.cfg.DisplayName != "" {
		return p.cfg.DisplayName
	}
	return "WebDAV (" + p.cfg.URL + ")"
}

func (p *Provider) TestConnection(ctx context.Context) error {
	if _, err := p.client.ReadDir("/"); err != nil {
		return fmt.Errorf("webdav: connection test failed: %w", err)
	}
	return nil
}

// Upload streams task.Reader to task.RemotePath, computing a SHA-256
// checksum while streaming so no second read of the source is needed.
func (p *Provider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	if err := p.ensureParentDir(task.RemotePath); err != nil {
		return provider.UploadResult{}, fmt.Errorf("webdav: upload %q: %w", task.RemotePath, err)
	}

	h := sha256.New()
	reader := io.TeeReader(&streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}, h)

	if err := p.client.WriteStream(task.RemotePath, reader, 0o644); err != nil {
		return provider.UploadResult{}, fmt.Errorf("webdav: upload %q: %w", task.RemotePath, err)
	}

	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    p.remoteURL(task.RemotePath),
		ChecksumAlgo: checksumAlgo,
		Checksum:     hex.EncodeToString(h.Sum(nil)),
	}, nil
}

func (p *Provider) Download(ctx context.Context, task provider.DownloadTask) error {
	reader, err := p.client.ReadStream(task.RemotePath)
	if err != nil {
		return fmt.Errorf("webdav: download %q: %w", task.RemotePath, err)
	}
	defer reader.Close()

	writer := task.Writer
	if task.Progress != nil {
		info, err := p.client.Stat(task.RemotePath)
		total := int64(0)
		if err == nil {
			total = info.Size()
		}
		writer = &streamio.ProgressWriter{W: writer, Total: total, OnProgress: task.Progress}
	}

	if _, err := io.Copy(writer, reader); err != nil {
		return fmt.Errorf("webdav: download %q: %w", task.RemotePath, err)
	}
	return nil
}

func (p *Provider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	infos, err := p.client.ReadDir(remotePath)
	if err != nil {
		return nil, fmt.Errorf("webdav: list %q: %w", remotePath, err)
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
	if err := p.client.Remove(remotePath); err != nil {
		return fmt.Errorf("webdav: delete %q: %w", remotePath, err)
	}
	return nil
}

// Exists implements provider.ExistenceChecker.
func (p *Provider) Exists(ctx context.Context, remotePath string) (bool, error) {
	_, err := p.client.Stat(remotePath)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("webdav: stat %q: %w", remotePath, err)
	}
	return true, nil
}

// VerifyChecksum implements provider.ChecksumVerifier. WebDAV servers do
// not reliably expose a content hash (ETag semantics vary by server), so
// this provider verifies by re-downloading the object and recomputing the
// same self-computed SHA-256 it produced during Upload. checksumAlgo values
// produced by other providers are rejected, since they were not computed by
// this code path.
func (p *Provider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	if algo != checksumAlgo {
		return false, fmt.Errorf("webdav: cannot verify checksum of algo %q", algo)
	}

	reader, err := p.client.ReadStream(remotePath)
	if err != nil {
		return false, fmt.Errorf("webdav: verify %q: %w", remotePath, err)
	}
	defer reader.Close()

	h := sha256.New()
	if _, err := io.Copy(h, reader); err != nil {
		return false, fmt.Errorf("webdav: verify %q: %w", remotePath, err)
	}

	return hex.EncodeToString(h.Sum(nil)) == checksum, nil
}

// ConfigFields implements provider.ConfigSchema. Username is routed to the
// secret store (FieldPassword) rather than the JSON config, matching how
// New reads it via secrets.Get(cfg.ConnectionID, secretUsername) below -
// same treatment S3 gives its AccessKeyID.
func (p *Provider) ConfigFields() []provider.FieldSpec {
	return []provider.FieldSpec{
		{Key: "url", Label: "Server URL", Type: provider.FieldText, Required: true},
		{Key: secretUsername, Label: "Username", Type: provider.FieldPassword, Required: true},
		{Key: secretPassword, Label: "Password", Type: provider.FieldPassword, Required: true},
	}
}

func (p *Provider) remoteURL(remotePath string) string {
	base, err := url.Parse(p.cfg.URL)
	if err != nil {
		return ""
	}
	base.Path = path.Join(base.Path, remotePath)
	return base.String()
}

// ensureParentDir creates every intermediate WebDAV collection in
// remotePath's directory portion (everything but the final segment, which
// Upload is about to create as a file). Needed for "preserve folder
// structure" uploads: unlike Dropbox's /files/upload, WebDAV's PUT does not
// implicitly create missing parent collections - most servers (and
// golang.org/x/net/webdav's handler, used by this package's own tests)
// reject it with 409 Conflict instead.
func (p *Provider) ensureParentDir(remotePath string) error {
	dir := path.Dir(remotePath)
	if dir == "." || dir == "/" || dir == "" {
		return nil
	}
	if err := p.client.MkdirAll(dir, 0o755); err != nil {
		// gowebdav's MkdirAll walks segment-by-segment and tolerates a 409
		// (parent missing) along the way, but the final MKCOL on a
		// collection that already exists gets a different status (405 per
		// RFC 4918) and MkdirAll reports that as an error even though
		// nothing is wrong. Confirm via Stat before treating this as
		// fatal, so re-uploading a second file into the same folder never
		// fails just because the folder is already there.
		if info, statErr := p.client.Stat(dir); statErr == nil && info.IsDir() {
			return nil
		}
		return fmt.Errorf("creating %q: %w", dir, err)
	}
	return nil
}

func isNotFound(err error) bool {
	return gowebdav.IsErrNotFound(err)
}
