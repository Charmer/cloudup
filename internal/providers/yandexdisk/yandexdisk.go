// Package yandexdisk implements provider.Provider over the native
// Yandex.Disk REST API v1 (https://cloud-api.yandex.net/v1/disk), not the
// generic WebDAV endpoint Yandex.Disk also exposes (which
// internal/providers/webdav already covers). The point of a dedicated
// native package is the same reason internal/providers/googledrive and
// internal/providers/b2 exist instead of routing through a generic
// protocol: the native API reports a reliable server-computed MD5 for
// every file (the Resource object's "md5" field), so VerifyChecksum here
// is a cheap metadata read instead of the re-download-and-rehash approach
// webdav/s3/dropbox are forced into.
//
// Every behavior this package relies on is taken from Yandex's official
// REST API reference (https://yandex.com/dev/disk-api/doc/en/reference/)
// or, where the reference prose was silent on a specific detail, corroborated
// independently by production tooling (rclone's Yandex backend) rather than
// assumed - see the two deliberate details called out below. No
// undocumented or third-party-only field is used: in particular, some
// community SDKs list an additional "sha256" field on the Resource object
// that the official reference never documents, so this package uses only
// "md5" as its checksum algorithm.
//
//  1. Authorization header scheme: Yandex's OAuth token endpoint reports
//     "token_type": "bearer" like almost every other OAuth2 API, which would
//     make golang.org/x/oauth2's standard Transport send
//     "Authorization: Bearer <token>" - but the Disk API's own reference
//     documents (and rclone's Yandex backend independently works around the
//     same mismatch by force-overwriting the stored token's TokenType) that
//     it actually requires the literal scheme name "OAuth". oauthTransport
//     below sends that scheme unconditionally rather than trusting the
//     token's reported type.
//  2. Default page size: GET /resources defaults to 20 items per folder
//     listing if "limit" is not given explicitly. List always passes an
//     explicit "limit" and paginates via "offset" until it has seen every
//     item the response's "_embedded.total" reports - otherwise a folder
//     with more than 20 entries would silently appear truncated.
//
// Like dropbox and googledrive, a Yandex.Disk connection cannot be
// constructed from config + secrets alone the first time: it needs an
// interactive OAuth2 consent step before any refresh token exists. See the
// package doc comment on internal/providers/dropbox for the general shape
// this package follows (Authorize as a separate one-time step, app-wide
// OAuth Client ID/Secret under AppCredentialsConnectionID, per-connection
// refresh token).
package yandexdisk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"cloudup/internal/debuglog"
	"cloudup/internal/provider"
	"cloudup/internal/registry"
	"cloudup/internal/streamio"
)

const Type = "yandexdisk"

const (
	secretClientID     = "clientId"
	secretClientSecret = "clientSecret"
	// secretRefreshToken is intentionally absent from ConfigFields - see the
	// package doc comment. It is written directly by the caller that
	// invoked Authorize, using this key.
	secretRefreshToken = "refreshToken"
)

// AppCredentialsConnectionID is the fixed pseudo-connection-ID under which
// the OAuth Client ID/Secret are stored in the secret store - one Yandex
// OAuth app covers every Yandex.Disk connection cloudup ever makes, exactly
// like dropbox.AppCredentialsConnectionID/googledrive's equivalent.
const AppCredentialsConnectionID = "yandexdisk-app-credentials"

// ClientIDKey, ClientSecretKey and RefreshTokenKey are the secret-store keys
// this provider reads/writes, exported so callers outside the package
// (internal/httpapi) can read or write them without duplicating magic
// strings.
const (
	ClientIDKey     = secretClientID
	ClientSecretKey = secretClientSecret
	RefreshTokenKey = secretRefreshToken
)

// checksumAlgo is the label stored in UploadResult/upload_log. Only "md5" -
// see the package doc comment on why no other field is trusted.
const checksumAlgo = "md5"

// apiBaseURL is a package variable (rather than a literal used directly)
// purely so tests can redirect it to a local httptest.Server, the same
// pattern b2 uses for authorizeURL.
var apiBaseURL = "https://cloud-api.yandex.net/v1/disk"

// oauthEndpoint is a package variable for the same reason - tests redirect
// the OAuth token exchange to a local fake server without touching Yandex.
var oauthEndpoint = oauth2.Endpoint{
	AuthURL:  "https://oauth.yandex.ru/authorize",
	TokenURL: "https://oauth.yandex.ru/token",
}

