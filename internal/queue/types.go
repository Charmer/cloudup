package queue

import (
	"io"
	"time"
)

// Status is the lifecycle state of a queued upload, reported via Event.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusUploading Status = "uploading" // also used for progress updates (Sent/Total set)
	StatusSuccess   Status = "success"
	StatusError     Status = "error"
	StatusCancelled Status = "cancelled"
)

// UploadRequest describes a file to enqueue. Open is called once per
// attempt (including retries) rather than accepting a bare io.Reader,
// because a stream that failed partway through generally cannot be
// rewound - each retry needs a fresh handle on the source.
type UploadRequest struct {
	LocalPath  string
	RemotePath string

	// Size is the exact byte count Open's stream will yield. Providers are
	// entitled to trust it: several must declare a Content-Length before
	// sending a body (b2 additionally derives its SHA-1 trailer offset from
	// it), so a Size that disagrees with the stream produces a failed or
	// corrupt upload rather than a clean error.
	//
	// That makes it a real constraint on callers, because Open is called
	// afresh for every attempt including retries: whatever Open returns must
	// not change size between attempts. Today's only caller satisfies this
	// by construction - internal/httpapi spools the request body to a
	// private temp file and hands out that path, so nothing can rewrite it
	// mid-upload. A future caller streaming a live local file (a folder
	// watcher, say) would not be safe, and should measure the size at Open
	// time rather than at enqueue time.
	Size int64

	// Open returns a fresh reader over the source. Called once per attempt,
	// so a retry re-reads from the beginning - which is why this is a
	// factory rather than a plain io.Reader.
	Open func() (io.ReadCloser, error)
}

// Event reports queue/task activity to subscribers (typically the UI).
type Event struct {
	ProviderID string
	TaskID     string
	LocalPath  string
	RemotePath string
	Status     Status
	Sent       int64
	Total      int64
	Err        error
	// HistoryID is set on terminal events (Success/Error/Cancelled) once the
	// outcome has been recorded in internal/history.
	HistoryID int64
}

// RetryPolicy controls how many times a failed upload is retried and how
// long to wait between attempts (exponential backoff, capped at MaxDelay).
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultRetryPolicy is a reasonable default: 3 attempts, starting at 1s
// and doubling up to 30s.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 30 * time.Second}
}
