// Package b2 implements provider.Provider over Backblaze B2's native
// b2api v2 REST API - not the S3-compatible layer B2 also exposes (which
// this project already covers generically via internal/providers/s3 with
// a custom endpoint). The point of a dedicated native package is the same
// reason internal/providers/googledrive exists instead of routing Google
// Drive through some generic HTTP layer: B2 reports a reliable
// server-native SHA-1 for every uploaded file (the b2_list_file_names
// response's contentSha1 field), so VerifyChecksum here is a cheap
// metadata lookup instead of the re-download-and-rehash approach
// webdav/s3 are forced into because their protocols don't expose a
// trustworthy hash.
package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloudup/internal/debuglog"
	"cloudup/internal/provider"
	"cloudup/internal/registry"
	"cloudup/internal/streamio"
)

const Type = "b2"

const (
	secretKeyID          = "keyId"
	secretApplicationKey = "applicationKey"
)

// checksumAlgo is the label stored in UploadResult/upload_log. Unlike
// WebDAV/S3, B2 computes and stores a SHA-1 for every file itself (either
// supplied by the uploader, as here, or computed server-side) and serves
// it back cheaply from b2_list_file_names - see the package doc comment.
const checksumAlgo = "sha1"

// authorizeURL is the b2_authorize_account endpoint. It is a package
// variable (rather than a literal used directly) purely so tests can
// redirect it to a local fake B2 server, the same pattern googledrive
// uses for oauthEndpoint.
var authorizeURL = "https://api.backblazeb2.com/b2api/v2/b2_authorize_account"

// errUnauthorized is returned internally by the low-level HTTP helpers
// when a B2 API call comes back 401, so the exported methods above them
// can decide whether to re-authorize and retry. It never escapes a public
// method - callers always see it wrapped with more context (or, once a
// retry has been attempted, whatever the retry itself returned), which is
// also why its own text carries no "b2: " prefix.
var errUnauthorized = errors.New("unauthorized")

// newHTTPClient builds the Provider's own http.Client instead of reusing
// http.DefaultClient. Two reasons: DefaultClient is process-global, so any
// setting made here would leak into every other HTTP user in the process
// (and vice versa), and it has no timeouts whatsoever, so a wedged
// connection would hang a queue worker forever.
//
// There is deliberately no http.Client.Timeout: that bounds the whole
// exchange including the body, which would abort a legitimately slow
// multi-gigabyte upload. The timeouts set here bound only the phases that
// should never take long (connect, TLS, waiting for response headers),
// leaving "how long may a transfer take" to the caller's context and to
// internal/queue.Manager's retry policy above this layer.
func newHTTPClient() *http.Client {
	// debuglog.Transport wraps (rather than replaces) the tuned transport
	// below: it is a transparent passthrough unless CLOUDUP_DEBUG is set, so
	// wiring it unconditionally costs nothing and means `CLOUDUP_DEBUG=1`
	// actually covers this provider - see internal/debuglog.
	return &http.Client{
		Transport: debuglog.Transport{RT: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
		}},
	}
}

func init() {
	registry.Register(Type, New)
	registry.RegisterSchema(Type, (&Provider{}).ConfigFields)
}

// Config is the non-secret part of a B2 connection, persisted in the JSON
// config file. The application key ID/secret live in the secret store.
type Config struct {
	ConnectionID string `json:"connectionId"`
	BucketName   string `json:"bucketName"`
	DisplayName  string `json:"displayName"`
}

// Provider implements provider.Provider (and several optional feature
// interfaces) over the Backblaze B2 native API.
//
// Unlike WebDAV/S3 (stateless per-request auth) and more like
// googledrive's OAuth token, a B2 session is obtained once via
// b2_authorize_account and then reused for many calls until it expires or
// is rejected. New deliberately does not call b2_authorize_account itself
// (same "no eager network calls in the factory" principle as
// googledrive.New) - session state is acquired lazily by ensureSession,
// called at the top of every exported method that needs the API, and
// guarded by mu since a queue.Manager may drive the same Provider from
// multiple goroutines concurrently.
type Provider struct {
	cfg    Config
	keyID  string
	appKey string

	httpClient *http.Client

	mu          sync.Mutex
	apiURL      string
	authToken   string
	downloadURL string
	accountID   string
	bucketID    string
}

// New is the registry.Factory for the "b2" provider type. It reads the
// keyId/applicationKey secrets but does not use them yet - see the
// Provider doc comment.
func New(rawCfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
	var cfg Config
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		return nil, fmt.Errorf("b2: invalid config: %w", err)
	}
	if cfg.BucketName == "" {
		return nil, fmt.Errorf("b2: bucketName is required")
	}

	keyID, err := secrets.Get(cfg.ConnectionID, secretKeyID)
	if err != nil {
		return nil, fmt.Errorf("b2: reading keyId secret: %w", err)
	}
	if keyID == "" {
		return nil, fmt.Errorf("b2: keyId is required")
	}
	appKey, err := secrets.Get(cfg.ConnectionID, secretApplicationKey)
	if err != nil {
		return nil, fmt.Errorf("b2: reading applicationKey secret: %w", err)
	}
	if appKey == "" {
		return nil, fmt.Errorf("b2: applicationKey is required")
	}

	return &Provider{
		cfg:        cfg,
		keyID:      keyID,
		appKey:     appKey,
		httpClient: newHTTPClient(),
	}, nil
}

func (p *Provider) Type() string { return Type }

