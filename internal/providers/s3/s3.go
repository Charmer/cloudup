// Package s3 implements provider.Provider over the AWS S3 API. Because a
// custom endpoint and path-style addressing are supported, this same
// package also covers S3-compatible storage (MinIO, Backblaze B2, Wasabi,
// Yandex Object Storage) - no separate implementation needed for them.
//
// Checksum verification does not rely on S3's native x-amz-checksum-*
// feature: most S3-compatible services don't implement it, so - exactly
// like internal/providers/webdav - this package computes its own SHA-256
// while streaming the upload and verifies later by re-downloading and
// rehashing. This keeps ChecksumVerifier's behavior identical (and
// identically testable) across every S3-compatible backend, at the cost of
// a full re-download on verification rather than a cheap metadata read.
package s3

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
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"cloudup/internal/debuglog"
	"cloudup/internal/provider"
	"cloudup/internal/registry"
	"cloudup/internal/streamio"
)

const Type = "s3"

const (
	secretAccessKeyID     = "accessKeyId"
	secretSecretAccessKey = "secretAccessKey"
)

// checksumAlgo is the label stored in UploadResult/upload_log - see the
// package doc comment for why this is self-computed rather than sourced
// from S3's checksum feature.
const checksumAlgo = provider.ChecksumSHA256SelfComputed

func init() {
	registry.Register(Type, New)
	registry.RegisterSchema(Type, (&Provider{}).ConfigFields)
}

// Config is the non-secret part of an S3 connection, persisted in the JSON
// config file. Credentials live in the secret store.
type Config struct {
	ConnectionID string `json:"connectionId"`
	Bucket       string `json:"bucket"`
	Region       string `json:"region"`
	Endpoint     string `json:"endpoint"`     // optional: set for S3-compatible storage
	UsePathStyle bool   `json:"usePathStyle"` // typically required for S3-compatible storage
	DisplayName  string `json:"displayName"`
}

// rawConfig mirrors Config but with UsePathStyle as a string, matching how
// internal/config stores every provider field as map[string]string (see
// provider.FieldSpec/FieldType - there is no boolean field type).
type rawConfig struct {
	ConnectionID string `json:"connectionId"`
	Bucket       string `json:"bucket"`
	Region       string `json:"region"`
	Endpoint     string `json:"endpoint"`
	UsePathStyle string `json:"usePathStyle"`
	DisplayName  string `json:"displayName"`
}

// Provider implements provider.Provider (and several optional feature
// interfaces) over the AWS S3 API.
type Provider struct {
	cfg    Config
	client *s3.Client
}

// New is the registry.Factory for the "s3" provider type.
func New(rawCfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
	var raw rawConfig
	if err := json.Unmarshal(rawCfg, &raw); err != nil {
		return nil, fmt.Errorf("s3: invalid config: %w", err)
	}
	if raw.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket is required")
	}
	if raw.Region == "" {
		return nil, fmt.Errorf("s3: region is required")
	}

	accessKeyID, err := secrets.Get(raw.ConnectionID, secretAccessKeyID)
	if err != nil {
		return nil, fmt.Errorf("s3: reading access key id secret: %w", err)
	}
	secretAccessKey, err := secrets.Get(raw.ConnectionID, secretSecretAccessKey)
	if err != nil {
		return nil, fmt.Errorf("s3: reading secret access key secret: %w", err)
	}

	cfg := Config{
		ConnectionID: raw.ConnectionID,
		Bucket:       raw.Bucket,
		Region:       raw.Region,
		Endpoint:     raw.Endpoint,
		UsePathStyle: raw.UsePathStyle == "true",
		DisplayName:  raw.DisplayName,
	}

	awsCfg := aws.Config{
		Region:      cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
		// Hand the SDK a client wrapped in debuglog.Transport so
		// CLOUDUP_DEBUG=1 covers S3 too; it is a transparent passthrough
		// when that variable is unset - see internal/debuglog.
		HTTPClient: &http.Client{Transport: debuglog.Transport{}},
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
		// Uploads stream from task.Reader, which is not guaranteed to be
		// seekable (it may already be wrapped in io.TeeReader for checksum
		// computation) - SigV4 payload signing needs to seek back to the
		// start after hashing, so it is swapped for unsigned payload here.
		// Integrity in transit is still covered by TLS, and content
		// integrity is independently covered by our own checksum in
		// UploadResult/ChecksumVerifier.
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	})

	return &Provider{cfg: cfg, client: client}, nil
}

