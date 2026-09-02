// Package onedrive implements provider.Provider over the Microsoft Graph API
// (https://graph.microsoft.com/v1.0), covering both personal OneDrive and,
// via the optional driveId config field, a SharePoint document library or a
// second user's OneDrive - anything Graph exposes as a "drive". Personal
// OneDrive is addressed as /me/drive, everything else as /drives/{driveId}.
//
// Like googledrive/dropbox/yandexdisk, a OneDrive connection cannot be
// constructed from config + secrets alone the first time: it needs an
// interactive OAuth2 consent step (open a browser, the user approves access,
// Microsoft redirects back with an authorization code) before any refresh
// token exists. See the package doc comment on internal/providers/dropbox
// for the general shape this package follows (Authorize as a separate
// one-time step in auth.go, app-wide OAuth Client ID/Secret under
// AppCredentialsConnectionID, per-connection refresh token).
//
// Unlike googledrive, this package does not depend on an official Microsoft
// Graph SDK - the idiomatic Go SDKs for Graph are heavy, codegen-based
// packages with a much larger surface than cloudup needs; a hand-rolled
// net/http + encoding/json client, the same shape as internal/providers/
// dropbox and internal/providers/yandexdisk, is a better fit for the handful
// of endpoints this package actually calls. graphBaseURL is a package
// variable purely so tests can redirect it to a local httptest.Server.
//
// Checksum: Graph reports a native, non-cryptographic hash
// (file.hashes.quickXorHash) on every uploaded item's metadata, so
// VerifyChecksum here is a cheap metadata GET rather than the
// re-download-and-rehash approach dropbox is forced into - the same
// "trust the server's own hash" shape as googledrive/b2/yandexdisk.
package onedrive

import (
	"bytes"
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

	"cloudup/internal/debuglog"
	"cloudup/internal/provider"
	"cloudup/internal/registry"
	"cloudup/internal/streamio"
)

const Type = "onedrive"

const (
	secretClientID     = "clientId"
	secretClientSecret = "clientSecret"
	// secretRefreshToken is intentionally absent from ConfigFields - see the
	// package doc comment. It is written directly by the caller that
	// invoked Authorize, using this key.
	secretRefreshToken = "refreshToken"
)

// AppCredentialsConnectionID is the fixed pseudo-connection-ID under which
// the OAuth Client ID/Secret (an Azure AD App Registration) are stored in
// the secret store - one registered app covers every OneDrive connection
// cloudup ever makes, exactly like dropbox.AppCredentialsConnectionID.
const AppCredentialsConnectionID = "onedrive-app-credentials"

// ClientIDKey, ClientSecretKey and RefreshTokenKey are the secret-store keys
// this provider reads/writes, exported so callers outside the package
// (internal/httpapi) can read or write them without duplicating magic
// strings.
const (
	ClientIDKey     = secretClientID
	ClientSecretKey = secretClientSecret
	RefreshTokenKey = secretRefreshToken
)

// checksumAlgo is the label stored in UploadResult/upload_log - see the
// package doc comment on why this trusts Graph's native hash.
const checksumAlgo = "quickXorHash"

// graphBaseURL is a package variable (rather than a constant) purely so
// tests can redirect it to a local httptest.Server without touching the
// real Graph API - same trick as dropbox's apiBaseURL/contentBaseURL.
var graphBaseURL = "https://graph.microsoft.com/v1.0"

// oauthEndpoint is a package variable for the same reason - tests redirect
// the OAuth token exchange to a local fake server without touching
// Microsoft. "common" allows sign-in with both personal Microsoft accounts
// and work/school (Azure AD) accounts, matching the driveId-based
// personal-vs-SharePoint split this package supports.
var oauthEndpoint = oauth2.Endpoint{
	AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
	TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
}

// oauthScopes requests read/write access to the user's files plus a basic
// profile scope for TestConnection. offline_access is what makes Microsoft's
// v2 endpoint issue a refresh token on the initial exchange - unlike Google
// (access_type=offline) or Dropbox (token_access_type=offline), this is a
// plain scope rather than a special AuthCodeOption, so auth.go needs no
// AuthCodeOpts at all.
var oauthScopes = []string{
	"offline_access",
	"Files.ReadWrite",
	"User.Read",
}

func init() {
	registry.Register(Type, New)
	registry.RegisterSchema(Type, (&Provider{}).ConfigFields)
	registry.RegisterOAuth(Type, oauthFlow())
}