func (p *Provider) DisplayName() string {
	if p.cfg.DisplayName != "" {
		return p.cfg.DisplayName
	}
	return "Backblaze B2 (" + p.cfg.BucketName + ")"
}

// ConfigFields implements provider.ConfigSchema.
func (p *Provider) ConfigFields() []provider.FieldSpec {
	return []provider.FieldSpec{
		{Key: "bucketName", Label: "Bucket name", Type: provider.FieldText, Required: true},
		{Key: secretKeyID, Label: "Key ID", Type: provider.FieldPassword, Required: true},
		{Key: secretApplicationKey, Label: "Application Key", Type: provider.FieldPassword, Required: true},
	}
}

func (p *Provider) TestConnection(ctx context.Context) error {
	if err := p.ensureSession(ctx); err != nil {
		return fmt.Errorf("b2: connection test failed: %w", err)
	}
	return nil
}

// Upload implements the B2 native upload flow: get a short-lived upload
// URL/token for the bucket, then POST the file to it with the trailing
// SHA-1 trick (see sha1TrailerReader) so the checksum is computed while
// streaming, with no second read of task.Reader.
//
// Retry scope: if b2_get_upload_url comes back 401 (the main session
// token expired), this re-authorizes and retries that step once - cheap,
// since no request body has been sent yet. If the upload POST itself
// comes back 401 (the upload-specific token expired mid-flight, which B2
// documents as possible independently of the main session), this does
// *not* retry: task.Reader has already been partially consumed and, being
// a general io.Reader, cannot be safely rewound to redo the upload from
// byte zero. internal/queue.Manager already retries failed uploads at a
// higher level (from a fresh reader), which is the right place to recover
// from this rare case rather than building reader-buffering/replay logic
// here for an MVP.
func (p *Provider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	if err := p.ensureSession(ctx); err != nil {
		return provider.UploadResult{}, fmt.Errorf("b2: upload %q: %w", task.RemotePath, err)
	}

	uploadURL, uploadToken, err := p.getUploadURL(ctx)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("b2: upload %q: %w", task.RemotePath, err)
	}

	result, err := p.doUpload(ctx, uploadURL, uploadToken, task)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("b2: upload %q: %w", task.RemotePath, err)
	}
	return result, nil
}