func (p *Provider) Type() string { return Type }

func (p *Provider) DisplayName() string {
	if p.cfg.DisplayName != "" {
		return p.cfg.DisplayName
	}
	return "S3 (" + p.cfg.Bucket + ")"
}

func (p *Provider) TestConnection(ctx context.Context) error {
	_, err := p.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(p.cfg.Bucket)})
	if err != nil {
		return fmt.Errorf("s3: connection test failed: %w", err)
	}
	return nil
}

// Upload streams task.Reader to task.RemotePath, computing a SHA-256
// checksum while streaming so no second read of the source is needed.
func (p *Provider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	h := sha256.New()
	reader := io.TeeReader(&streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}, h)

	_, err := p.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(p.cfg.Bucket),
		Key:           aws.String(task.RemotePath),
		Body:          reader,
		ContentLength: aws.Int64(task.Size),
	})
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("s3: upload %q: %w", task.RemotePath, err)
	}

	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    p.objectURL(task.RemotePath),
		ChecksumAlgo: checksumAlgo,
		Checksum:     hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// UploadMultipart implements provider.MultipartUploader: task.Reader is
// split into partSize chunks sent as independent parts, while the whole
// stream is still hashed exactly once via the same TeeReader technique as
// Upload - the checksum is of the complete file regardless of how it was
// chunked for transport, so it is directly comparable to a single-part
// upload's checksum.
func (p *Provider) UploadMultipart(ctx context.Context, task provider.UploadTask, partSize int64) (provider.UploadResult, error) {
	if partSize <= 0 {
		return provider.UploadResult{}, fmt.Errorf("s3: partSize must be positive")
	}

	create, err := p.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(task.RemotePath),
	})
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("s3: creating multipart upload for %q: %w", task.RemotePath, err)
	}
	uploadID := create.UploadId

	abort := func() {
		_, _ = p.client.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{
			Bucket: aws.String(p.cfg.Bucket), Key: aws.String(task.RemotePath), UploadId: uploadID,
		})
	}

	h := sha256.New()
	source := io.TeeReader(&streamio.ProgressReader{R: task.Reader, Total: task.Size, OnProgress: task.Progress}, h)

	var parts []types.CompletedPart
	buf := make([]byte, partSize)
	var partNumber int32 = 1

	for {
		n, readErr := io.ReadFull(source, buf)
		if n > 0 {
			out, err := p.client.UploadPart(ctx, &s3.UploadPartInput{
				Bucket:     aws.String(p.cfg.Bucket),
				Key:        aws.String(task.RemotePath),
				UploadId:   uploadID,
				PartNumber: aws.Int32(partNumber),
				Body:       bytes.NewReader(buf[:n]),
			})
			if err != nil {
				abort()
				return provider.UploadResult{}, fmt.Errorf("s3: uploading part %d of %q: %w", partNumber, task.RemotePath, err)
			}
			parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(partNumber), ETag: out.ETag})
			partNumber++
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			abort()
			return provider.UploadResult{}, fmt.Errorf("s3: reading %q for multipart upload: %w", task.LocalPath, readErr)
		}
	}

	_, err = p.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(p.cfg.Bucket),
		Key:             aws.String(task.RemotePath),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		abort()
		return provider.UploadResult{}, fmt.Errorf("s3: completing multipart upload for %q: %w", task.RemotePath, err)
	}

	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    p.objectURL(task.RemotePath),
		ChecksumAlgo: checksumAlgo,
		Checksum:     hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// UploadMultipartParallel implements provider.ParallelMultipartUploader.