// rawConfig is the non-secret part of a OneDrive connection, persisted in
// the JSON config file. The OAuth Client ID/Secret (app-wide) and this
// connection's own refresh token live in the secret store.
type rawConfig struct {
	ConnectionID string `json:"connectionId"`
	DriveID      string `json:"driveId"`  // optional; empty means /me/drive
	RootPath     string `json:"rootPath"` // optional; empty means the drive root
	DisplayName  string `json:"displayName"`
}

// Provider implements provider.Provider (and several optional feature
// interfaces) over the Microsoft Graph API.
type Provider struct {
	cfg rawConfig

	// httpClient is an OAuth2-authenticated *http.Client (built from the
	// connection's refresh token by New), with CheckRedirect overridden -
	// see stripAuthOnCrossHostRedirect. Tests construct a Provider directly
	// with a plain *http.Client pointed at an httptest.Server, bypassing
	// New/OAuth entirely - see onedrive_wire_test.go.
	httpClient *http.Client
}

// New is the registry.Factory for the "onedrive" provider type. It requires
// that the app-wide OAuth Client ID/Secret are configured (see
// AppCredentialsConnectionID) and that Authorize has already been run for
// this specific connection, with its refresh token stored under
// secretRefreshToken.
func New(rawCfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
	var cfg rawConfig
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return nil, fmt.Errorf("onedrive: invalid config: %w", err)
	}

	clientID, err := secrets.Get(AppCredentialsConnectionID, secretClientID)
	if err != nil {
		return nil, fmt.Errorf("onedrive: reading client ID: %w", err)
	}
	clientSecret, err := secrets.Get(AppCredentialsConnectionID, secretClientSecret)
	if err != nil {
		return nil, fmt.Errorf("onedrive: reading client secret: %w", err)
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("onedrive: OAuth Client ID/Secret not configured - set them in Settings first")
	}
	refreshToken, err := secrets.Get(cfg.ConnectionID, secretRefreshToken)
	if err != nil {
		return nil, fmt.Errorf("onedrive: reading refresh token: %w", err)
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("onedrive: connection not authorized yet - run Authorize first")
	}

	oauthCfg := oauthConfig(clientID, clientSecret)
	// oauth2.HTTPClient in the context sets the *base* client the token
	// source and the returned client both transport over, which is the only
	// hook for slipping debuglog underneath OAuth. debuglog.Transport is a
	// transparent passthrough unless CLOUDUP_DEBUG is set.
	baseCtx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: debuglog.Transport{}})
	tokenSource := oauthCfg.TokenSource(baseCtx, &oauth2.Token{RefreshToken: refreshToken})
	httpClient := oauth2.NewClient(baseCtx, tokenSource)
	httpClient.CheckRedirect = stripAuthOnCrossHostRedirect

	return &Provider{cfg: cfg, httpClient: httpClient}, nil
}

// stripAuthOnCrossHostRedirect removes the Authorization header before
// net/http follows a redirect to a different host than the original
// request. Graph's content-download endpoint (see Download) 302s to a
// separate, pre-authenticated SharePoint/OneDrive blob-storage host that
// neither needs nor expects our bearer token - resending it there would leak
// it to a host outside Microsoft's own token-issuing domain. Overriding
// CheckRedirect makes this explicit and provider-owned rather than relying
// on whatever the standard library's default policy happens to do, and
// reimplements its usual 10-redirect cap since setting CheckRedirect
// replaces the default entirely.
func stripAuthOnCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}
	return nil
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
	return "OneDrive"
}

// ConfigFields implements provider.ConfigSchema. Deliberately excludes the
// OAuth Client ID/Secret (app-wide, configured once - see the package doc
// comment) and the refresh token (per-connection, but written directly by
// the caller that invoked Authorize, not typed into a form) - same
// reasoning as dropbox/googledrive/yandexdisk's ConfigFields.
func (p *Provider) ConfigFields() []provider.FieldSpec {
	return []provider.FieldSpec{
		{Key: "driveId", Label: "Drive ID, optional - leave empty for your personal OneDrive, or set it to a SharePoint document library's drive ID to upload there instead", Type: provider.FieldText},
		{Key: "rootPath", Label: "Subfolder in the drive to upload into, optional (e.g. backups/cloudup) - leave empty for the drive root", Type: provider.FieldText},
	}
}

