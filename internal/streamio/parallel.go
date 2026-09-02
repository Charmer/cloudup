package streamio

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Part describes one contiguous byte range of a larger upload, as sliced by
// UploadPartsConcurrently.
type Part struct {
	Number int // 1-based, matching every provider's own part numbering
	Offset int64
	Size   int64
}

// splitParts divides size bytes into parts of partSize (the last part may
// be smaller), numbered from 1.
func splitParts(size, partSize int64) []Part {
	if size <= 0 {
		return nil
	}
	parts := make([]Part, 0, size/partSize+1)
	var offset int64
	for number := 1; offset < size; number++ {
		n := partSize
		if remaining := size - offset; remaining < n {
			n = remaining
		}
		parts = append(parts, Part{Number: number, Offset: offset, Size: n})
		offset += n
	}
	return parts
}

// UploadPartsConcurrently splits size bytes (read from ra, starting at
// offset 0) into parts of partSize and calls upload for each one, running
// up to streams of them at a time. upload receives an io.Reader restricted
// to exactly that part's byte range (an io.SectionReader over ra) and must
// fully consume it before returning.
//
// This is the shared engine behind every provider's
// provider.ParallelMultipartUploader implementation (S3, B2): it owns the
// concurrency bookkeeping - worker pool, first-error-wins cancellation,
// result ordering - so each provider package only supplies the
// protocol-specific per-part upload call.
//
// On success, the returned slice has one entry per part, ordered by part
// number (ascending, 1-based) regardless of which order the parts actually
// finished in - S3 and B2 both require their completion call to list parts
// in that order. On the first error from any part, ctx passed to every
// still-running upload call is canceled, remaining unlaunched parts are
// skipped, and the first error encountered (wrapped with its part number)
// is returned. Already-uploaded parts are not cleaned up here - that is a
// provider-specific concern (see e.g. s3's abort()/b2's cancelLargeFile()).
func UploadPartsConcurrently[R any](ctx context.Context, ra io.ReaderAt, size, partSize int64, streams int, upload func(ctx context.Context, part Part, r io.Reader) (R, error)) ([]R, error) {
	parts := splitParts(size, partSize)
	if len(parts) == 0 {
		return nil, nil
	}
	if streams < 1 {
		streams = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]R, len(parts))
	sem := make(chan struct{}, streams)
	var wg sync.WaitGroup
	var firstErr error
	var once sync.Once

partsLoop:
	for i, part := range parts {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// A previous part already failed (or the caller's context was
			// canceled): stop launching new work. What is already in
			// flight is still awaited below.
			break partsLoop
		}

		wg.Add(1)
		go func(i int, part Part) {
			defer wg.Done()
			defer func() { <-sem }()

			r := io.NewSectionReader(ra, part.Offset, part.Size)
			res, err := upload(ctx, part, r)
			if err != nil {
				once.Do(func() {
					firstErr = fmt.Errorf("part %d: %w", part.Number, err)
					cancel()
				})
				return
			}
			results[i] = res
		}(i, part)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

// AtomicProgress aggregates progress reports from multiple concurrent
// readers - the parallel part uploads a ParallelMultipartUploader
// implementation runs - into the single cumulative (sent, total) callback
// every provider's UploadTask.Progress expects. Plain ProgressReader
// assumes exactly one sequential reader and is not safe to share across
// goroutines; AtomicProgress is.
type AtomicProgress struct {
	sent       atomic.Int64
	total      int64
	onProgress func(sent, total int64)
}

// NewAtomicProgress returns an AtomicProgress reporting against total,
// invoking onProgress (which may be nil, matching UploadTask.Progress) as
// bytes are reported via Reader.
func NewAtomicProgress(total int64, onProgress func(sent, total int64)) *AtomicProgress {
	return &AtomicProgress{total: total, onProgress: onProgress}
}

// Reader wraps r so every Read reports its byte count through p - safe to
// call once per part, from as many concurrent goroutines as there are
// parts in flight, each wrapping a different io.SectionReader.
func (p *AtomicProgress) Reader(r io.Reader) io.Reader {
	return &atomicProgressReader{r: r, p: p}
}

type atomicProgressReader struct {
	r io.Reader
	p *AtomicProgress
}

func (a *atomicProgressReader) Read(buf []byte) (int, error) {
	n, err := a.r.Read(buf)
	if n > 0 && a.p.onProgress != nil {
		sent := a.p.sent.Add(int64(n))
		a.p.onProgress(sent, a.p.total)
	}
	return n, err
}