// oauthScopes requests full read/write access to the user's whole Disk
// (cloud_api:disk.write, cloud_api:disk.read) plus disk.info for
// TestConnection - not the app-folder-restricted scope, since RootPath is a
// user-chosen arbitrary path, like rootPath on dropbox, not a fixed
// sandboxed folder.
var oauthScopes = []string{
	"cloud_api:disk.write",
	"cloud_api:disk.read",
	"cloud_api:disk.info",
}

func init() {
	registry.Register(Type, New)
	registry.RegisterSchema(Type, (&Provider{}).ConfigFields)
	registry.RegisterOAuth(Type, oauthFlow())
}

// rawConfig is the non-secret part of a Yandex.Disk connection, persisted in
// the JSON config file. The OAuth Client ID/Secret (app-wide) and this
// connection's own refresh token live in the secret store.
type rawConfig struct {
	ConnectionID string `json:"connectionId"`
	RootPath     string `json:"rootPath"` // optional; empty means the Disk root
	DisplayName  string `json:"displayName"`
}

// Provider implements provider.Provider (and several optional feature
// interfaces) over the Yandex.Disk REST API.
type Provider struct {
	cfg rawConfig

	// httpClient authenticates every request via oauthTransport - see its
	// doc comment for why this is not a plain oauth2.NewClient client.
	httpClient *http.Client
}

// New is the registry.Factory for the "yandexdisk" provider type. It
// requires that the app-wide OAuth Client ID/Secret are configured (see
// AppCredentialsConnectionID) and that Authorize has already been run for
// this specific connection, with its refresh token stored under
// secretRefreshToken.
func New(rawCfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
	var cfg rawConfig
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return nil, fmt.Errorf("yandexdisk: invalid config: %w", err)
	}

	clientID, err := secrets.Get(AppCredentialsConnectionID, secretClientID)
	if err != nil {
		return nil, fmt.Errorf("yandexdisk: reading client ID: %w", err)
	}
	clientSecret, err := secrets.Get(AppCredentialsConnectionID, secretClientSecret)
	if err != nil {
		return nil, fmt.Errorf("yandexdisk: reading client secret: %w", err)
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("yandexdisk: OAuth Client ID/Secret not configured - set them in Settings first")
	}
	refreshToken, err := secrets.Get(cfg.ConnectionID, secretRefreshToken)
	if err != nil {
		return nil, fmt.Errorf("yandexdisk: reading refresh token: %w", err)
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("yandexdisk: connection not authorized yet - run Authorize first")
	}

	oauthCfg := oauthConfig(clientID, clientSecret)
	// oauth2.HTTPClient in the context is what puts debuglog underneath the
	// token-refresh requests; oauthTransport.Base does the same for the
	// actual Disk API calls - see its doc comment for why this provider
	// cannot just use oauth2.NewClient like googledrive/dropbox do.
	baseCtx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: debuglog.Transport{}})
	tokenSource := oauthCfg.TokenSource(baseCtx, &oauth2.Token{RefreshToken: refreshToken})
	httpClient := &http.Client{Transport: &oauthTransport{Source: tokenSource, Base: debuglog.Transport{}}}

	return &Provider{cfg: cfg, httpClient: httpClient}, nil
}

// oauthTransport sends every request with "Authorization: OAuth <token>" -
// see the package doc comment's point 1 for why this cannot be
// oauth2.NewClient's standard Transport.
type oauthTransport struct {
	Source oauth2.TokenSource
	Base   http.RoundTripper
}

func (t *oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.Source.Token()
	if err != nil {
		return nil, fmt.Errorf("yandexdisk: refreshing oauth token: %w", err)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "OAuth "+token.AccessToken)
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func oauthConfig(clientID, clientSecret string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       oauthScopes,
		Endpoint:     oauthEndpoint,
	}
}

func (p *Provider) Type() string { return Type }

func (p *Provider) DisplayName() string {
	if p.cfg.DisplayName != "" {
		return p.cfg.DisplayName
	}
	return "Yandex.Disk"
}

// ConfigFields implements provider.ConfigSchema. Deliberately excludes the
// OAuth Client ID/Secret (app-wide) and the refresh token (per-connection,
// but written directly by the caller that invoked Authorize) - same
// reasoning as dropbox/googledrive's ConfigFields.
func (p *Provider) ConfigFields() []provider.FieldSpec {
	return []provider.FieldSpec{
		{Key: "rootPath", Label: "Subfolder on your Disk to upload into, optional (e.g. backups/cloudup) - leave empty for the Disk root", Type: provider.FieldText},
	}
}