// driveBase returns the Graph URL prefix identifying this connection's
// drive: /me/drive for personal OneDrive (the common case, DriveID unset),
// or /drives/{driveId} for a specific drive - a second user's OneDrive or a
// SharePoint document library.
func (p *Provider) driveBase() string {
	if p.cfg.DriveID == "" {
		return graphBaseURL + "/me/drive"
	}
	return graphBaseURL + "/drives/" + p.cfg.DriveID
}

// itemSegment returns the Graph path-addressing segment for the item at
// fullPath within a drive (append to driveBase(), then optionally
// "/content", "/children" or "/createUploadSession"): "/root" for the drive
// root itself (fullPath == ""), or the colon-delimited "/root:/{path}:" form
// Graph uses to address a nested item by path instead of by ID.
func itemSegment(fullPath string) string {
	if fullPath == "" {
		return "/root"
	}
	return "/root:/" + fullPath + ":"
}

// TestConnection fetches this connection's drive metadata - the same
// lightweight "is this token valid at all" probe every other provider's
// TestConnection performs.
func (p *Provider) TestConnection(ctx context.Context) error {
	if err := p.callAPI(ctx, http.MethodGet, p.driveBase(), nil, nil); err != nil {
		return fmt.Errorf("onedrive: connection test failed: %w", err)
	}
	return nil
}

// driveItem mirrors the fields of Graph's driveItem resource
// (https://learn.microsoft.com/en-us/graph/api/resources/driveitem) this
// package actually uses - not every field the API can return.
type driveItem struct {
	ID                   string     `json:"id"`
	Name                 string     `json:"name"`
	Size                 int64      `json:"size"`
	WebURL               string     `json:"webUrl"`
	LastModifiedDateTime string     `json:"lastModifiedDateTime"`
	Folder               *struct{}  `json:"folder,omitempty"`
	File                 *driveFile `json:"file,omitempty"`
}

type driveFile struct {
	Hashes struct {
		QuickXorHash string `json:"quickXorHash"`
	} `json:"hashes"`
}

// quickXorHash returns the item's native checksum, or "" for a folder or a
// file Graph didn't report a hash for.
func (d driveItem) quickXorHash() string {
	if d.File == nil {
		return ""
	}
	return d.File.Hashes.QuickXorHash
}

// Upload PUTs task.Reader directly to task.RemotePath's content endpoint -
// Graph's single-request upload path, usable up to 4 MiB. Larger files go
// through UploadMultipart instead (internal/queue picks between the two
// based on size, see provider.MultipartUploader's doc comment). The
// resulting checksum is Graph's own quickXorHash, read back from the same
// response - no local hashing needed, see the package doc comment.
func (p *Provider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	fullPath := joinOnedrivePath(p.cfg.RootPath, task.RemotePath)
	if fullPath == "" {
		return provider.UploadResult{}, fmt.Errorf("onedrive: remote path %q has no file name", task.RemotePath)
	}

	reader := &streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.driveBase()+itemSegment(fullPath)+"/content", reader)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("onedrive: upload %q: %w", task.RemotePath, err)
	}
	req.ContentLength = task.Size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("onedrive: upload %q: %w", task.RemotePath, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("onedrive: upload %q: reading response: %w", task.RemotePath, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return provider.UploadResult{}, fmt.Errorf("onedrive: upload %q: %w", task.RemotePath, newAPIErr(resp.StatusCode, body))
	}

	var item driveItem
	if err := json.Unmarshal(body, &item); err != nil {
		return provider.UploadResult{}, fmt.Errorf("onedrive: upload %q: decoding response: %w", task.RemotePath, err)
	}

	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    item.WebURL,
		ChecksumAlgo: checksumAlgo,
		Checksum:     item.quickXorHash(),
	}, nil
}

// chunkGranularity and maxChunkMultiples bound the chunk size accepted by
// Graph's upload-session API (createUploadSession + chunked PUTs to the
// returned uploadUrl), which Microsoft's documentation constrains in two
// independent ways:
//
//   - every chunk except the last must be a multiple of 320 KiB.
//   - Microsoft's own guidance recommends chunks no larger than about 60 MiB
//     per request for reliability over typical connections.
//
// So the usable chunk size is a multiple of 320 KiB that stays at or below
// that guidance: 192 * 320 KiB = 60 MiB exactly. These are variables rather
// than constants purely so tests can shrink them and exercise the chunking
// loop with byte-sized payloads - same trick as dropbox's chunkGranularity.
var (
	chunkGranularity  int64 = 320 * 1024
	maxChunkMultiples int64 = 192
)

