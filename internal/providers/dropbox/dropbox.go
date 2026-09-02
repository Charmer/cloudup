// Package dropbox implements provider.Provider over the Dropbox API v2.
//
// Like googledrive, a Dropbox connection cannot be constructed from config +
// secrets alone the first time: it needs an interactive OAuth2 consent step
// (open a browser, the user approves access, Dropbox redirects back with an
// authorization code) before any refresh token exists. That step is exposed
// as the package-level Authorize function (auth.go), called once by the
// connection-setup API flow - it is deliberately not part of the
// provider.Provider/ConfigSchema machinery, since ConfigSchema describes
// static form fields and has no notion of a multi-step interactive flow.
// Once Authorize succeeds, its refresh token is stored under
// secretRefreshToken by the caller, and New (the registry.Factory, used for
// every subsequent construction) can build a working client from it like
// any other provider.
//
// The OAuth Client ID/Secret are a property of the *app* registered with
// Dropbox (one "App" covers every Dropbox connection cloudup ever makes),
// not of an individual connection - ConfigFields does not ask for them per
// connection. They are configured once (frontend: Settings tab, i.e.
// PUT /api/v1/provider-types/dropbox/oauth-credentials) and stored under
// AppCredentialsConnectionID, shared by every Dropbox connection's
// Authorize/New call.
//
// Unlike googledrive, this package does not depend on an official Dropbox
// SDK - Dropbox's HTTP API is simple enough (plain JSON POST requests, plus
// two "content" endpoints that take/return raw bytes with a JSON header)
// that golang.org/x/oauth2 (already a module dependency, for the token
// exchange/refresh) plus net/http/encoding/json is enough. apiBaseURL and
// contentBaseURL are package variables purely so tests can redirect them to
// a local httptest.Server.
package dropbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"cloudup/internal/debuglog"
	"cloudup/internal/provider"
	"cloudup/internal/registry"
	"cloudup/internal/streamio"
)

const Type = "dropbox"

const (
	secretClientID     = "clientId"
	secretClientSecret = "clientSecret"
	// secretRefreshToken is intentionally absent from ConfigFields - see the
	// package doc comment. It is written directly by the caller that
	// invoked Authorize, using this key.
	secretRefreshToken = "refreshToken"
)

// AppCredentialsConnectionID is the fixed pseudo-connection-ID under which
// the OAuth Client ID/Secret are stored in the secret store - see the
// package doc comment's "one App for the whole app" note. It is not a real
// config.Connection ID; it never appears in config.json, only as a
// secrets.Store key namespace, exactly like a real connection ID would be.
const AppCredentialsConnectionID = "dropbox-app-credentials"

// ClientIDKey, ClientSecretKey and RefreshTokenKey are the secret-store keys
// this provider reads/writes, exported so callers outside the package
// (internal/httpapi) can read or write them without duplicating
// magic strings. ClientIDKey/ClientSecretKey are stored once under
// AppCredentialsConnectionID (shared by every Dropbox connection);
// RefreshTokenKey is per-connection, written by the caller that invoked
// Authorize for that specific connection.
const (
	ClientIDKey     = secretClientID
	ClientSecretKey = secretClientSecret
	RefreshTokenKey = secretRefreshToken
)

// checksumAlgo is the label stored in UploadResult/upload_log. Dropbox's
// native content_hash is a nonstandard hash (SHA-256 of 4MiB blocks, then
// SHA-256 of the concatenated block hashes) that isn't worth reimplementing
// for this MVP, so - like webdav/s3 - this provider computes its own
// SHA-256 while streaming the upload and verifies by re-downloading and
// rehashing (see VerifyChecksum) rather than trusting a Dropbox-native
// value.
const checksumAlgo = provider.ChecksumSHA256SelfComputed

// apiBaseURL and contentBaseURL are package variables (rather than
// constants) purely so tests can redirect them to a local httptest.Server
// without touching the real Dropbox API.
var (
	apiBaseURL     = "https://api.dropboxapi.com/2"
	contentBaseURL = "https://content.dropboxapi.com/2"
)

