package streamio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// fakeLimiter records every WaitN call instead of actually delaying, so
// these tests run instantly regardless of the configured burst.
type fakeLimiter struct {
	burst int
	calls []int
	err   error // returned by the next WaitN call, then cleared
}

func (f *fakeLimiter) Burst() int { return f.burst }

func (f *fakeLimiter) WaitN(ctx context.Context, n int) error {
	if n > f.burst {
		return errors.New("fakeLimiter: n exceeds burst")
	}
	if f.err != nil {
		err := f.err
		f.err = nil
		return err
	}
	f.calls = append(f.calls, n)
	return nil
}

func TestLimitedReaderSplitsReadsAtBurst(t *testing.T) {
	src := bytes.Repeat([]byte("x"), 250)
	lim := &fakeLimiter{burst: 100}
	r := &LimitedReader{R: bytes.NewReader(src), Limiter: lim, Ctx: context.Background()}

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Fatalf("data mismatch: got %d bytes, want %d", len(got), len(src))
	}

	var total int
	for _, n := range lim.calls {
		if n > lim.burst {
			t.Fatalf("WaitN called with n=%d exceeding burst=%d", n, lim.burst)
		}
		total += n
	}
	if total != len(src) {
		t.Fatalf("limiter saw %d total bytes, want %d", total, len(src))
	}
}

func TestLimitedReaderPropagatesLimiterError(t *testing.T) {
	lim := &fakeLimiter{burst: 10, err: context.Canceled}
	r := &LimitedReader{R: bytes.NewReader([]byte("hello")), Limiter: lim, Ctx: context.Background()}

	buf := make([]byte, 5)
	n, err := r.Read(buf)
	if n != 5 {
		t.Fatalf("n = %d, want 5 (the underlying read still happened)", n)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestLimitedReaderPassesThroughUnderlyingError(t *testing.T) {
	lim := &fakeLimiter{burst: 1000}
	r := &LimitedReader{R: iotest_errReader{err: io.ErrUnexpectedEOF}, Limiter: lim, Ctx: context.Background()}

	_, err := r.Read(make([]byte, 4))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

type iotest_errReader struct{ err error }

func (r iotest_errReader) Read([]byte) (int, error) { return 0, r.err }

func TestLimitedReaderAtSplitsReadsAtBurst(t *testing.T) {
	src := bytes.Repeat([]byte("y"), 333)
	lim := &fakeLimiter{burst: 64}
	ra := &LimitedReaderAt{R: bytes.NewReader(src), Limiter: lim, Ctx: context.Background()}

	buf := make([]byte, len(src))
	n, err := ra.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(src) || !bytes.Equal(buf, src) {
		t.Fatalf("ReadAt returned %d bytes, data mismatch=%v", n, !bytes.Equal(buf, src))
	}

	var total int
	for _, c := range lim.calls {
		if c > lim.burst {
			t.Fatalf("WaitN called with n=%d exceeding burst=%d", c, lim.burst)
		}
		total += c
	}
	if total != len(src) {
		t.Fatalf("limiter saw %d total bytes, want %d", total, len(src))
	}
}

func TestLimitedReaderAtPropagatesLimiterError(t *testing.T) {
	lim := &fakeLimiter{burst: 10, err: context.DeadlineExceeded}
	src := bytes.Repeat([]byte("z"), 10)
	ra := &LimitedReaderAt{R: bytes.NewReader(src), Limiter: lim, Ctx: context.Background()}

	n, err := ra.ReadAt(make([]byte, 10), 0)
	if n != 10 {
		t.Fatalf("n = %d, want 10 (the underlying read still happened)", n)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}