// Unlike UploadMultipart's sequential loop, S3 parts may be uploaded in any
// order and concurrently - a standard, widely-relied-upon S3 behavior (every
// major S3 client uploads multipart parts this way) - so this dispatches
// part uploads across up to streams goroutines via
// streamio.UploadPartsConcurrently, reading each part straight from
// task.ReaderAt instead of task.Reader.
//
// Checksum parity with Upload/UploadMultipart: since parts are read out of
// order by definition, they cannot share one sequential TeeReader the way
// the sequential paths do. Instead, a dedicated goroutine makes its own
// ordinary sequential pass over the whole content
// (io.NewSectionReader(task.ReaderAt, 0, task.Size)) to compute the SHA-256,
// running concurrently with the part uploads. This is safe because
// task.ReaderAt is guaranteed by internal/queue to back onto a real
// *os.File, and os.File.ReadAt supports concurrent calls from independent
// goroutines - see its own doc comment. The result is byte-for-byte the
// same SHA-256 the sequential paths would produce for the same content.
func (p *Provider) UploadMultipartParallel(ctx context.Context, task provider.UploadTask, partSize int64, streams int) (provider.UploadResult, error) {
	if partSize <= 0 {
		return provider.UploadResult{}, fmt.Errorf("s3: partSize must be positive")
	}
	if task.ReaderAt == nil {
		return provider.UploadResult{}, fmt.Errorf("s3: parallel multipart upload requires a random-access source")
	}

	create, err := p.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(task.RemotePath),
	})
	if err != nil {
		return provider.UploadResult{}, fmt.Errorf("s3: creating multipart upload for %q: %w", task.RemotePath, err)
	}
	uploadID := create.UploadId

	abort := func() {
		_, _ = p.client.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{
			Bucket: aws.String(p.cfg.Bucket), Key: aws.String(task.RemotePath), UploadId: uploadID,
		})
	}

	hashDone := make(chan error, 1)
	var sum []byte
	go func() {
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(task.ReaderAt, 0, task.Size)); err != nil {
			hashDone <- err
			return
		}
		sum = h.Sum(nil)
		hashDone <- nil
	}()

	progress := streamio.NewAtomicProgress(task.Size, task.Progress)
	parts, err := streamio.UploadPartsConcurrently(ctx, task.ReaderAt, task.Size, partSize, streams,
		func(ctx context.Context, part streamio.Part, r io.Reader) (types.CompletedPart, error) {
			// ContentLength must be set explicitly here: r is progress.
			// Reader(r), a wrapper that satisfies plain io.Reader only, so
			// the SDK can no longer infer a length the way it does for the
			// *io.SectionReader/*bytes.Reader UploadMultipart's sequential
			// loop passes directly - without this, S3 rejects the request
			// as MissingContentLength (found by the parallel round-trip
			// test failing before this was added).
			out, err := p.client.UploadPart(ctx, &s3.UploadPartInput{
				Bucket:        aws.String(p.cfg.Bucket),
				Key:           aws.String(task.RemotePath),
				UploadId:      uploadID,
				PartNumber:    aws.Int32(int32(part.Number)),
				Body:          progress.Reader(r),
				ContentLength: aws.Int64(part.Size),
			})
			if err != nil {
				return types.CompletedPart{}, err
			}
			return types.CompletedPart{PartNumber: aws.Int32(int32(part.Number)), ETag: out.ETag}, nil
		})
	if err != nil {
		abort()
		return provider.UploadResult{}, fmt.Errorf("s3: parallel multipart upload of %q: %w", task.RemotePath, err)
	}

	if err := <-hashDone; err != nil {
		abort()
		return provider.UploadResult{}, fmt.Errorf("s3: parallel multipart upload of %q: hashing source: %w", task.RemotePath, err)
	}

	if _, err := p.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(p.cfg.Bucket),
		Key:             aws.String(task.RemotePath),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		abort()
		return provider.UploadResult{}, fmt.Errorf("s3: parallel multipart upload of %q: completing: %w", task.RemotePath, err)
	}

	return provider.UploadResult{
		RemotePath:   task.RemotePath,
		RemoteURL:    p.objectURL(task.RemotePath),
		ChecksumAlgo: checksumAlgo,
		Checksum:     hex.EncodeToString(sum),
	}, nil
}

