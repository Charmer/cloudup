// Package provider defines the contract that every cloud storage backend
// implements. The core (queue, history, UI) depends only on the interfaces
// declared here, never on a concrete storage implementation.
package provider

import (
	"context"
	"io"
	"time"
)

// UploadTask describes a single file to be uploaded.
type UploadTask struct {
	LocalPath  string
	RemotePath string
	Size       int64
	Reader     io.Reader

	// ReaderAt, when non-nil, gives concurrent random-access reads of the
	// exact same content as Reader (both read the same underlying source -
	// a provider must use one or the other for a given upload, never both).
	// internal/queue populates this only when Open's result happens to
	// support it (true for the private temp file internal/httpapi spools
	// every upload to; false for a hypothetical future source that streams
	// without ever landing on local disk, like a live network pipe).
	//
	// This exists solely for ParallelMultipartUploader implementations,
	// which need several goroutines each reading their own disjoint byte
	// range at once - something a plain sequential io.Reader cannot support
	// safely. A provider that only implements MultipartUploader/Upload can
	// ignore this field entirely.
	ReaderAt io.ReaderAt

	// Progress is called by the provider as bytes are sent. May be nil.
	Progress func(sent, total int64)
}

// DownloadTask describes a single remote object to be downloaded.
type DownloadTask struct {
	RemotePath string
	LocalPath  string
	Writer     io.Writer

	// Progress is called by the provider as bytes are received. May be nil.
	Progress func(received, total int64)
}

// UploadResult is returned by Upload (and the optional upload variants) on
// success. ChecksumAlgo/Checksum are opaque to the core: only the same
// provider that produced them knows how to interpret and verify them later
// (see ChecksumVerifier).
type UploadResult struct {
	RemotePath   string
	RemoteURL    string
	ChecksumAlgo string
	Checksum     string
}

// RemoteEntry describes a single object/folder as reported by a provider.
type RemoteEntry struct {
	Path    string
	Name    string
	Size    int64
	IsDir   bool
	ModTime time.Time
}

// Provider is the minimal contract every storage backend must satisfy.
// Anything a storage backend can additionally do (multipart upload, resume,
// checksum verification, existence checks, quota, ...) is expressed through
// separate, optional interfaces in features.go and detected via type
// assertion by the core - never by extending this interface.
type Provider interface {
	// Type is the stable identifier used for registration/config, e.g. "s3".
	Type() string

	// DisplayName is a human-readable name shown in the UI.
	DisplayName() string

	// TestConnection verifies the stored credentials/settings actually work.
	TestConnection(ctx context.Context) error

	// Upload sends task.Reader to task.RemotePath. Implementations that can
	// compute a checksum while streaming should do so and populate it in
	// UploadResult instead of requiring a second read of the file.
	Upload(ctx context.Context, task UploadTask) (UploadResult, error)

	// Download retrieves the object at task.RemotePath into task.Writer.
	Download(ctx context.Context, task DownloadTask) error

	// List enumerates entries under remotePath.
	List(ctx context.Context, remotePath string) ([]RemoteEntry, error)

	// Delete removes the object at remotePath.
	Delete(ctx context.Context, remotePath string) error
}
