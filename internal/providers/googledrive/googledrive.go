// Package googledrive implements provider.Provider over the Google Drive
// API v3.
//
// Unlike WebDAV/S3, a Google Drive connection cannot be constructed from
// config + secrets alone the first time: it needs an interactive OAuth2
// consent step (open a browser, the user approves access, Google redirects
// back with an authorization code) before any refresh token exists. That
// step is exposed as the package-level Authorize function, called once by
// the connection-setup API flow - it is deliberately not part of the
// provider.Provider/ConfigSchema machinery, since ConfigSchema describes
// static form fields and has no notion of a multi-step interactive flow.
// Once Authorize succeeds, its refresh token is stored under
// secretRefreshToken by the caller, and New (the registry.Factory, used
// for every subsequent construction) can build a working client from it
// like any other provider.
//
// The OAuth Client ID/Secret are a property of the *app* registered with
// Google (one "Desktop app" OAuth client covers every Google Drive
// connection cloudup ever makes), not of an individual connection - unlike
// early versions of this package, ConfigFields no longer asks for them per
// connection. They are configured once (frontend: Settings tab, i.e.
// PUT /api/v1/provider-types/googledrive/oauth-credentials) and stored under
// AppCredentialsConnectionID, shared by every Google Drive connection's
// Authorize/New call.
package googledrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"golang.org/x/oauth2"
	xoauth2google "golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"cloudup/internal/debuglog"
	"cloudup/internal/provider"
	"cloudup/internal/registry"
	"cloudup/internal/streamio"
)

const Type = "googledrive"

const (
	secretClientID     = "clientId"
	secretClientSecret = "clientSecret"
	// secretRefreshToken is intentionally absent from ConfigFields - see
	// the package doc comment. It is written directly by the caller that
	// invoked Authorize, using this key.
	secretRefreshToken = "refreshToken"
)

// AppCredentialsConnectionID is the fixed pseudo-connection-ID under which
// the OAuth Client ID/Secret are stored in the secret store - see the
// package doc comment's "one Client ID for the whole app" note. It is not
// a real config.Connection ID; it never appears in config.json, only as a
// secrets.Store key namespace, exactly like a real connection ID would be.
const AppCredentialsConnectionID = "googledrive-app-credentials"

// ClientIDKey, ClientSecretKey and RefreshTokenKey are the secret-store
// keys this provider reads/writes, exported so callers outside the
// package (internal/httpapi) can read or write them without
// duplicating magic strings. ClientIDKey/ClientSecretKey are stored once under
// AppCredentialsConnectionID (shared by every Google Drive connection);
// RefreshTokenKey is per-connection, written by the caller that invoked
// Authorize for that specific connection.
const (
	ClientIDKey     = secretClientID
	ClientSecretKey = secretClientSecret
	RefreshTokenKey = secretRefreshToken
)

// checksumAlgo is the label stored in UploadResult/upload_log. Unlike
// WebDAV/S3, Google Drive reliably reports an MD5 checksum for every
// binary file's content (Files.get's md5Checksum field), so - unlike
// those two providers - VerifyChecksum here is a cheap metadata read
// rather than a full re-download and rehash.
const checksumAlgo = "md5"

const driveFolderMimeType = "application/vnd.google-apps.folder"

var errNotFound = errors.New("googledrive: not found")

func init() {
	registry.Register(Type, New)
	registry.RegisterSchema(Type, (&Provider{}).ConfigFields)
	// Unlike webdav/s3/b2, this type also needs an interactive consent step
	// before New can ever succeed - registering it here is what lets the
	// REST API offer "authorize this connection" generically, without
	// importing this package (see auth.go).
	registry.RegisterOAuth(Type, oauthFlow())
}

// rawConfig is the non-secret part of a Google Drive connection, persisted
// in the JSON config file. The OAuth Client ID/Secret (app-wide, not
// per-connection - see the package doc comment) and this connection's own
// refresh token live in the secret store.
type rawConfig struct {
	ConnectionID string `json:"connectionId"`
	FolderID     string `json:"folderId"` // optional; empty means "My Drive" root
	DisplayName  string `json:"displayName"`
}