func (p *Provider) getUploadURL(ctx context.Context) (string, string, error) {
	var uploadURL, uploadToken string
	err := p.withReauth(ctx, func() error {
		var err error
		uploadURL, uploadToken, err = p.requestUploadURL(ctx)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return uploadURL, uploadToken, nil
}

func (p *Provider) requestUploadURL(ctx context.Context) (string, string, error) {
	_, _, _, bucketID := p.session()

	var out struct {
		UploadURL          string `json:"uploadUrl"`
		AuthorizationToken string `json:"authorizationToken"`
	}
	if err := p.postJSON(ctx, "/b2api/v2/b2_get_upload_url", map[string]string{"bucketId": bucketID}, &out); err != nil {
		return "", "", err
	}
	return out.UploadURL, out.AuthorizationToken, nil
}

func (p *Provider) doUpload(ctx context.Context, uploadURL, uploadToken string, task provider.UploadTask) (provider.UploadResult, error) {
	reader, err := exactSizeBody(task.Reader, task.Size)
	if err != nil {
		return provider.UploadResult{}, err
	}
	progress := &streamio.ProgressReader{R: reader, Total: task.Size, OnProgress: task.Progress}
	body := newSHA1TrailerReader(progress)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, body)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("building upload request: %w", err)
	}
	req.Header.Set("Authorization", uploadToken)
	req.Header.Set("X-Bz-File-Name", escapeB2Path(task.RemotePath))
	req.Header.Set("Content-Type", "b2/x-auto")
	req.Header.Set("X-Bz-Content-Sha1", "hex_digits_at_end")
	// 40 = the hex SHA-1 trailer sha1TrailerReader appends; B2 requires an
	// exact Content-Length up front, which is why exactSizeBody above
	// refuses a task.Size that cannot be the real length.
	req.ContentLength = task.Size + sha1TrailerLen

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("uploading: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return provider.UploadResult{}, unauthorizedError(resp)
	}
	if resp.StatusCode/100 != 2 {
		return provider.UploadResult{}, parseB2Error(resp)
	}

	var out struct {
		FileID      string `json:"fileId"`
		FileName    string `json:"fileName"`
		ContentSha1 string `json:"contentSha1"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return provider.UploadResult{}, fmt.Errorf("decoding upload response: %w", err)
	}

	_, _, downloadURL, _ := p.session()
	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    downloadURL + "/file/" + p.cfg.BucketName + "/" + escapeB2Path(task.RemotePath),
		ChecksumAlgo: checksumAlgo,
		Checksum:     out.ContentSha1,
	}, nil
}

// sha1TrailerLen is the number of bytes newSHA1TrailerReader appends to
// the body: a SHA-1 digest as lowercase hex.
const sha1TrailerLen = 40

// exactSizeBody guards the one assumption doUpload cannot verify on its
// own: that task.Size really is the exact byte length of task.Reader.
// B2's "hex_digits_at_end" contract needs Content-Length declared up front
// (Size + sha1TrailerLen), so a wrong Size is not a cosmetic problem - the
// upload either fails cryptically in net/http's transfer writer or, worse,
// could store a truncated object.
//
// A negative Size is nonsensical and rejected outright. Size == 0 is
// ambiguous: it is legitimate for a genuinely empty file (which B2 accepts,
// and which the trailer contract handles fine), but it is also what a
// caller would pass for an unknown-length stream. The two are told apart
// by reading a single byte: if one arrives, Size was a lie, and failing
// here with a clear message beats silently uploading 40 bytes of trailer
// as the whole object. Nothing is lost by consuming that byte: either the
// stream was empty, or this call fails and the reader is discarded.
func exactSizeBody(r io.Reader, size int64) (io.Reader, error) {
	if size < 0 {
		return nil, fmt.Errorf("negative size %d", size)
	}
	if size > 0 {
		return r, nil
	}
	// io.ReadFull rather than a bare Read: a Reader may legally return
	// (0, nil) without being at EOF, which a single Read couldn't tell
	// apart from an empty source.
	var probe [1]byte
	n, err := io.ReadFull(r, probe[:])
	if n > 0 {
		return nil, fmt.Errorf("size is 0 but the source is not empty: B2 needs the exact length before the upload starts, so unknown-length streams are not supported")
	}
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("reading source: %w", err)
	}
	return r, nil
}

// b2MinPartSize is the smallest size b2_upload_part accepts for every part
// except the last one - B2's documented absolute minimum. It is a package
// variable, not a constant, purely so tests can shrink it and exercise the
// chunking loop with byte-sized payloads instead of megabytes - same trick
// as dropbox's chunkGranularity.
var b2MinPartSize int64 = 5_000_000

// clampPartSize raises partSize to at least b2MinPartSize: internal/queue
// picks a generic part size with no knowledge of B2's own minimum, so
// silently sending a smaller value would fail only after the first
// b2_upload_part call, mid-session, with some parts already stored.
func clampPartSize(partSize int64) int64 {
	if partSize < b2MinPartSize {
		return b2MinPartSize
	}
	return partSize
}

// UploadMultipart implements provider.MultipartUploader using B2's native
// large-file API (b2_start_large_file / b2_get_upload_part_url /
// b2_upload_part / b2_finish_large_file) - the counterpart to the
// single-request Upload above, needed because a single b2_upload_file
// request has no documented size ceiling in principle but in practice
// times out or is impractical for very large files, exactly like S3's and
// Dropbox's single-request endpoints.
//
// Unlike Upload, which streams task.Reader straight through with the
// SHA-1-trailer trick to avoid buffering, each part here is buffered in
// memory first (one partSize buffer, reused - never the whole file): B2
// needs a real (non-trailer) X-Bz-Content-Sha1 header and exact
// Content-Length before a part's body starts. Buffering also means a part
// whose upload token has expired can simply be resent with a fresh one
// (see the retry around doUploadPart below) - not possible for the
// trailer-based single-request path, whose body has already been
// partially consumed by the time a 401 could come back.
//
// Checksum parity with Upload: B2 reports "none" for a large file's own
// contentSha1 unless the whole-file SHA-1 is supplied as fileInfo when the
// large file is *started* - which isn't knowable in advance from a single
// streamed pass over task.Reader. So the value returned here still is the
// correct whole-file SHA-1 (task.Reader is wrapped in exactly one
// TeeReader before being sliced into parts, same as Upload and the same
// technique s3/dropbox use for their own multipart paths), but B2 itself
// won't serve it back cheaply the way it does for single-part uploads -
// VerifyChecksum falls back to re-download-and-rehash for exactly the
// files where the native lookup comes back "none".
func (p *Provider) UploadMultipart(ctx context.Context, task provider.UploadTask, partSize int64) (provider.UploadResult, error) {
	if partSize <= 0 {
		return provider.UploadResult{}, fmt.Errorf("b2: partSize must be positive")
	}
	partSize = clampPartSize(partSize)

	if err := p.ensureSession(ctx); err != nil {
		return provider.UploadResult{}, fmt.Errorf("b2: multipart upload %q: %w", task.RemotePath, err)
	}

	fileID, err := p.startLargeFile(ctx, task.RemotePath)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("b2: multipart upload %q: starting large file: %w", task.RemotePath, err)
	}
	cancel := func() {
		// Best-effort cleanup so an aborted upload doesn't keep incurring
		// B2 storage cost for parts already accepted - mirrors
		// s3.UploadMultipart's own abort() on failure. Uses a background
		// context since ctx may already be the reason this failed.
		_ = p.postJSON(context.Background(), "/b2api/v2/b2_cancel_large_file", map[string]string{"fileId": fileID}, nil)
	}

	partUploadURL, partUploadToken, err := p.getPartUploadURL(ctx, fileID)
	if err != nil {
		cancel()
		return provider.UploadResult{}, fmt.Errorf("b2: multipart upload %q: %w", task.RemotePath, err)
	}

	h := sha1.New()
	source := io.TeeReader(&streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}, h)

	buf := make([]byte, partSize)
	var partSha1s []string
	partNumber := 1

	for {
		n, readErr := io.ReadFull(source, buf)
		last := errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF)
		if readErr != nil && !last {
			cancel()
			return provider.UploadResult{}, fmt.Errorf("b2: multipart upload %q: reading source for part %d: %w", task.RemotePath, partNumber, readErr)
		}

		if n > 0 {
			sha1sum, err := p.doUploadPart(ctx, partUploadURL, partUploadToken, partNumber, buf[:n])
			if errors.Is(err, errUnauthorized) {
				// The part-specific upload token expired. Unlike Upload's
				// body, buf[:n] is still fully in memory, so fetching a
				// fresh URL/token and resending this exact part is safe.
				partUploadURL, partUploadToken, err = p.getPartUploadURL(ctx, fileID)
				if err == nil {
					sha1sum, err = p.doUploadPart(ctx, partUploadURL, partUploadToken, partNumber, buf[:n])
				}
			}
			if err != nil {
				cancel()
				return provider.UploadResult{}, fmt.Errorf("b2: multipart upload %q: uploading part %d: %w", task.RemotePath, partNumber, err)
			}
			partSha1s = append(partSha1s, sha1sum)
			partNumber++
		}
		if last {
			break
		}
	}

	if len(partSha1s) == 0 {
		// internal/queue only reaches UploadMultipart for files above
		// DefaultMultipartThreshold, so an empty task.Reader cannot occur
		// through normal use - this is a direct-call guard, not a real
		// code path, documented rather than special-cased below.
		cancel()
		return provider.UploadResult{}, fmt.Errorf("b2: multipart upload %q: source is empty, the large-file API requires at least one part", task.RemotePath)
	}

	if err := p.finishLargeFile(ctx, fileID, partSha1s); err != nil {
		cancel()
		return provider.UploadResult{}, fmt.Errorf("b2: multipart upload %q: finishing large file: %w", task.RemotePath, err)
	}

	_, _, downloadURL, _ := p.session()
	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    downloadURL + "/file/" + p.cfg.BucketName + "/" + escapeB2Path(task.RemotePath),
		ChecksumAlgo: checksumAlgo,
		Checksum:     hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// startLargeFile calls b2_start_large_file and returns the fileId every
// later call in the session (get_upload_part_url/finish/cancel) must carry.
// fileName is sent unescaped, like the JSON body of doDeleteFileVersion/
// doListFileNames - the percent-encoding escapeB2Path does is only needed
// for the X-Bz-File-Name HTTP header the single-request Upload path sends.
func (p *Provider) startLargeFile(ctx context.Context, remotePath string) (string, error) {
	var fileID string
	err := p.withReauth(ctx, func() error {
		_, _, _, bucketID := p.session()
		var out struct {
			FileID string `json:"fileId"`
		}
		if err := p.postJSON(ctx, "/b2api/v2/b2_start_large_file", map[string]string{
			"bucketId":    bucketID,
			"fileName":    remotePath,
			"contentType": "b2/x-auto",
		}, &out); err != nil {
			return err
		}
		fileID = out.FileID
		return nil
	})
	return fileID, err
}

// getPartUploadURL calls b2_get_upload_part_url, which - like
// b2_get_upload_url for the single-request path - hands back a short-lived
// URL/token pair scoped to this one large-file session.
func (p *Provider) getPartUploadURL(ctx context.Context, fileID string) (string, string, error) {
	var uploadURL, uploadToken string
	err := p.withReauth(ctx, func() error {
		var out struct {
			UploadURL          string `json:"uploadUrl"`
			AuthorizationToken string `json:"authorizationToken"`
		}
		if err := p.postJSON(ctx, "/b2api/v2/b2_get_upload_part_url", map[string]string{"fileId": fileID}, &out); err != nil {
			return err
		}
		uploadURL, uploadToken = out.UploadURL, out.AuthorizationToken
		return nil
	})
	return uploadURL, uploadToken, err
}

// doUploadPart POSTs one already-buffered part to uploadURL. Unlike
// doUpload's SHA-1-trailer trick, chunk's length and hash are both already
// known, so this sends a normal exact Content-Length and a real
// X-Bz-Content-Sha1 header up front. Returns errUnauthorized on 401 rather
// than retrying itself, so UploadMultipart's caller can decide whether a
// fresh URL/token is worth fetching - this function never rewinds chunk.
func (p *Provider) doUploadPart(ctx context.Context, uploadURL, uploadToken string, partNumber int, chunk []byte) (string, error) {
	sum := sha1.Sum(chunk)
	sha1hex := hex.EncodeToString(sum[:])

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(chunk))
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", uploadToken)
	req.Header.Set("X-Bz-Part-Number", strconv.Itoa(partNumber))
	req.Header.Set("X-Bz-Content-Sha1", sha1hex)
	req.ContentLength = int64(len(chunk))

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("uploading part %d: %w", partNumber, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", unauthorizedError(resp)
	}
	if resp.StatusCode/100 != 2 {
		return "", parseB2Error(resp)
	}

	var out struct {
		ContentSha1 string `json:"contentSha1"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding upload_part response: %w", err)
	}
	return out.ContentSha1, nil
}