// oauthEndpoint is a package variable (rather than a literal inside
// oauthConfig) purely so tests can redirect the OAuth token exchange to a
// local fake server without touching Dropbox - same trick as googledrive's
// oauthEndpoint.
var oauthEndpoint = oauth2.Endpoint{
	AuthURL:  "https://www.dropbox.com/oauth2/authorize",
	TokenURL: "https://api.dropboxapi.com/oauth2/token",
}

// oauthScopes is the fixed set of scopes cloudup requests - enough to read
// account info (for TestConnection) and read/write file content and
// metadata.
var oauthScopes = []string{
	"account_info.read",
	"files.content.write",
	"files.content.read",
	"files.metadata.read",
	"files.metadata.write",
}

func init() {
	registry.Register(Type, New)
	registry.RegisterSchema(Type, (&Provider{}).ConfigFields)
	// Unlike webdav/s3/b2, this type also needs an interactive consent step
	// before New can ever succeed - registering it here is what lets the
	// REST API offer "authorize this connection" generically, without
	// importing this package (see auth.go).
	registry.RegisterOAuth(Type, oauthFlow())
}

// rawConfig is the non-secret part of a Dropbox connection, persisted in the
// JSON config file. The OAuth Client ID/Secret (app-wide, not per-connection
// - see the package doc comment) and this connection's own refresh token
// live in the secret store.
type rawConfig struct {
	ConnectionID string `json:"connectionId"`
	RootPath     string `json:"rootPath"` // optional; empty means the Dropbox root
	DisplayName  string `json:"displayName"`
}

// Provider implements provider.Provider (and several optional feature
// interfaces) over the Dropbox API.
type Provider struct {
	cfg rawConfig

	// httpClient is an OAuth2-authenticated *http.Client (built from the
	// connection's refresh token by New). Tests construct a Provider
	// directly with a plain *http.Client pointed at an httptest.Server,
	// bypassing New/OAuth entirely - see dropbox_test.go.
	httpClient *http.Client
}

// New is the registry.Factory for the "dropbox" provider type. It requires
// that the app-wide OAuth Client ID/Secret are configured (see
// AppCredentialsConnectionID) and that Authorize has already been run for
// this specific connection, with its refresh token stored under
// secretRefreshToken.
func New(rawCfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
	var cfg rawConfig
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return nil, fmt.Errorf("dropbox: invalid config: %w", err)
	}

	clientID, err := secrets.Get(AppCredentialsConnectionID, secretClientID)
	if err != nil {
		return nil, fmt.Errorf("dropbox: reading client ID: %w", err)
	}
	clientSecret, err := secrets.Get(AppCredentialsConnectionID, secretClientSecret)
	if err != nil {
		return nil, fmt.Errorf("dropbox: reading client secret: %w", err)
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("dropbox: OAuth Client ID/Secret not configured - set them in Settings first")
	}
	refreshToken, err := secrets.Get(cfg.ConnectionID, secretRefreshToken)
	if err != nil {
		return nil, fmt.Errorf("dropbox: reading refresh token: %w", err)
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("dropbox: connection not authorized yet - run Authorize first")
	}

	oauthCfg := oauthConfig(clientID, clientSecret)
	// oauth2.HTTPClient in the context sets the *base* client the token
	// source and the returned client both transport over, which is the only
	// hook for slipping debuglog underneath OAuth. debuglog.Transport is a
	// transparent passthrough unless CLOUDUP_DEBUG is set.
	baseCtx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: debuglog.Transport{}})
	tokenSource := oauthCfg.TokenSource(baseCtx, &oauth2.Token{RefreshToken: refreshToken})
	httpClient := oauth2.NewClient(baseCtx, tokenSource)

	return &Provider{cfg: cfg, httpClient: httpClient}, nil
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
	return "Dropbox"
}