// Provider implements provider.Provider (and several optional feature
// interfaces) over the Google Drive API.
type Provider struct {
	cfg rawConfig
	srv *drive.Service
}

// New is the registry.Factory for the "googledrive" provider type. It
// requires that the app-wide OAuth Client ID/Secret are configured (see
// AppCredentialsConnectionID) and that Authorize has already been run for
// this specific connection, with its refresh token stored under
// secretRefreshToken.
func New(rawCfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
	var cfg rawConfig
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return nil, fmt.Errorf("googledrive: invalid config: %w", err)
	}

	clientID, err := secrets.Get(AppCredentialsConnectionID, secretClientID)
	if err != nil {
		return nil, fmt.Errorf("googledrive: reading client ID: %w", err)
	}
	clientSecret, err := secrets.Get(AppCredentialsConnectionID, secretClientSecret)
	if err != nil {
		return nil, fmt.Errorf("googledrive: reading client secret: %w", err)
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("googledrive: OAuth Client ID/Secret not configured - set them in Settings first")
	}
	refreshToken, err := secrets.Get(cfg.ConnectionID, secretRefreshToken)
	if err != nil {
		return nil, fmt.Errorf("googledrive: reading refresh token: %w", err)
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("googledrive: connection not authorized yet - run Authorize first")
	}

	oauthCfg := oauthConfig(clientID, clientSecret)

	srv, err := drive.NewService(context.Background(), option.WithHTTPClient(authedHTTPClient(oauthCfg, refreshToken)))
	if err != nil {
		return nil, fmt.Errorf("googledrive: creating Drive client: %w", err)
	}

	return &Provider{cfg: cfg, srv: srv}, nil
}

// authedHTTPClient builds the token-source-backed client the Drive service
// transports over.
//
// It exists as its own function for two reasons. First, the Drive client is
// built with option.WithHTTPClient rather than option.WithTokenSource,
// because WithHTTPClient is the only hook google-api-go-client offers for
// supplying a base transport - and without it CLOUDUP_DEBUG would silently
// cover every provider except this one. Auth is unaffected: the client
// handed over is already token-source-backed, exactly what WithTokenSource
// would have constructed internally. Second, having it separate means the
// debuglog wiring is assertable in a test (see googledrive_test.go) instead
// of being invisible inside a *drive.Service.
//
// oauth2.HTTPClient in the context is what puts debuglog underneath the
// token-refresh requests too, not only the API calls. debuglog.Transport is
// a transparent passthrough unless CLOUDUP_DEBUG is set.
func authedHTTPClient(oauthCfg *oauth2.Config, refreshToken string) *http.Client {
	baseCtx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: debuglog.Transport{}})
	tokenSource := oauthCfg.TokenSource(baseCtx, &oauth2.Token{RefreshToken: refreshToken})
	return oauth2.NewClient(baseCtx, tokenSource)
}

func oauthConfig(clientID, clientSecret string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       []string{drive.DriveFileScope},
		Endpoint:     oauthEndpoint,
	}
}

// oauthEndpoint is a package variable (rather than a direct reference to
// xoauth2google.Endpoint) purely so tests can redirect the OAuth token
// exchange to a local fake server without touching Google.
var oauthEndpoint = xoauth2google.Endpoint

func (p *Provider) Type() string { return Type }

func (p *Provider) DisplayName() string {
	if p.cfg.DisplayName != "" {
		return p.cfg.DisplayName
	}
	return "Google Drive"
}

func (p *Provider) TestConnection(ctx context.Context) error {
	if _, err := p.srv.About.Get().Fields("user").Context(ctx).Do(); err != nil {
		return fmt.Errorf("googledrive: connection test failed: %w", err)
	}
	return nil
}