// finishLargeFile calls b2_finish_large_file with the SHA-1 of every part
// in order, committing the session to a real object.
func (p *Provider) finishLargeFile(ctx context.Context, fileID string, partSha1s []string) error {
	return p.withReauth(ctx, func() error {
		return p.postJSON(ctx, "/b2api/v2/b2_finish_large_file", map[string]any{
			"fileId":        fileID,
			"partSha1Array": partSha1s,
		}, nil)
	})
}

// UploadMultipartParallel implements provider.ParallelMultipartUploader.
// Unlike UploadMultipart's sequential loop, B2 parts may be uploaded in any
// order and concurrently - officially documented B2 behavior - so this
// dispatches part uploads across up to streams goroutines via
// streamio.UploadPartsConcurrently, reading each part straight from
// task.ReaderAt instead of task.Reader.
//
// One B2-specific wrinkle absent from s3.UploadMultipartParallel: every
// concurrent uploader needs its *own* upload-part URL/token, obtained via
// its own b2_get_upload_part_url call. B2 rejects concurrent requests
// against the same token with a documented auth_token_limit error, unlike
// S3 (no such per-URL exclusivity) - so each worker below fetches its own
// URL/token rather than sharing the single one UploadMultipart's sequential
// loop reuses across all its parts.
//
// Checksum parity: exactly the s3.UploadMultipartParallel approach - a
// dedicated goroutine makes its own ordinary sequential pass over
// io.NewSectionReader(task.ReaderAt, 0, task.Size) to compute the SHA-1,
// concurrently with the part uploads, relying on task.ReaderAt backing onto
// a real *os.File (safe for concurrent ReadAt calls). VerifyChecksum's
// existing "none"-contentSha1 fallback (re-download-and-rehash) already
// covers files uploaded this way, same as the sequential multipart path.
func (p *Provider) UploadMultipartParallel(ctx context.Context, task provider.UploadTask, partSize int64, streams int) (provider.UploadResult, error) {
	if partSize <= 0 {
		return provider.UploadResult{}, fmt.Errorf("b2: partSize must be positive")
	}
	if task.ReaderAt == nil {
		return provider.UploadResult{}, fmt.Errorf("b2: parallel multipart upload requires a random-access source")
	}
	partSize = clampPartSize(partSize)

	if err := p.ensureSession(ctx); err != nil {
		return provider.UploadResult{}, fmt.Errorf("b2: parallel multipart upload %q: %w", task.RemotePath, err)
	}

	fileID, err := p.startLargeFile(ctx, task.RemotePath)
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("b2: parallel multipart upload %q: starting large file: %w", task.RemotePath, err)
	}
	cancel := func() {
		_ = p.postJSON(context.Background(), "/b2api/v2/b2_cancel_large_file", map[string]string{"fileId": fileID}, nil)
	}

	hashDone := make(chan error, 1)
	var sum []byte
	go func() {
		h := sha1.New()
		if _, err := io.Copy(h, io.NewSectionReader(task.ReaderAt, 0, task.Size)); err != nil {
			hashDone <- err
			return
		}
		sum = h.Sum(nil)
		hashDone <- nil
	}()

	progress := streamio.NewAtomicProgress(task.Size, task.Progress)
	partSha1s, err := streamio.UploadPartsConcurrently(ctx, task.ReaderAt, task.Size, partSize, streams,
		func(ctx context.Context, part streamio.Part, r io.Reader) (string, error) {
			// The chunk has to be fully buffered before it can be sent: B2
			// needs a real (non-trailer) X-Bz-Content-Sha1 header computed
			// from the whole part up front - see doUploadPart.
			buf, err := io.ReadAll(progress.Reader(r))
			if err != nil {
				return "", fmt.Errorf("reading part %d: %w", part.Number, err)
			}

			uploadURL, uploadToken, err := p.getPartUploadURL(ctx, fileID)
			if err != nil {
				return "", err
			}
			sha1sum, err := p.doUploadPart(ctx, uploadURL, uploadToken, part.Number, buf)
			if errors.Is(err, errUnauthorized) {
				uploadURL, uploadToken, err = p.getPartUploadURL(ctx, fileID)
				if err == nil {
					sha1sum, err = p.doUploadPart(ctx, uploadURL, uploadToken, part.Number, buf)
				}
			}
			return sha1sum, err
		})
	if err != nil {
		cancel()
		return provider.UploadResult{}, fmt.Errorf("b2: parallel multipart upload %q: %w", task.RemotePath, err)
	}

	if err := <-hashDone; err != nil {
		cancel()
		return provider.UploadResult{}, fmt.Errorf("b2: parallel multipart upload %q: hashing source: %w", task.RemotePath, err)
	}

	if len(partSha1s) == 0 {
		cancel()
		return provider.UploadResult{}, fmt.Errorf("b2: parallel multipart upload %q: source is empty, the large-file API requires at least one part", task.RemotePath)
	}

	if err := p.finishLargeFile(ctx, fileID, partSha1s); err != nil {
		cancel()
		return provider.UploadResult{}, fmt.Errorf("b2: parallel multipart upload %q: finishing large file: %w", task.RemotePath, err)
	}

	_, _, downloadURL, _ := p.session()
	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    downloadURL + "/file/" + p.cfg.BucketName + "/" + escapeB2Path(task.RemotePath),
		ChecksumAlgo: checksumAlgo,
		Checksum:     hex.EncodeToString(sum),
	}, nil
}