// clampPartSize rounds partSize into the range Graph's upload-session API
// actually accepts (see chunkGranularity): down to a whole multiple of the
// 320 KiB granularity, at least one such multiple, and never above the
// largest multiple that stays within Microsoft's per-request guidance.
// internal/queue picks a generic part size with no knowledge of any
// particular provider's limits, so silently sending a value Graph will
// reject mid-session - after some chunks are already uploaded - is exactly
// the failure mode this avoids.
func clampPartSize(partSize int64) int64 {
	if maxChunk := maxChunkMultiples * chunkGranularity; partSize > maxChunk {
		return maxChunk
	}
	partSize -= partSize % chunkGranularity
	if partSize < chunkGranularity {
		return chunkGranularity
	}
	return partSize
}

// UploadMultipart implements provider.MultipartUploader using Graph's
// upload-session API: createUploadSession opens the session, then each
// chunk is PUT to the returned uploadUrl with a Content-Range header. Unlike
// Dropbox's upload_session (start/append_v2/finish), there is no separate
// "finish" call - the session completes itself the moment a chunk's
// Content-Range reaches the file's full size, at which point Graph responds
// with the finished driveItem instead of the usual 202 Accepted.
//
// Like dropbox.UploadMultipart, task.Reader is wrapped exactly once - in a
// ProgressReader - and every chunk is sliced off that single wrapped reader,
// so progress runs continuously from 0 to task.Size instead of restarting
// per chunk. There is no parallel variant (provider.ParallelMultipartUploader)
// for the same reason Dropbox has none: Graph's Content-Range chunks must
// arrive in order, at the exact offset the server has already accepted -
// out-of-order or concurrent chunks are a protocol violation, not a missing
// feature.
//
// An empty file is delegated to Upload instead of being sent through a
// session: Graph's chunked-upload protocol is built around a non-empty
// Content-Range, and internal/queue never picks this path for a 0-byte file
// in practice anyway (it only uses UploadMultipart above a size threshold).
func (p *Provider) UploadMultipart(ctx context.Context, task provider.UploadTask, partSize int64) (provider.UploadResult, error) {
	if partSize <= 0 {
		return provider.UploadResult{}, fmt.Errorf("onedrive: partSize must be positive")
	}
	if task.Size == 0 {
		return p.Upload(ctx, task)
	}
	partSize = clampPartSize(partSize)

	fullPath := joinOnedrivePath(p.cfg.RootPath, task.RemotePath)
	if fullPath == "" {
		return provider.UploadResult{}, fmt.Errorf("onedrive: remote path %q has no file name", task.RemotePath)
	}

	uploadURL, err := p.createUploadSession(ctx, fullPath)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("onedrive: chunked upload of %q: starting session: %w", task.RemotePath, err)
	}

	source := &streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}
	buf := make([]byte, partSize)

	var offset int64
	for offset < task.Size {
		chunkLen := partSize
		if remaining := task.Size - offset; chunkLen > remaining {
			chunkLen = remaining
		}

		n, readErr := io.ReadFull(source, buf[:chunkLen])
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return provider.UploadResult{}, fmt.Errorf("onedrive: chunked upload of %q: reading source at offset %d: %w", task.RemotePath, offset, readErr)
		}
		if int64(n) != chunkLen {
			return provider.UploadResult{}, fmt.Errorf("onedrive: chunked upload of %q: source ended early at offset %d, expected %d more bytes", task.RemotePath, offset, task.Size-offset)
		}

		item, err := p.putChunk(ctx, uploadURL, buf[:n], offset, task.Size)
		if err != nil {
			return provider.UploadResult{}, fmt.Errorf("onedrive: chunked upload of %q: uploading %d bytes at offset %d: %w", task.RemotePath, n, offset, err)
		}
		offset += int64(n)

		if item != nil {
			return provider.UploadResult{
				RemotePath:   task.RemotePath,
				RemoteURL:    item.WebURL,
				ChecksumAlgo: checksumAlgo,
				Checksum:     item.quickXorHash(),
			}, nil
		}
	}

	return provider.UploadResult{}, fmt.Errorf("onedrive: chunked upload of %q: server never finalized the session", task.RemotePath)
}

