package streamio

import (
	"context"
	"io"
)

// RateLimiter is the subset of golang.org/x/time/rate.Limiter that
// LimitedReader/LimitedReaderAt need. Defined here instead of depending on
// *rate.Limiter directly so tests can swap in a fake that tracks calls
// without actually sleeping.
type RateLimiter interface {
	// WaitN blocks until n tokens are available or ctx is done. n must not
	// exceed Burst().
	WaitN(ctx context.Context, n int) error
	// Burst returns the maximum number of tokens a single WaitN call may
	// request.
	Burst() int
}

// waitN drains n tokens from lim, split into calls of at most lim.Burst()
// each - WaitN itself rejects a single call for more than that. Splitting
// here (rather than requiring every caller to do it) is what lets
// LimitedReader/LimitedReaderAt hand back arbitrarily large reads from the
// underlying source while still honoring the limiter's per-call cap.
func waitN(ctx context.Context, lim RateLimiter, n int) error {
	burst := lim.Burst()
	for n > 0 {
		take := n
		if burst > 0 && take > burst {
			take = burst
		}
		if err := lim.WaitN(ctx, take); err != nil {
			return err
		}
		n -= take
	}
	return nil
}

// LimitedReader wraps R so the aggregate byte rate read through it (and any
// other LimitedReader/LimitedReaderAt sharing the same Limiter) does not
// exceed the limiter's configured rate. Unlike a naive implementation that
// caps the buffer passed to R.Read, this reads whatever R gives back and
// only then blocks - so a byte count already read from local disk is simply
// held back before being handed to the caller (typically about to write it
// to the network), which is what actually paces the network write that
// follows.
type LimitedReader struct {
	R       io.Reader
	Limiter RateLimiter
	Ctx     context.Context
}

func (l *LimitedReader) Read(buf []byte) (int, error) {
	n, err := l.R.Read(buf)
	if n > 0 {
		if werr := waitN(l.Ctx, l.Limiter, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// LimitedReaderAt is LimitedReader's counterpart for io.ReaderAt, needed by
// ParallelMultipartUploader implementations, whose concurrent parts read
// via ReaderAt directly and never go through a wrapped io.Reader. Sharing
// the same Limiter as a LimitedReader (or another LimitedReaderAt) caps
// their combined throughput, not each independently.
//
// ReadAt's contract requires returning exactly len(p) bytes unless erroring
// (io.ReaderAt), so - like LimitedReader - this never truncates the
// underlying read to fit the limiter's burst; it reads in full, then blocks
// proportionally to what was read.
type LimitedReaderAt struct {
	R       io.ReaderAt
	Limiter RateLimiter
	Ctx     context.Context
}

func (l *LimitedReaderAt) ReadAt(buf []byte, off int64) (int, error) {
	n, err := l.R.ReadAt(buf, off)
	if n > 0 {
		if werr := waitN(l.Ctx, l.Limiter, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}