// Upload creates task.RemotePath under the connection's root folder,
// creating any missing intermediate folders. Progress is reported as
// task.Reader is read; the checksum comes from Drive's own md5Checksum
// rather than being computed locally - see the package doc comment.
func (p *Provider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	dirs, name := splitRemotePath(task.RemotePath)
	if name == "" {
		return provider.UploadResult{}, fmt.Errorf("googledrive: remote path %q has no file name", task.RemotePath)
	}
	parentID, err := p.resolveFolder(ctx, dirs, true)
	if err != nil {
		return provider.UploadResult{}, err
	}

	reader := &streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}
	created, err := p.srv.Files.Create(&drive.File{Name: name, Parents: []string{parentID}}).
		Media(reader).
		Fields("id", "webViewLink", "md5Checksum").
		Context(ctx).
		Do()
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("googledrive: upload %q: %w", task.RemotePath, err)
	}

	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    created.WebViewLink,
		ChecksumAlgo: checksumAlgo,
		Checksum:     created.Md5Checksum,
	}, nil
}

func (p *Provider) Download(ctx context.Context, task provider.DownloadTask) error {
	file, err := p.resolveFile(ctx, task.RemotePath)
	if err != nil {
		return fmt.Errorf("googledrive: download %q: %w", task.RemotePath, err)
	}

	resp, err := p.srv.Files.Get(file.Id).Context(ctx).Download()
	if err != nil {
		return fmt.Errorf("googledrive: download %q: %w", task.RemotePath, err)
	}
	defer resp.Body.Close()

	writer := task.Writer
	if task.Progress != nil {
		writer = &streamio.ProgressWriter{W: writer, Total: file.Size, OnProgress: task.Progress}
	}

	if _, err := copyBody(writer, resp); err != nil {
		return fmt.Errorf("googledrive: download %q: %w", task.RemotePath, err)
	}
	return nil
}

func (p *Provider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	dirs, name := splitRemotePath(remotePath)
	if name != "" {
		dirs = append(dirs, name)
	}
	parentID, err := p.resolveFolder(ctx, dirs, false)
	if err != nil {
		return nil, fmt.Errorf("googledrive: list %q: %w", remotePath, err)
	}

	list, err := p.srv.Files.List().
		Q(fmt.Sprintf("%s in parents and trashed = false", quoteQueryValue(parentID))).
		Fields("files(id,name,mimeType,size,modifiedTime)").
		Context(ctx).
		Do()
	if err != nil {
		return nil, fmt.Errorf("googledrive: list %q: %w", remotePath, err)
	}

	entries := make([]provider.RemoteEntry, 0, len(list.Files))
	for _, f := range list.Files {
		modTime, _ := parseRFC3339(f.ModifiedTime)
		entries = append(entries, provider.RemoteEntry{
			Path:    joinRemotePath(remotePath, f.Name),
			Name:    f.Name,
			Size:    f.Size,
			IsDir:   f.MimeType == driveFolderMimeType,
			ModTime: modTime,
		})
	}
	return entries, nil
}

func (p *Provider) Delete(ctx context.Context, remotePath string) error {
	file, err := p.resolveFile(ctx, remotePath)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil
		}
		return fmt.Errorf("googledrive: delete %q: %w", remotePath, err)
	}
	if err := p.srv.Files.Delete(file.Id).Context(ctx).Do(); err != nil {
		return fmt.Errorf("googledrive: delete %q: %w", remotePath, err)
	}
	return nil
}

// Exists implements provider.ExistenceChecker.
func (p *Provider) Exists(ctx context.Context, remotePath string) (bool, error) {
	_, err := p.resolveFile(ctx, remotePath)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("googledrive: stat %q: %w", remotePath, err)
	}
	return true, nil
}

// VerifyChecksum implements provider.ChecksumVerifier by re-reading the
// object's md5Checksum metadata field - no re-download needed, since Drive
// already stores and serves this value reliably (see the package doc
// comment).
func (p *Provider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	if algo != checksumAlgo {
		return false, fmt.Errorf("googledrive: cannot verify checksum of algo %q", algo)
	}

	file, err := p.resolveFile(ctx, remotePath)
	if err != nil {
		return false, fmt.Errorf("googledrive: verify %q: %w", remotePath, err)
	}
	return file.Md5Checksum == checksum, nil
}

