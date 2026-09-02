package provider

import "context"

// MultipartUploader is implemented by providers that can send a large
// upload in chunks instead of one request (S3's multipart upload, Dropbox's
// upload sessions).
//
// internal/queue.Manager calls UploadMultipart instead of Upload when the
// file exceeds its configured threshold and the provider implements this
// interface; otherwise it falls back to Upload. Implementing this method is
// therefore the whole opt-in - nothing needs registering.
//
// Two obligations on implementors:
//
//   - partSize is a generic hint from the core, which knows nothing about
//     any particular service's rules. Clamp it to whatever the protocol
//     requires (S3: ≥5 MiB per part except the last; Dropbox: multiples of
//     4 MiB, ≤150 MB per request) rather than passing it through and
//     failing mid-upload.
//   - The checksum reported in UploadResult must match what Upload would
//     produce for the same bytes. Wrap task.Reader once, before chunking,
//     so the hash spans the whole file - otherwise the same file verifies
//     differently depending on its size, and internal/history's
//     verification silently starts failing.
type MultipartUploader interface {
	UploadMultipart(ctx context.Context, task UploadTask, partSize int64) (UploadResult, error)
}

// ParallelMultipartUploader is implemented by providers whose chunked-
// upload protocol allows independent parts to be uploaded concurrently
// from multiple goroutines instead of one at a time - S3 (parts may be
// uploaded in any order, a standard and widely-relied-upon S3 behavior) and
// B2 (same, but each concurrent uploader needs its own upload-part
// URL/token, obtained via a separate b2_get_upload_part_url call - see
// internal/providers/b2). Dropbox's upload_session API is deliberately not
// a candidate: each append_v2 call must carry the exact byte offset the
// server has already received, so parts cannot be sent out of order or
// concurrently without the server rejecting them - this is not a missing
// feature, it is a protocol constraint.
//
// internal/queue.Manager calls UploadMultipartParallel instead of
// UploadMultipart/Upload only when all of the following hold: the file is
// at least as large as the configured multi-thread-streams threshold
// (internal/settings.Settings.MultiThreadThresholdBytes), the configured
// stream count is > 1, the provider implements this interface, and the
// source supports concurrent random access (task.ReaderAt is non-nil - see
// its doc comment). Any of these being false falls back to the ordinary
// MultipartUploader/Upload path, so implementing this interface is purely
// additive - like MultipartUploader itself, nothing needs registering.
//
// Implementors take on the same two obligations as MultipartUploader
// (clamp partSize to the protocol's own rules; the returned checksum must
// match what Upload/UploadMultipart would produce for the same bytes) plus
// a third: clamp streams to whatever concurrency the provider/account can
// actually sustain.
type ParallelMultipartUploader interface {
	UploadMultipartParallel(ctx context.Context, task UploadTask, partSize int64, streams int) (UploadResult, error)
}

// ChecksumVerifier is implemented by providers that can confirm a
// previously uploaded object still matches a checksum. checksumAlgo and
// checksum are exactly the opaque values the same provider returned in
// UploadResult when the object was uploaded - the core never inspects or
// interprets them.
type ChecksumVerifier interface {
	VerifyChecksum(ctx context.Context, remotePath, checksumAlgo, checksum string) (bool, error)
}

// ExistenceChecker is implemented by providers that can cheaply confirm an
// object is still present on the remote storage, without a full checksum
// verification.
type ExistenceChecker interface {
	Exists(ctx context.Context, remotePath string) (bool, error)
}

// FieldType describes how a config field should be rendered/collected.
type FieldType string

const (
	FieldText     FieldType = "text"
	FieldPassword FieldType = "password" // must be routed to the secret store, never to the JSON config
	FieldSelect   FieldType = "select"
)

// FieldSpec describes a single field of a provider's connection form.
type FieldSpec struct {
	Key      string
	Label    string
	Type     FieldType
	Required bool
	Options  []string // only used when Type == FieldSelect
}

// ConfigSchema is implemented by providers so the UI can render their
// connection form generically instead of hardcoding a form per provider
// type.
type ConfigSchema interface {
	ConfigFields() []FieldSpec
}