// Download fetches the object via B2's download-by-name URL, using the
// main session token (unlike Upload, downloads from a private bucket
// don't need a separate short-lived token).
func (p *Provider) Download(ctx context.Context, task provider.DownloadTask) error {
	if err := p.ensureSession(ctx); err != nil {
		return fmt.Errorf("b2: download %q: %w", task.RemotePath, err)
	}

	if err := p.withReauth(ctx, func() error { return p.doDownload(ctx, task) }); err != nil {
		return fmt.Errorf("b2: download %q: %w", task.RemotePath, err)
	}
	return nil
}

func (p *Provider) doDownload(ctx context.Context, task provider.DownloadTask) error {
	_, authToken, downloadURL, _ := p.session()
	fileURL := downloadURL + "/file/" + p.cfg.BucketName + "/" + escapeB2Path(task.RemotePath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", authToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return unauthorizedError(resp)
	}
	if resp.StatusCode/100 != 2 {
		return parseB2Error(resp)
	}

	writer := task.Writer
	if task.Progress != nil {
		writer = &streamio.ProgressWriter{W: writer, Total: resp.ContentLength, OnProgress: task.Progress}
	}
	if _, err := io.Copy(writer, resp.Body); err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}
	return nil
}

// listFileNamesResponse mirrors the relevant fields of b2_list_file_names'
// JSON response - see https://www.backblaze.com/apidocs/b2-list-file-names.
type listFileNamesResponse struct {
	Files []struct {
		FileID          string `json:"fileId"`
		FileName        string `json:"fileName"`
		ContentLength   int64  `json:"contentLength"`
		ContentSha1     string `json:"contentSha1"`
		UploadTimestamp int64  `json:"uploadTimestamp"`
		Action          string `json:"action"`
	} `json:"files"`
	NextFileName string `json:"nextFileName"`
	NextFileID   string `json:"nextFileId"`
}