// ConfigFields implements provider.ConfigSchema. Deliberately excludes the
// OAuth Client ID/Secret (app-wide, configured once - see the package doc
// comment) and the refresh token (per-connection, but written directly by
// the caller that invoked Authorize, not typed into a form).
func (p *Provider) ConfigFields() []provider.FieldSpec {
	return []provider.FieldSpec{
		{Key: "folderId", Label: "Root folder ID (optional, defaults to My Drive)", Type: provider.FieldText},
	}
}

// resolveFolder walks segments (each a folder name) starting at the
// connection's root folder, returning the final folder's ID. If create is
// true, missing folders along the way are created; otherwise a missing
// folder results in errNotFound.
func (p *Provider) resolveFolder(ctx context.Context, segments []string, create bool) (string, error) {
	parentID := p.cfg.FolderID
	if parentID == "" {
		parentID = "root"
	}
	for _, seg := range segments {
		id, err := p.findChild(ctx, parentID, seg, driveFolderMimeType)
		if err != nil {
			return "", err
		}
		if id == "" {
			if !create {
				return "", errNotFound
			}
			id, err = p.createFolder(ctx, parentID, seg)
			if err != nil {
				return "", err
			}
		}
		parentID = id
	}
	return parentID, nil
}

func (p *Provider) createFolder(ctx context.Context, parentID, name string) (string, error) {
	created, err := p.srv.Files.Create(&drive.File{
		Name:     name,
		MimeType: driveFolderMimeType,
		Parents:  []string{parentID},
	}).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("googledrive: creating folder %q: %w", name, err)
	}
	return created.Id, nil
}

// findChild returns the ID of the child of parentID named name, restricted
// to mimeType if non-empty (used to disambiguate folders from files of the
// same name), or "" if no such child exists.
func (p *Provider) findChild(ctx context.Context, parentID, name, mimeType string) (string, error) {
	q := fmt.Sprintf("%s in parents and name = %s and trashed = false", quoteQueryValue(parentID), quoteQueryValue(name))
	if mimeType != "" {
		q += fmt.Sprintf(" and mimeType = %s", quoteQueryValue(mimeType))
	}
	list, err := p.srv.Files.List().Q(q).Fields("files(id)").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("googledrive: querying %q: %w", name, err)
	}
	if len(list.Files) == 0 {
		return "", nil
	}
	return list.Files[0].Id, nil
}

// resolveFile finds the file at remotePath (its final segment is the file
// name, preceding segments are folder names) and fetches its metadata.
func (p *Provider) resolveFile(ctx context.Context, remotePath string) (*drive.File, error) {
	dirs, name := splitRemotePath(remotePath)
	if name == "" {
		return nil, errNotFound
	}
	parentID, err := p.resolveFolder(ctx, dirs, false)
	if err != nil {
		return nil, err
	}
	id, err := p.findChild(ctx, parentID, name, "")
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errNotFound
	}
	return p.srv.Files.Get(id).Fields("id", "name", "mimeType", "size", "modifiedTime", "webViewLink", "md5Checksum").Context(ctx).Do()
}

func splitRemotePath(remotePath string) (dirs []string, name string) {
	trimmed := strings.Trim(remotePath, "/")
	if trimmed == "" {
		return nil, ""
	}
	parts := strings.Split(trimmed, "/")
	return parts[:len(parts)-1], parts[len(parts)-1]
}

func joinRemotePath(dir, name string) string {
	return path.Join(strings.Trim(dir, "/"), name)
}

// quoteQueryValue formats a value for Drive's query language (single
// quotes, with any embedded single quote escaped).
func quoteQueryValue(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
}

func parseRFC3339(s string) (t time.Time, err error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

func copyBody(dst io.Writer, resp *http.Response) (int64, error) {
	return io.Copy(dst, resp.Body)
}