// TestConnection calls GET /v1/disk/ (documented as the general Disk info
// endpoint - total/used space), the same lightweight "is this token valid at
// all" probe TestConnection performs for every other provider.
func (p *Provider) TestConnection(ctx context.Context) error {
	if err := p.apiCall(ctx, http.MethodGet, "/", nil, nil); err != nil {
		return fmt.Errorf("yandexdisk: connection test failed: %w", err)
	}
	return nil
}

// resourceMeta mirrors the fields of the Resource object
// (https://yandex.com/dev/disk-api/doc/en/reference/response-objects) this
// package actually uses - not every field the API can return.
type resourceMeta struct {
	Name      string        `json:"name"`
	Path      string        `json:"path"`
	Type      string        `json:"type"` // "file" or "dir"
	MimeType  string        `json:"mime_type"`
	Size      int64         `json:"size"`
	MD5       string        `json:"md5"`
	Modified  string        `json:"modified"`
	PublicURL string        `json:"public_url"`
	Embedded  *resourceList `json:"_embedded,omitempty"`
}

// resourceList mirrors the ResourceList object embedded in a folder's
// metadata response.
type resourceList struct {
	Items  []resourceMeta `json:"items"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
	Total  int            `json:"total"`
}

// listPageSize is how many entries List asks for per page. The API's own
// default (20) is too small to rely on implicitly - see the package doc
// comment's point 2 - so this is passed explicitly on every request.
const listPageSize = 200

// Upload streams task.Reader to task.RemotePath (prefixed with this
// connection's RootPath, if any), creating any missing intermediate
// folders first.
//
// Unlike webdav/s3/dropbox, this does not hash the stream itself: the PUT
// to the upload href returns no body at all (documented: "201 Created,
// Content-Length: 0"), so the freshly stored object's server-computed md5
// is read back with one cheap metadata GET afterward - the same
// "trust the server's own hash" approach googledrive/b2 use, just requiring
// an explicit follow-up call here because Yandex's upload response has
// nothing to report it inline.
func (p *Provider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	fullPath := joinYandexPath(p.cfg.RootPath, task.RemotePath)
	if fullPath == "/" {
		return provider.UploadResult{}, fmt.Errorf("yandexdisk: remote path %q has no file name", task.RemotePath)
	}

	if err := p.ensureParentFolders(ctx, fullPath); err != nil {
		return provider.UploadResult{}, fmt.Errorf("yandexdisk: upload %q: %w", task.RemotePath, err)
	}

	var uploadHref struct {
		Href string `json:"href"`
	}
	q := url.Values{"path": {fullPath}, "overwrite": {"true"}}
	if err := p.apiCall(ctx, http.MethodGet, "/resources/upload", q, &uploadHref); err != nil {
		return provider.UploadResult{}, fmt.Errorf("yandexdisk: upload %q: requesting upload url: %w", task.RemotePath, err)
	}
	if uploadHref.Href == "" {
		return provider.UploadResult{}, fmt.Errorf("yandexdisk: upload %q: server returned no upload href", task.RemotePath)
	}

	reader := &streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}
	if err := p.putBytes(ctx, uploadHref.Href, reader); err != nil {
		return provider.UploadResult{}, fmt.Errorf("yandexdisk: upload %q: %w", task.RemotePath, err)
	}

	meta, err := p.statResource(ctx, fullPath)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("yandexdisk: upload %q: reading back metadata: %w", task.RemotePath, err)
	}

	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    meta.PublicURL,
		ChecksumAlgo: checksumAlgo,
		Checksum:     meta.MD5,
	}, nil
}

// putBytes PUTs body to an already-issued upload href. No Authorization
// header is required by Yandex for this URL (it is self-contained/signed),
// but oauthTransport attaches one anyway - harmless, since the href is a
// different host entirely and simply ignores it.
func (p *Provider) putBytes(ctx context.Context, href string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, href, body)
	if err != nil {
		return fmt.Errorf("building upload request: %w", err)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("uploading: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return newAPIErr(resp)
	}
	return nil
}

// Download fetches a temporary download href for task.RemotePath and reads
// the file from it.
func (p *Provider) Download(ctx context.Context, task provider.DownloadTask) error {
	fullPath := joinYandexPath(p.cfg.RootPath, task.RemotePath)

	var downloadHref struct {
		Href string `json:"href"`
	}
	q := url.Values{"path": {fullPath}}
	if err := p.apiCall(ctx, http.MethodGet, "/resources/download", q, &downloadHref); err != nil {
		return fmt.Errorf("yandexdisk: download %q: %w", task.RemotePath, err)
	}
	if downloadHref.Href == "" {
		return fmt.Errorf("yandexdisk: download %q: server returned no download href", task.RemotePath)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadHref.Href, nil)
	if err != nil {
		return fmt.Errorf("yandexdisk: download %q: %w", task.RemotePath, err)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("yandexdisk: download %q: %w", task.RemotePath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("yandexdisk: download %q: %w", task.RemotePath, newAPIErr(resp))
	}

	writer := task.Writer
	if task.Progress != nil {
		writer = &streamio.ProgressWriter{W: writer, Total: resp.ContentLength, OnProgress: task.Progress}
	}
	if _, err := io.Copy(writer, resp.Body); err != nil {
		return fmt.Errorf("yandexdisk: download %q: %w", task.RemotePath, err)
	}
	return nil
}

// List enumerates the immediate children of remotePath, paginating
// explicitly - see the package doc comment's point 2.
func (p *Provider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	fullPath := joinYandexPath(p.cfg.RootPath, remotePath)

	var entries []provider.RemoteEntry
	offset := 0
	for {
		var meta resourceMeta
		q := url.Values{
			"path":   {fullPath},
			"limit":  {strconv.Itoa(listPageSize)},
			"offset": {strconv.Itoa(offset)},
		}
		if err := p.apiCall(ctx, http.MethodGet, "/resources", q, &meta); err != nil {
			return nil, fmt.Errorf("yandexdisk: list %q: %w", remotePath, err)
		}
		if meta.Embedded == nil || len(meta.Embedded.Items) == 0 {
			break
		}
		for _, item := range meta.Embedded.Items {
			modTime, _ := time.Parse(time.RFC3339, item.Modified)
			entries = append(entries, provider.RemoteEntry{
				Path:    joinRemotePath(remotePath, item.Name),
				Name:    item.Name,
				Size:    item.Size,
				IsDir:   item.Type == "dir",
				ModTime: modTime,
			})
		}
		offset += len(meta.Embedded.Items)
		if offset >= meta.Embedded.Total {
			break
		}
	}
	return entries, nil
}

// Delete removes the object at remotePath permanently (bypassing Trash, to
// match the same "gone means gone" convention every other provider's
// Delete follows). Deleting an already-missing object is not an error - see
// internal/secrets.Store.Delete for the same idempotency principle.
func (p *Provider) Delete(ctx context.Context, remotePath string) error {
	fullPath := joinYandexPath(p.cfg.RootPath, remotePath)
	q := url.Values{"path": {fullPath}, "permanently": {"true"}}
	err := p.apiCall(ctx, http.MethodDelete, "/resources", q, nil)
	if err != nil {
		var aerr *apiErr
		if errors.As(err, &aerr) && aerr.Code == "DiskNotFoundError" {
			return nil
		}
		return fmt.Errorf("yandexdisk: delete %q: %w", remotePath, err)
	}
	return nil
}

// Exists implements provider.ExistenceChecker.
func (p *Provider) Exists(ctx context.Context, remotePath string) (bool, error) {
	fullPath := joinYandexPath(p.cfg.RootPath, remotePath)
	_, err := p.statResource(ctx, fullPath)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("yandexdisk: stat %q: %w", remotePath, err)
	}
	return true, nil
}

// VerifyChecksum implements provider.ChecksumVerifier by re-reading the
// object's md5 metadata field - no re-download needed, since the Disk API
// already stores and serves this value reliably (see the package doc
// comment).
func (p *Provider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	if algo != checksumAlgo {
		return false, fmt.Errorf("yandexdisk: cannot verify checksum of algo %q", algo)
	}

	fullPath := joinYandexPath(p.cfg.RootPath, remotePath)
	meta, err := p.statResource(ctx, fullPath)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return false, fmt.Errorf("yandexdisk: verify %q: not found", remotePath)
		}
		return false, fmt.Errorf("yandexdisk: verify %q: %w", remotePath, err)
	}
	return meta.MD5 == checksum, nil
}

var errNotFound = errors.New("yandexdisk: not found")

// statResource fetches the metadata of the exact resource at fullPath (not
// its children, unlike List), translating the documented
// "DiskNotFoundError" into errNotFound so callers can each decide how to
// report a miss (Exists: false/nil; Delete: idempotent success; everything
// else: a real error).
func (p *Provider) statResource(ctx context.Context, fullPath string) (*resourceMeta, error) {
	var meta resourceMeta
	q := url.Values{"path": {fullPath}}
	err := p.apiCall(ctx, http.MethodGet, "/resources", q, &meta)
	if err != nil {
		var aerr *apiErr
		if errors.As(err, &aerr) && aerr.Code == "DiskNotFoundError" {
			return nil, errNotFound
		}
		return nil, err
	}
	return &meta, nil
}

// ensureParentFolders creates every intermediate directory in fullPath
// (everything but the final path segment, which Upload is about to create
// as a file), since the official upload reference documents no automatic
// parent-folder creation - unlike Dropbox's /files/upload, which does
// create them implicitly.
func (p *Provider) ensureParentFolders(ctx context.Context, fullPath string) error {
	trimmed := strings.Trim(fullPath, "/")
	if trimmed == "" {
		return nil
	}
	segments := strings.Split(trimmed, "/")
	if len(segments) <= 1 {
		return nil // the file lives directly at the Disk (or RootPath) root
	}

	current := ""
	for _, seg := range segments[:len(segments)-1] {
		current += "/" + seg
		if err := p.mkdirIdempotent(ctx, current); err != nil {
			return fmt.Errorf("creating folder %q: %w", current, err)
		}
	}
	return nil
}

// mkdirIdempotent creates the folder at path, treating "already exists" as
// success. Per the official create-folder reference, PUT /resources returns
// 409 both when the folder already exists (error code
// "DiskPathPointsToExistentDirectoryError") and when a *parent* of path is
// missing - the two are told apart by the error code, not the status alone,
// which is why any other error is propagated rather than swallowed.
func (p *Provider) mkdirIdempotent(ctx context.Context, path string) error {
	q := url.Values{"path": {path}}
	err := p.apiCall(ctx, http.MethodPut, "/resources", q, nil)
	if err == nil {
		return nil
	}
	var aerr *apiErr
	if errors.As(err, &aerr) && aerr.Code == "DiskPathPointsToExistentDirectoryError" {
		return nil
	}
	return err
}

// apiCall issues one Disk API request with query parameters (this API has
// no JSON request bodies at all outside of the raw bytes PUT to an upload
// href - see putBytes) and decodes a 2xx JSON response into respOut
// (skipped when respOut is nil).
//
// Deliberately no "fields" partial-response parameter anywhere in this
// package: it would save some bandwidth, but a single typo in a
// dot-separated field path (e.g. "_embedded.items.path") would silently
// drop that field from every response instead of failing loudly - not a
// trade worth making for an MVP that already fits every response in
// memory.
func (p *Provider) apiCall(ctx context.Context, method, path string, query url.Values, respOut any) error {
	u := apiBaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return newAPIErr(resp)
	}
	if respOut == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(respOut); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return nil
}

// apiErr represents a non-2xx Disk API JSON error response, documented as
// {"error": "<Code>", "description": "..."} (e.g. "DiskNotFoundError").
type apiErr struct {
	StatusCode  int
	Code        string
	Description string
}

func newAPIErr(resp *http.Response) *apiErr {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var parsed struct {
		Error       string `json:"error"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(body, &parsed)
	return &apiErr{StatusCode: resp.StatusCode, Code: parsed.Error, Description: parsed.Description}
}

func (e *apiErr) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s (status %d)", e.Code, e.Description, e.StatusCode)
	}
	return fmt.Sprintf("unexpected api status %d", e.StatusCode)
}

// joinYandexPath returns the absolute Disk path for path, prefixed by root
// (this connection's configured RootPath, if any) - mirrors dropbox's
// joinDropboxPath. Yandex Disk paths must start with "/"; an empty result
// (both root and path empty) means the Disk root, which is what
// GET /resources expects for "/".
func joinYandexPath(root, path string) string {
	root = strings.Trim(root, "/")
	path = strings.Trim(path, "/")
	switch {
	case root == "" && path == "":
		return "/"
	case path == "":
		return "/" + root
	case root == "":
		return "/" + path
	default:
		return "/" + root + "/" + path
	}
}

// joinRemotePath returns the RemoteEntry.Path List reports for a child
// named name found under dir - mirrors googledrive's joinRemotePath.
func joinRemotePath(dir, name string) string {
	return path.Join(strings.Trim(dir, "/"), name)
}