// List enumerates one "level" of remotePath by asking B2 for a "/"
// delimiter, which makes it return real files as "upload" entries and
// synthetic one-level-deep sub-prefixes as "folder" entries, instead of a
// flat recursive listing of the whole bucket.
func (p *Provider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	if err := p.ensureSession(ctx); err != nil {
		return nil, fmt.Errorf("b2: list %q: %w", remotePath, err)
	}

	prefix := normalizeListPrefix(remotePath)
	var entries []provider.RemoteEntry
	startName, startID := "", ""
	for {
		resp, err := p.listFileNames(ctx, prefix, 1000, "/", startName, startID)
		if err != nil {
			return nil, fmt.Errorf("b2: list %q: %w", remotePath, err)
		}
		for _, f := range resp.Files {
			switch f.Action {
			case "folder":
				name := strings.TrimSuffix(f.FileName, "/")
				entries = append(entries, provider.RemoteEntry{
					Path:  name,
					Name:  path.Base(name),
					IsDir: true,
				})
			case "upload":
				entries = append(entries, provider.RemoteEntry{
					Path:    f.FileName,
					Name:    path.Base(f.FileName),
					Size:    f.ContentLength,
					ModTime: time.UnixMilli(f.UploadTimestamp),
				})
			}
		}
		if resp.NextFileName == "" && resp.NextFileID == "" {
			break
		}
		startName, startID = resp.NextFileName, resp.NextFileID
	}
	return entries, nil
}