func (p *Provider) TestConnection(ctx context.Context) error {
	if err := p.callAPI(ctx, apiBaseURL+"/users/get_current_account", nil, nil); err != nil {
		return fmt.Errorf("dropbox: connection test failed: %w", err)
	}
	return nil
}

// dropboxAPIArg is the JSON structure sent in the Dropbox-API-Arg header of
// the /files/upload and /files/download content endpoints.
type uploadAPIArg struct {
	Path       string `json:"path"`
	Mode       string `json:"mode"`
	Autorename bool   `json:"autorename"`
	Mute       bool   `json:"mute"`
}

// Upload streams task.Reader to task.RemotePath (prefixed with this
// connection's RootPath, if any), computing a SHA-256 checksum while
// streaming so no second read of the source is needed - see the checksumAlgo
// doc comment for why this is self-computed rather than Dropbox-native.
func (p *Provider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	remotePath := joinDropboxPath(p.cfg.RootPath, task.RemotePath)
	if remotePath == "" {
		return provider.UploadResult{}, fmt.Errorf("dropbox: remote path %q has no file name", task.RemotePath)
	}

	argBytes, err := json.Marshal(uploadAPIArg{Path: remotePath, Mode: "overwrite", Autorename: false, Mute: false})
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("dropbox: upload %q: encoding API arg: %w", task.RemotePath, err)
	}

	h := sha256.New()
	reader := io.TeeReader(&streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}, h)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, contentBaseURL+"/files/upload", reader)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("dropbox: upload %q: %w", task.RemotePath, err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Dropbox-API-Arg", string(argBytes))

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("dropbox: upload %q: %w", task.RemotePath, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("dropbox: upload %q: reading response: %w", task.RemotePath, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return provider.UploadResult{}, fmt.Errorf("dropbox: upload %q: %w", task.RemotePath, newAPIErr(resp.StatusCode, body))
	}

	var result struct {
		PathDisplay string `json:"path_display"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return provider.UploadResult{}, fmt.Errorf("dropbox: upload %q: decoding response: %w", task.RemotePath, err)
	}

	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    result.PathDisplay,
		ChecksumAlgo: checksumAlgo,
		Checksum:     hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// chunkGranularity and maxChunkMultiples bound the chunk size accepted by
// Dropbox's upload_session API, which constrains it in two independent ways:
//
//   - every chunk that will be followed by another one (i.e. the
//     upload_session/start and upload_session/append_v2 bodies) must be a
//     multiple of 4 MiB - Dropbox rejects a session whose chunks are ragged.
//     Only the final upload_session/finish body may be an arbitrary size.
//   - no single request body may exceed 150 MB, the same cap that makes the
//     plain /files/upload endpoint unusable for large files in the first
//     place.
//
// So the usable chunk size is a multiple of 4 MiB that stays under 150 MB:
// 35 * 4 MiB = 140 MiB = 146,800,640 bytes fits, 36 * 4 MiB ≈ 151 MB does
// not. These are variables rather than constants purely so tests can shrink
// them and exercise the chunking loop with byte-sized payloads instead of
// hundreds of megabytes - same trick as apiBaseURL/contentBaseURL above.
var (
	chunkGranularity  int64 = 4 << 20
	maxChunkMultiples int64 = 35
)

// clampPartSize rounds partSize into the range Dropbox's upload_session API
// actually accepts (see chunkGranularity): down to a whole multiple of the
// 4 MiB granularity, at least one such multiple, and never above the largest
// multiple that stays under the 150 MB per-request cap. internal/queue picks
// a generic part size with no knowledge of any particular provider's limits,
// so silently sending a value Dropbox will reject mid-session - after some
// chunks are already uploaded - is exactly the failure mode this avoids.
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

// uploadSessionCursor identifies a chunked upload in progress. Offset is the
// number of bytes Dropbox has already accepted for this session, i.e. the
// position at which the accompanying request body will be written - not
// including that body. For the final upload_session/finish request with an
// empty body it therefore equals the total file size.
type uploadSessionCursor struct {
	SessionID string `json:"session_id"`
	Offset    int64  `json:"offset"`
}

// uploadSessionStartArg, uploadSessionAppendArg and uploadSessionFinishArg
// are the JSON structures sent in the Dropbox-API-Arg header of the three
// /files/upload_session/* content endpoints. Close is always false: cloudup
// never leaves a session open for a later process to resume, it finishes
// every session it starts within one UploadMultipart call.
type uploadSessionStartArg struct {
	Close bool `json:"close"`
}

type uploadSessionAppendArg struct {
	Cursor uploadSessionCursor `json:"cursor"`
	Close  bool                `json:"close"`
}

// uploadSessionFinishArg's Commit reuses uploadAPIArg because
// upload_session/finish takes exactly the same commit info (path/mode/
// autorename/mute) that the single-request /files/upload endpoint takes as
// its whole argument.
type uploadSessionFinishArg struct {
	Cursor uploadSessionCursor `json:"cursor"`
	Commit uploadAPIArg        `json:"commit"`
}

// UploadMultipart implements provider.MultipartUploader using Dropbox's
// upload_session API (start/append_v2/finish), which is the only way to
// upload files larger than the 150 MB cap on /files/upload.
//
// Like s3.UploadMultipart, task.Reader is wrapped exactly once - in a
// ProgressReader, then a TeeReader into the hasher - and every chunk is
// sliced off that single wrapped reader. That is what makes the resulting
// checksum byte-for-byte identical to what Upload would produce for the same
// content (the hash is of the whole file, and knows nothing about how it was
// chunked for transport, so history verification cannot tell the two paths
// apart), and what makes the reported progress run continuously from 0 to
// task.Size instead of restarting at every chunk boundary.
//
// Unlike s3, there is no abort call on failure: a Dropbox upload session that
// is never finished simply expires on its own (Dropbox documents a 48h
// window) and the partially uploaded data is discarded, so there is nothing
// for a cleanup path to do here.
func (p *Provider) UploadMultipart(ctx context.Context, task provider.UploadTask, partSize int64) (provider.UploadResult, error) {
	if partSize <= 0 {
		return provider.UploadResult{}, fmt.Errorf("dropbox: partSize must be positive")
	}
	partSize = clampPartSize(partSize)

	remotePath := joinDropboxPath(p.cfg.RootPath, task.RemotePath)
	if remotePath == "" {
		return provider.UploadResult{}, fmt.Errorf("dropbox: remote path %q has no file name", task.RemotePath)
	}

	h := sha256.New()
	source := io.TeeReader(&streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}, h)

	// Each chunk has to be materialized in memory: Dropbox needs a
	// Content-Length on every upload_session request, and task.Reader is not
	// seekable (it is an already-wrapped stream), so its length cannot be
	// discovered without reading it. Only one chunk is ever held at a time -
	// the file as a whole is never buffered.
	buf := make([]byte, partSize)

	var (
		sessionID string
		committed int64 // bytes Dropbox has accepted so far, = the next cursor offset
	)

	for {
		n, readErr := io.ReadFull(source, buf)
		// A short or empty read is not a failure, it is the end of the
		// stream: this chunk is the last one and belongs in the finish
		// request.
		last := errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF)
		if readErr != nil && !last {
			return provider.UploadResult{}, fmt.Errorf("dropbox: chunked upload of %q: reading source at offset %d: %w", task.RemotePath, committed, readErr)
		}

		switch {
		case sessionID == "":
			id, err := p.startUploadSession(ctx, buf[:n])
			if err != nil {
				return provider.UploadResult{}, fmt.Errorf("dropbox: chunked upload of %q: starting session: %w", task.RemotePath, err)
			}
			sessionID = id
			committed += int64(n)
			if !last {
				continue
			}
			// The whole stream fit in the first chunk, which start already
			// sent - finish it with an empty body.
			n = 0
		case !last:
			if err := p.appendToUploadSession(ctx, sessionID, committed, buf[:n]); err != nil {
				return provider.UploadResult{}, fmt.Errorf("dropbox: chunked upload of %q: appending %d bytes at offset %d: %w", task.RemotePath, n, committed, err)
			}
			committed += int64(n)
			continue
		}

		pathDisplay, err := p.finishUploadSession(ctx, sessionID, committed, remotePath, buf[:n])
		if err != nil {
			return provider.UploadResult{}, fmt.Errorf("dropbox: chunked upload of %q: finishing session at offset %d: %w", task.RemotePath, committed, err)
		}

		return provider.UploadResult{
			RemotePath:   task.RemotePath,
			RemoteURL:    pathDisplay,
			ChecksumAlgo: checksumAlgo,
			Checksum:     hex.EncodeToString(h.Sum(nil)),
		}, nil
	}
}

// startUploadSession opens an upload session with chunk as its first body and
// returns the session ID every later request in the session must carry.
func (p *Provider) startUploadSession(ctx context.Context, chunk []byte) (string, error) {
	var resp struct {
		SessionID string `json:"session_id"`
	}
	if err := p.postContent(ctx, contentBaseURL+"/files/upload_session/start", uploadSessionStartArg{Close: false}, chunk, &resp); err != nil {
		return "", err
	}
	if resp.SessionID == "" {
		return "", fmt.Errorf("response contained no session_id")
	}
	return resp.SessionID, nil
}

// appendToUploadSession writes chunk at offset in an open session. Dropbox
// answers with 200 and an empty body, so there is nothing to decode.
func (p *Provider) appendToUploadSession(ctx context.Context, sessionID string, offset int64, chunk []byte) error {
	arg := uploadSessionAppendArg{Cursor: uploadSessionCursor{SessionID: sessionID, Offset: offset}, Close: false}
	return p.postContent(ctx, contentBaseURL+"/files/upload_session/append_v2", arg, chunk, nil)
}

// finishUploadSession writes the last chunk (possibly empty, when the file
// size is an exact multiple of the chunk size) and commits the session to
// remotePath, returning the path_display of the created file. The response
// shape is the same file metadata /files/upload returns.
func (p *Provider) finishUploadSession(ctx context.Context, sessionID string, offset int64, remotePath string, chunk []byte) (string, error) {
	arg := uploadSessionFinishArg{
		Cursor: uploadSessionCursor{SessionID: sessionID, Offset: offset},
		Commit: uploadAPIArg{Path: remotePath, Mode: "overwrite", Autorename: false, Mute: false},
	}
	var resp struct {
		PathDisplay string `json:"path_display"`
	}
	if err := p.postContent(ctx, contentBaseURL+"/files/upload_session/finish", arg, chunk, &resp); err != nil {
		return "", err
	}
	return resp.PathDisplay, nil
}

// postContent POSTs body to one of Dropbox's "content" endpoints with arg
// marshalled into the Dropbox-API-Arg header, decoding a 2xx response into
// respOut when respOut is non-nil (append_v2 answers with an empty body, so
// it passes nil). Errors are deliberately returned without a "dropbox: "
// prefix - the caller adds the phase context (start/append/finish) - which is
// the same division of labour callAPI uses for the JSON endpoints.
func (p *Provider) postContent(ctx context.Context, url string, arg any, body []byte, respOut any) error {
	argBytes, err := json.Marshal(arg)
	if err != nil {
		return fmt.Errorf("encoding API arg: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Dropbox-API-Arg", string(argBytes))

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIErr(resp.StatusCode, respBody)
	}
	if respOut != nil {
		if err := json.Unmarshal(respBody, respOut); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// downloadAPIArg is the JSON structure sent in the Dropbox-API-Arg header of
// the /files/download content endpoint.
type downloadAPIArg struct {
	Path string `json:"path"`
}

func (p *Provider) Download(ctx context.Context, task provider.DownloadTask) error {
	remotePath := joinDropboxPath(p.cfg.RootPath, task.RemotePath)

	argBytes, err := json.Marshal(downloadAPIArg{Path: remotePath})
	if err != nil {
		return fmt.Errorf("dropbox: download %q: encoding API arg: %w", task.RemotePath, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, contentBaseURL+"/files/download", nil)
	if err != nil {
		return fmt.Errorf("dropbox: download %q: %w", task.RemotePath, err)
	}
	req.Header.Set("Dropbox-API-Arg", string(argBytes))

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("dropbox: download %q: %w", task.RemotePath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("dropbox: download %q: %w", task.RemotePath, newAPIErr(resp.StatusCode, body))
	}

	writer := task.Writer
	if task.Progress != nil {
		var total int64
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
				total = n
			}
		}
		writer = &streamio.ProgressWriter{W: writer, Total: total, OnProgress: task.Progress}
	}

	if _, err := io.Copy(writer, resp.Body); err != nil {
		return fmt.Errorf("dropbox: download %q: %w", task.RemotePath, err)
	}
	return nil
}

type listFolderEntry struct {
	Tag            string `json:".tag"`
	Name           string `json:"name"`
	PathDisplay    string `json:"path_display"`
	Size           int64  `json:"size"`
	ServerModified string `json:"server_modified"`
}

type listFolderResponse struct {
	Entries []listFolderEntry `json:"entries"`
	Cursor  string            `json:"cursor"`
	HasMore bool              `json:"has_more"`
}

func (p *Provider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	normalized := joinDropboxPath(p.cfg.RootPath, remotePath)

	var resp listFolderResponse
	if err := p.callAPI(ctx, apiBaseURL+"/files/list_folder", struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}{Path: normalized, Recursive: false}, &resp); err != nil {
		return nil, fmt.Errorf("dropbox: list %q: %w", remotePath, err)
	}

	entries := entriesToRemote(resp.Entries)
	for resp.HasMore {
		cursor := resp.Cursor
		resp = listFolderResponse{}
		if err := p.callAPI(ctx, apiBaseURL+"/files/list_folder/continue", struct {
			Cursor string `json:"cursor"`
		}{Cursor: cursor}, &resp); err != nil {
			return nil, fmt.Errorf("dropbox: list %q: %w", remotePath, err)
		}
		entries = append(entries, entriesToRemote(resp.Entries)...)
	}
	return entries, nil
}

func entriesToRemote(in []listFolderEntry) []provider.RemoteEntry {
	out := make([]provider.RemoteEntry, 0, len(in))
	for _, e := range in {
		modTime, _ := time.Parse(time.RFC3339, e.ServerModified)
		out = append(out, provider.RemoteEntry{
			Path:    e.PathDisplay,
			Name:    e.Name,
			Size:    e.Size,
			IsDir:   e.Tag == "folder",
			ModTime: modTime,
		})
	}
	return out
}

// Delete removes the object at remotePath. Deleting an already-missing
// object is not treated as an error, matching this repo's idempotency
// convention (see e.g. internal/secrets.Store.Delete) - Dropbox reports this
// case as an HTTP 409 with a structured "path_lookup/not_found" error body.
func (p *Provider) Delete(ctx context.Context, remotePath string) error {
	err := p.callAPI(ctx, apiBaseURL+"/files/delete_v2", struct {
		Path string `json:"path"`
	}{Path: joinDropboxPath(p.cfg.RootPath, remotePath)}, nil)
	if err != nil {
		var aerr *apiErr
		if errors.As(err, &aerr) && aerr.notFound() {
			return nil
		}
		return fmt.Errorf("dropbox: delete %q: %w", remotePath, err)
	}
	return nil
}

// Exists implements provider.ExistenceChecker.
func (p *Provider) Exists(ctx context.Context, remotePath string) (bool, error) {
	err := p.callAPI(ctx, apiBaseURL+"/files/get_metadata", struct {
		Path string `json:"path"`
	}{Path: joinDropboxPath(p.cfg.RootPath, remotePath)}, nil)
	if err != nil {
		var aerr *apiErr
		if errors.As(err, &aerr) && aerr.notFound() {
			return false, nil
		}
		return false, fmt.Errorf("dropbox: stat %q: %w", remotePath, err)
	}
	return true, nil
}

// VerifyChecksum implements provider.ChecksumVerifier by re-downloading the
// object and recomputing the same self-computed SHA-256 Upload produced -
// see the checksumAlgo doc comment. Reuses Download rather than duplicating
// the HTTP call, writing straight into the hasher instead of a real
// destination file.
func (p *Provider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	if algo != checksumAlgo {
		return false, fmt.Errorf("dropbox: cannot verify checksum of algo %q", algo)
	}

	h := sha256.New()
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: remotePath, Writer: h}); err != nil {
		return false, fmt.Errorf("dropbox: verify %q: %w", remotePath, err)
	}
	return hex.EncodeToString(h.Sum(nil)) == checksum, nil
}

// ConfigFields implements provider.ConfigSchema. Deliberately excludes the
// OAuth Client ID/Secret (app-wide, configured once - see the package doc
// comment) and the refresh token (per-connection, but written directly by
// the caller that invoked Authorize, not typed into a form) - same
// reasoning as googledrive's ConfigFields.
func (p *Provider) ConfigFields() []provider.FieldSpec {
	return []provider.FieldSpec{
		{Key: "rootPath", Label: "Subfolder in your app folder to upload into, optional (e.g. backups/cloudup) - leave empty for the app folder root", Type: provider.FieldText},
	}
}

// joinDropboxPath returns the absolute Dropbox path for path, prefixed by
// root (this connection's configured RootPath, if any) - mirrors
// googledrive's FolderID acting as a base for every operation. Dropbox
// paths must start with "/"; an empty result (both root and path empty)
// means the Dropbox root folder, which is what files/list_folder expects
// for "".
func joinDropboxPath(root, path string) string {
	root = strings.Trim(root, "/")
	path = strings.Trim(path, "/")
	switch {
	case root == "" && path == "":
		return ""
	case path == "":
		return "/" + root
	case root == "":
		return "/" + path
	default:
		return "/" + root + "/" + path
	}
}

// callAPI POSTs reqBody (or a literal JSON null if reqBody is nil, as
// Dropbox's no-argument endpoints like get_current_account require) as JSON
// to url and decodes a 2xx response body into respOut (if non-nil). A
// non-2xx response is turned into an *apiErr - see newAPIErr and apiErr.notFound,
// used consistently by every JSON API call in this file instead of
// duplicating this parsing per call site.
func (p *Provider) callAPI(ctx context.Context, url string, reqBody any, respOut any) error {
	var bodyReader io.Reader = strings.NewReader("null")
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

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
	if respOut != nil {
		if err := json.Unmarshal(body, respOut); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// apiErr represents a non-2xx Dropbox API JSON error response.
type apiErr struct {
	StatusCode int
	Summary    string
	Raw        string
}

// newAPIErr parses body (a non-2xx Dropbox API response body) into an
// *apiErr. If body isn't the expected {"error_summary": "..."} shape,
// Summary stays empty and Error() falls back to reporting the raw body.
func newAPIErr(statusCode int, body []byte) *apiErr {
	var parsed struct {
		ErrorSummary string `json:"error_summary"`
	}
	_ = json.Unmarshal(body, &parsed)
	return &apiErr{StatusCode: statusCode, Summary: parsed.ErrorSummary, Raw: strings.TrimSpace(string(body))}
}

func (e *apiErr) Error() string {
	if e.Summary != "" {
		return fmt.Sprintf("%s (status %d)", e.Summary, e.StatusCode)
	}
	return fmt.Sprintf("unexpected status %d: %s", e.StatusCode, e.Raw)
}

// notFound reports whether this error is Dropbox's convention for "the
// given path does not exist" (delete_v2/get_metadata return HTTP 409 with
// an error_summary like "path_lookup/not_found/..." in that case).
func (e *apiErr) notFound() bool {
	return strings.Contains(e.Summary, "not_found")
}