func (p *Provider) Download(ctx context.Context, task provider.DownloadTask) error {
	out, err := p.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(task.RemotePath),
	})
	if err != nil {
		return fmt.Errorf("s3: download %q: %w", task.RemotePath, err)
	}
	defer out.Body.Close()

	writer := task.Writer
	if task.Progress != nil {
		writer = &streamio.ProgressWriter{W: writer, Total: aws.ToInt64(out.ContentLength), OnProgress: task.Progress}
	}

	if _, err := io.Copy(writer, out.Body); err != nil {
		return fmt.Errorf("s3: download %q: %w", task.RemotePath, err)
	}
	return nil
}

func (p *Provider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	out, err := p.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(p.cfg.Bucket),
		Prefix:    aws.String(remotePath),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		return nil, fmt.Errorf("s3: list %q: %w", remotePath, err)
	}

	entries := make([]provider.RemoteEntry, 0, len(out.Contents)+len(out.CommonPrefixes))
	for _, cp := range out.CommonPrefixes {
		prefix := aws.ToString(cp.Prefix)
		entries = append(entries, provider.RemoteEntry{
			Path:  prefix,
			Name:  strings.TrimSuffix(strings.TrimPrefix(prefix, remotePath), "/"),
			IsDir: true,
		})
	}
	for _, obj := range out.Contents {
		key := aws.ToString(obj.Key)
		entries = append(entries, provider.RemoteEntry{
			Path:    key,
			Name:    strings.TrimPrefix(key, remotePath),
			Size:    aws.ToInt64(obj.Size),
			ModTime: aws.ToTime(obj.LastModified),
		})
	}
	return entries, nil
}

func (p *Provider) Delete(ctx context.Context, remotePath string) error {
	_, err := p.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(remotePath),
	})
	if err != nil {
		return fmt.Errorf("s3: delete %q: %w", remotePath, err)
	}
	return nil
}

// Exists implements provider.ExistenceChecker.
func (p *Provider) Exists(ctx context.Context, remotePath string) (bool, error) {
	_, err := p.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(remotePath),
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("s3: stat %q: %w", remotePath, err)
	}
	return true, nil
}

// VerifyChecksum implements provider.ChecksumVerifier - see the package
// doc comment for why this re-downloads and rehashes instead of trusting a
// server-reported checksum.
func (p *Provider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	if algo != checksumAlgo {
		return false, fmt.Errorf("s3: cannot verify checksum of algo %q", algo)
	}

	out, err := p.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(remotePath),
	})
	if err != nil {
		return false, fmt.Errorf("s3: verify %q: %w", remotePath, err)
	}
	defer out.Body.Close()

	h := sha256.New()
	if _, err := io.Copy(h, out.Body); err != nil {
		return false, fmt.Errorf("s3: verify %q: %w", remotePath, err)
	}

	return hex.EncodeToString(h.Sum(nil)) == checksum, nil
}

// ConfigFields implements provider.ConfigSchema.
func (p *Provider) ConfigFields() []provider.FieldSpec {
	return []provider.FieldSpec{
		{Key: "bucket", Label: "Bucket", Type: provider.FieldText, Required: true},
		{Key: "region", Label: "Region", Type: provider.FieldText, Required: true},
		{Key: "endpoint", Label: "Endpoint (optional, for S3-compatible storage)", Type: provider.FieldText},
		{Key: "usePathStyle", Label: "Path-style addressing", Type: provider.FieldSelect, Options: []string{"false", "true"}},
		{Key: secretAccessKeyID, Label: "Access Key ID", Type: provider.FieldPassword, Required: true},
		{Key: secretSecretAccessKey, Label: "Secret Access Key", Type: provider.FieldPassword, Required: true},
	}
}

func (p *Provider) objectURL(key string) string {
	if p.cfg.Endpoint != "" {
		return fmt.Sprintf("%s/%s/%s", strings.TrimRight(p.cfg.Endpoint, "/"), p.cfg.Bucket, key)
	}
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", p.cfg.Bucket, p.cfg.Region, key)
}

func isNotFound(err error) bool {
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		return respErr.HTTPStatusCode() == 404
	}
	return false
}