// createUploadSession opens an upload session for fullPath, replacing any
// existing item there (matching Upload's overwrite semantics), and returns
// the uploadUrl every chunk in this session must be PUT to.
func (p *Provider) createUploadSession(ctx context.Context, fullPath string) (string, error) {
	reqBody := struct {
		Item struct {
			ConflictBehavior string `json:"@microsoft.graph.conflictBehavior"`
		} `json:"item"`
	}{}
	reqBody.Item.ConflictBehavior = "replace"

	var resp struct {
		UploadURL string `json:"uploadUrl"`
	}
	url := p.driveBase() + itemSegment(fullPath) + "/createUploadSession"
	if err := p.callAPI(ctx, http.MethodPost, url, reqBody, &resp); err != nil {
		return "", err
	}
	if resp.UploadURL == "" {
		return "", fmt.Errorf("server returned no uploadUrl")
	}
	return resp.UploadURL, nil
}

// putChunk PUTs chunk (the bytes at [offset, offset+len(chunk)) of a total
// file of size total) to an open upload session. A 202 response means the
// session is still open (more chunks expected) and putChunk returns a nil
// item; a 200/201 response means this chunk completed the file and its body
// is the finished driveItem.
func (p *Provider) putChunk(ctx context.Context, uploadURL string, chunk []byte, offset, total int64) (*driveItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(chunk))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(chunk))
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(len(chunk))-1, total))

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var item driveItem
		if err := json.Unmarshal(body, &item); err != nil {
			return nil, fmt.Errorf("decoding finished item: %w", err)
		}
		return &item, nil
	case http.StatusAccepted:
		return nil, nil
	default:
		return nil, newAPIErr(resp.StatusCode, body)
	}
}