func (p *Provider) listFileNames(ctx context.Context, prefix string, maxCount int, delimiter, startName, startID string) (*listFileNamesResponse, error) {
	var resp *listFileNamesResponse
	err := p.withReauth(ctx, func() error {
		var err error
		resp, err = p.doListFileNames(ctx, prefix, maxCount, delimiter, startName, startID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *Provider) doListFileNames(ctx context.Context, prefix string, maxCount int, delimiter, startName, startID string) (*listFileNamesResponse, error) {
	_, _, _, bucketID := p.session()

	reqBody := map[string]any{
		"bucketId":     bucketID,
		"prefix":       prefix,
		"maxFileCount": maxCount,
	}
	if delimiter != "" {
		reqBody["delimiter"] = delimiter
	}
	if startName != "" {
		reqBody["startFileName"] = startName
	}
	if startID != "" {
		reqBody["startFileId"] = startID
	}
	var out listFileNamesResponse
	if err := p.postJSON(ctx, "/b2api/v2/b2_list_file_names", reqBody, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// findFile looks up the exact object at remotePath (no delimiter, so it
// matches a real file rather than a folder prefix) and returns its
// current fileId/contentSha1. It is shared by Delete, Exists and
// VerifyChecksum so the b2_list_file_names lookup - the only way B2
// exposes "does this exact name exist" - isn't duplicated three times.
func (p *Provider) findFile(ctx context.Context, remotePath string) (fileID, sha1sum string, found bool, err error) {
	if err := p.ensureSession(ctx); err != nil {
		return "", "", false, err
	}
	resp, err := p.listFileNames(ctx, remotePath, 1, "", "", "")
	if err != nil {
		return "", "", false, err
	}
	if len(resp.Files) == 0 {
		return "", "", false, nil
	}
	f := resp.Files[0]
	if f.FileName != remotePath {
		// b2_list_file_names with this prefix returns the first name at
		// or after remotePath lexicographically - if it isn't an exact
		// match, remotePath itself doesn't exist.
		return "", "", false, nil
	}
	return f.FileID, f.ContentSha1, true, nil
}

// Delete removes the current version of the object at remotePath.
//
// Deleting an already-missing object is not an error in this codebase -
// see internal/secrets.Store.Delete for the same idempotency principle -
// so a missing file is treated as success, not as a "not found" error.
//
// B2 supports multiple versions per file name (b2_delete_file_version
// requires an explicit fileId precisely because of this) and this only
// deletes the single version findFile's lookup returned, which is
// normally the most recent one; it does not enumerate and delete every
// historical version. That is an intentionally scoped gap for this MVP,
// the same kind of documented gap internal/providers/googledrive leaves
// around integration test coverage.
func (p *Provider) Delete(ctx context.Context, remotePath string) error {
	fileID, _, found, err := p.findFile(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("b2: delete %q: %w", remotePath, err)
	}
	if !found {
		return nil
	}
	if err := p.deleteFileVersion(ctx, remotePath, fileID); err != nil {
		return fmt.Errorf("b2: delete %q: %w", remotePath, err)
	}
	return nil
}

func (p *Provider) deleteFileVersion(ctx context.Context, fileName, fileID string) error {
	return p.withReauth(ctx, func() error { return p.doDeleteFileVersion(ctx, fileName, fileID) })
}

func (p *Provider) doDeleteFileVersion(ctx context.Context, fileName, fileID string) error {
	// nil respOut: b2_delete_file_version echoes back the fileId/fileName
	// we already know, so there is nothing worth decoding.
	return p.postJSON(ctx, "/b2api/v2/b2_delete_file_version",
		map[string]string{"fileName": fileName, "fileId": fileID}, nil)
}

// Exists implements provider.ExistenceChecker.
func (p *Provider) Exists(ctx context.Context, remotePath string) (bool, error) {
	_, _, found, err := p.findFile(ctx, remotePath)
	if err != nil {
		return false, fmt.Errorf("b2: stat %q: %w", remotePath, err)
	}
	return found, nil
}

// b2NoContentSha1 is what b2_list_file_names reports for a large file's
// contentSha1 when it wasn't supplied as fileInfo at b2_start_large_file
// time - always true for files this package uploads via UploadMultipart,
// since the whole-file hash isn't known until the last part has streamed
// by. See VerifyChecksum for how this is handled.
const b2NoContentSha1 = "none"

// VerifyChecksum implements provider.ChecksumVerifier. For an ordinary
// single-request upload it reads the object's contentSha1 back from
// b2_list_file_names - no re-download needed, since B2 already stores and
// serves this value reliably (either the value this package supplied at
// upload time via the trailer, or one it computed itself for an object
// uploaded some other way) - see the package doc comment. For a file
// uploaded through UploadMultipart, B2 has nothing cheap to serve back
// (contentSha1 comes back as b2NoContentSha1 - see its doc comment), so
// this falls back to re-downloading and rehashing, the same approach
// webdav/dropbox use unconditionally because their protocols never expose
// a trustworthy native hash at all.
func (p *Provider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	if algo != checksumAlgo {
		return false, fmt.Errorf("b2: cannot verify checksum of algo %q", algo)
	}

	_, sha1sum, found, err := p.findFile(ctx, remotePath)
	if err != nil {
		return false, fmt.Errorf("b2: verify %q: %w", remotePath, err)
	}
	if !found {
		return false, fmt.Errorf("b2: verify %q: not found", remotePath)
	}
	if sha1sum != b2NoContentSha1 {
		return sha1sum == checksum, nil
	}

	h := sha1.New()
	if err := p.Download(ctx, provider.DownloadTask{RemotePath: remotePath, Writer: h}); err != nil {
		return false, fmt.Errorf("b2: verify %q: %w", remotePath, err)
	}
	return hex.EncodeToString(h.Sum(nil)) == checksum, nil
}

// withReauth runs call; if it reports errUnauthorized, it invalidates the
// cached session, re-authorizes, and runs call exactly once more. B2's
// session tokens expire on a schedule this package cannot observe, so one
// blind retry is cheaper than tracking expiry - and internal/queue.Manager
// retries above this layer anyway, so a second failure needs no further
// heroics here.
//
// Note which calls do *not* go through this: the upload POST itself, which
// has already consumed part of task.Reader by the time a 401 could come
// back and therefore cannot be replayed - see the Upload doc comment.
func (p *Provider) withReauth(ctx context.Context, call func() error) error {
	err := call()
	if errors.Is(err, errUnauthorized) {
		p.invalidateSession()
		if err2 := p.ensureSession(ctx); err2 != nil {
			return err2
		}
		err = call()
	}
	return err
}

// postJSON POSTs reqBody as JSON to the current session's apiURL + path,
// using the session auth token, and decodes a 2xx JSON body into respOut
// (skipped when respOut is nil). Returns errUnauthorized on 401 so callers
// can re-authorize - see withReauth.
//
// Every b2api control call in this package is this same shape (marshal,
// authorize, POST, classify status, decode), which is why it lives here
// once instead of at each of the five call sites. The two streaming
// endpoints - the upload POST and the file download - deliberately stay
// hand-rolled: they send/receive raw bodies rather than JSON, and the
// upload carries the X-Bz-Content-Sha1 trailer contract on top.
func (p *Provider) postJSON(ctx context.Context, path string, reqBody, respOut any) error {
	apiURL, authToken, _, _ := p.session()
	return p.postJSONTo(ctx, apiURL+path, authToken, reqBody, respOut)
}

// postJSONTo is postJSON's primitive: it targets an explicit URL with an
// explicit Authorization header value, for the two calls that run before a
// session exists (authorize, resolveBucket) and therefore cannot look one
// up - see resolveBucket for why reading it would in fact deadlock.
func (p *Provider) postJSONTo(ctx context.Context, url, authHeader string, reqBody, respOut any) error {
	var body io.Reader
	if reqBody != nil {
		encoded, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	endpoint := endpointName(url)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return unauthorizedError(resp)
	}
	if resp.StatusCode/100 != 2 {
		return parseB2Error(resp)
	}

	if respOut == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(respOut); err != nil {
		return fmt.Errorf("decoding %s response: %w", endpoint, err)
	}
	return nil
}

// basicAuth builds the Authorization header value b2_authorize_account
// expects. It duplicates what http.Request.SetBasicAuth does, because
// postJSONTo takes the header value rather than credentials (session-token
// calls have no username/password to give it).
func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// endpointName is the last path segment of a B2 API URL ("b2_list_buckets"),
// used to name the failing call in error messages without repeating the
// endpoint string at every call site.
func endpointName(rawURL string) string {
	if i := strings.IndexAny(rawURL, "?#"); i >= 0 {
		rawURL = rawURL[:i]
	}
	return path.Base(strings.TrimSuffix(rawURL, "/"))
}

// session returns a consistent snapshot of the cached session fields.
func (p *Provider) session() (apiURL, authToken, downloadURL, bucketID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.apiURL, p.authToken, p.downloadURL, p.bucketID
}

// invalidateSession clears every cached session field so the next
// ensureSession re-authorizes and re-resolves the bucket from scratch.
//
// Clearing authToken and bucketID together (rather than just the token)
// keeps the "do we have a usable session" check in ensureSession a single
// simple condition. The remaining fields are cleared too so that no stale
// apiURL/downloadURL/accountID can be read by session() in the window
// between invalidation and the next successful authorize - a caller that
// used one would be aiming a fresh token at an endpoint from a previous
// session.
func (p *Provider) invalidateSession() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.apiURL = ""
	p.authToken = ""
	p.downloadURL = ""
	p.accountID = ""
	p.bucketID = ""
}

// ensureSession authorizes against B2 and resolves cfg.BucketName to a
// bucketId if there is no cached session yet. It is called at the top of
// every exported method that needs the API - see the Provider doc
// comment for why this, rather than New, is where the first network call
// happens.
func (p *Provider) ensureSession(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.authToken != "" && p.bucketID != "" {
		return nil
	}
	if err := p.authorize(ctx); err != nil {
		return fmt.Errorf("b2: authorizing: %w", err)
	}
	if err := p.resolveBucket(ctx); err != nil {
		return fmt.Errorf("b2: resolving bucket %q: %w", p.cfg.BucketName, err)
	}
	return nil
}

// authorize calls b2_authorize_account and caches the resulting session.
// Must be called with p.mu held.
func (p *Provider) authorize(ctx context.Context) error {
	var out struct {
		APIURL             string `json:"apiUrl"`
		AuthorizationToken string `json:"authorizationToken"`
		DownloadURL        string `json:"downloadUrl"`
		AccountID          string `json:"accountId"`
	}
	// basicAuth rather than req.SetBasicAuth: this is the one call that
	// authenticates with the application key itself instead of a session
	// token, and postJSONTo takes the Authorization value verbatim so it
	// can serve both cases. nil reqBody - b2_authorize_account takes none.
	if err := p.postJSONTo(ctx, authorizeURL, basicAuth(p.keyID, p.appKey), nil, &out); err != nil {
		return err
	}

	p.apiURL = out.APIURL
	p.authToken = out.AuthorizationToken
	p.downloadURL = out.DownloadURL
	p.accountID = out.AccountID
	return nil
}

// resolveBucket looks up cfg.BucketName's bucketId via b2_list_buckets and
// caches it. Must be called with p.mu held, after authorize has populated
// p.apiURL/p.authToken/p.accountID.
func (p *Provider) resolveBucket(ctx context.Context) error {
	var out struct {
		Buckets []struct {
			BucketID   string `json:"bucketId"`
			BucketName string `json:"bucketName"`
		} `json:"buckets"`
	}
	// postJSONTo, not postJSON: this runs with p.mu held, and postJSON
	// reads the session snapshot through session(), which takes the same
	// mutex - that would deadlock. The fields it would read are p.apiURL/
	// p.authToken, which are right here anyway.
	if err := p.postJSONTo(ctx, p.apiURL+"/b2api/v2/b2_list_buckets", p.authToken,
		map[string]string{"accountId": p.accountID, "bucketName": p.cfg.BucketName}, &out); err != nil {
		return err
	}
	for _, b := range out.Buckets {
		if b.BucketName == p.cfg.BucketName {
			p.bucketID = b.BucketID
			return nil
		}
	}
	return fmt.Errorf("bucket %q not found or not accessible with this application key", p.cfg.BucketName)
}

// b2ErrorBody mirrors B2's standard JSON error shape, e.g.
// {"status":400,"code":"bad_request","message":"..."}.
type b2ErrorBody struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// parseB2Error turns a non-2xx B2 API response into a descriptive Go
// error, falling back to a generic message if the body isn't B2's usual
// error shape. Shared by every API call site in this package instead of
// duplicating the same parsing at each one.
//
// The message deliberately does not name "b2" itself: every public method
// already wraps its errors as `b2: <op> %q: ...` like the other providers
// do, so saying it here too produced messages that read "b2: upload "x":
// b2 api error ...".
func parseB2Error(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var eb b2ErrorBody
	if err := json.Unmarshal(body, &eb); err == nil && (eb.Code != "" || eb.Message != "") {
		return fmt.Errorf("api error (status %d, code %q): %s", resp.StatusCode, eb.Code, eb.Message)
	}
	return fmt.Errorf("unexpected api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// unauthorizedError reports a 401 as the errUnauthorized sentinel (so
// withReauth can recognize it) while keeping B2's own explanation attached,
// which matters for the calls that are never retried: a wrong
// keyId/applicationKey must not be reported as a bare "unauthorized".
func unauthorizedError(resp *http.Response) error {
	return fmt.Errorf("%w: %w", errUnauthorized, parseB2Error(resp))
}

// escapeB2Path percent-encodes each path segment of path individually and
// rejoins them with "/", which B2 treats as a legal, unescaped separator
// within a file name.
func escapeB2Path(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// normalizeListPrefix turns a remotePath into a B2 list prefix that
// behaves like a folder: empty stays empty (list the whole bucket root),
// anything else gets a trailing "/" if it doesn't already have one.
func normalizeListPrefix(remotePath string) string {
	if remotePath == "" {
		return ""
	}
	if strings.HasSuffix(remotePath, "/") {
		return remotePath
	}
	return remotePath + "/"
}