// Download fetches task.RemotePath's content. Graph typically 302-redirects
// this request to a separate, pre-authenticated blob-storage URL - see
// stripAuthOnCrossHostRedirect, wired in New, for why the Authorization
// header must not follow that redirect.
func (p *Provider) Download(ctx context.Context, task provider.DownloadTask) error {
	fullPath := joinOnedrivePath(p.cfg.RootPath, task.RemotePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.driveBase()+itemSegment(fullPath)+"/content", nil)
	if err != nil {
		return fmt.Errorf("onedrive: download %q: %w", task.RemotePath, err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("onedrive: download %q: %w", task.RemotePath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("onedrive: download %q: %w", task.RemotePath, newAPIErr(resp.StatusCode, body))
	}

	writer := task.Writer
	if task.Progress != nil {
		writer = &streamio.ProgressWriter{W: writer, Total: resp.ContentLength, OnProgress: task.Progress}
	}
	if _, err := io.Copy(writer, resp.Body); err != nil {
		return fmt.Errorf("onedrive: download %q: %w", task.RemotePath, err)
	}
	return nil
}

// List enumerates the immediate children of remotePath, following Graph's
// @odata.nextLink cursor pagination until exhausted.
func (p *Provider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	fullPath := joinOnedrivePath(p.cfg.RootPath, remotePath)
	url := p.driveBase() + itemSegment(fullPath) + "/children"

	var entries []provider.RemoteEntry
	for url != "" {
		var page struct {
			Value    []driveItem `json:"value"`
			NextLink string      `json:"@odata.nextLink"`
		}
		if err := p.callAPI(ctx, http.MethodGet, url, nil, &page); err != nil {
			return nil, fmt.Errorf("onedrive: list %q: %w", remotePath, err)
		}
		for _, item := range page.Value {
			modTime, _ := time.Parse(time.RFC3339, item.LastModifiedDateTime)
			entries = append(entries, provider.RemoteEntry{
				Path:    joinRemotePath(remotePath, item.Name),
				Name:    item.Name,
				Size:    item.Size,
				IsDir:   item.Folder != nil,
				ModTime: modTime,
			})
		}
		url = page.NextLink
	}
	return entries, nil
}

// Delete removes the item at remotePath. Deleting an already-missing item is
// not treated as an error, matching this repo's idempotency convention (see
// e.g. internal/secrets.Store.Delete) - Graph reports this case as an HTTP
// 404 with error.code "itemNotFound".
func (p *Provider) Delete(ctx context.Context, remotePath string) error {
	fullPath := joinOnedrivePath(p.cfg.RootPath, remotePath)
	err := p.callAPI(ctx, http.MethodDelete, p.driveBase()+itemSegment(fullPath), nil, nil)
	if err != nil {
		var aerr *apiErr
		if errors.As(err, &aerr) && aerr.notFound() {
			return nil
		}
		return fmt.Errorf("onedrive: delete %q: %w", remotePath, err)
	}
	return nil
}

// Exists implements provider.ExistenceChecker.
func (p *Provider) Exists(ctx context.Context, remotePath string) (bool, error) {
	fullPath := joinOnedrivePath(p.cfg.RootPath, remotePath)
	_, err := p.statItem(ctx, fullPath)
	if err != nil {
		var aerr *apiErr
		if errors.As(err, &aerr) && aerr.notFound() {
			return false, nil
		}
		return false, fmt.Errorf("onedrive: stat %q: %w", remotePath, err)
	}
	return true, nil
}

// VerifyChecksum implements provider.ChecksumVerifier by re-reading the
// item's quickXorHash metadata field - no re-download needed, see the
// package doc comment.
func (p *Provider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	if algo != checksumAlgo {
		return false, fmt.Errorf("onedrive: cannot verify checksum of algo %q", algo)
	}

	fullPath := joinOnedrivePath(p.cfg.RootPath, remotePath)
	item, err := p.statItem(ctx, fullPath)
	if err != nil {
		var aerr *apiErr
		if errors.As(err, &aerr) && aerr.notFound() {
			return false, fmt.Errorf("onedrive: verify %q: not found", remotePath)
		}
		return false, fmt.Errorf("onedrive: verify %q: %w", remotePath, err)
	}
	return item.quickXorHash() == checksum, nil
}

// statItem fetches the metadata of the exact item at fullPath (not its
// children, unlike List).
func (p *Provider) statItem(ctx context.Context, fullPath string) (*driveItem, error) {
	var item driveItem
	if err := p.callAPI(ctx, http.MethodGet, p.driveBase()+itemSegment(fullPath), nil, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// joinOnedrivePath returns the Graph colon-path for path, prefixed by root
// (this connection's configured RootPath, if any) - mirrors dropbox's
// joinDropboxPath. Unlike Dropbox, Graph paths passed to itemSegment carry
// no leading "/" of their own (itemSegment supplies "/root:/" itself), so
// this just joins the two trimmed segments with "/".
func joinOnedrivePath(root, path string) string {
	root = strings.Trim(root, "/")
	path = strings.Trim(path, "/")
	switch {
	case root == "" && path == "":
		return ""
	case path == "":
		return root
	case root == "":
		return path
	default:
		return root + "/" + path
	}
}

// joinRemotePath returns the RemoteEntry.Path List reports for a child named
// name found under dir - mirrors googledrive/yandexdisk's joinRemotePath.
func joinRemotePath(dir, name string) string {
	return path.Join(strings.Trim(dir, "/"), name)
}

// callAPI issues one Graph JSON request and decodes a 2xx response body into
// respOut (skipped when respOut is nil or the body is empty, as Graph's
// DELETE responses are). A non-2xx response is turned into an *apiErr - see
// newAPIErr and apiErr.notFound - used consistently by every JSON API call
// in this file instead of duplicating this parsing per call site. url is
// always an already-complete Graph URL (built by the caller via driveBase/
// itemSegment, or a page's @odata.nextLink), never joined with graphBaseURL
// here, so pagination links Graph itself returns work unchanged.
func (p *Provider) callAPI(ctx context.Context, method, url string, reqBody, respOut any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIErr(resp.StatusCode, body)
	}
	if respOut != nil && len(body) > 0 {
		if err := json.Unmarshal(body, respOut); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// apiErr represents a non-2xx Graph API JSON error response, documented as
// {"error": {"code": "...", "message": "..."}}.
type apiErr struct {
	StatusCode int
	Code       string
	Message    string
	Raw        string
}

func newAPIErr(statusCode int, body []byte) *apiErr {
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	return &apiErr{StatusCode: statusCode, Code: parsed.Error.Code, Message: parsed.Error.Message, Raw: strings.TrimSpace(string(body))}
}

func (e *apiErr) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s (status %d)", e.Code, e.Message, e.StatusCode)
	}
	return fmt.Sprintf("unexpected status %d: %s", e.StatusCode, e.Raw)
}

// notFound reports whether this error is Graph's convention for "the given
// item does not exist" - a 404 status with error.code "itemNotFound". The
// status alone is checked too, in case a test double or an older API
// revision omits the structured code.
func (e *apiErr) notFound() bool {
	return e.StatusCode == http.StatusNotFound || e.Code == "itemNotFound"
}
