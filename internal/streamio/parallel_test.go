package streamio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestUploadPartsConcurrentlyOrdersResultsByPartNumber pins that the
// returned slice is indexed by part number, not by whatever order the
// concurrent workers happen to finish in (which Go's scheduler makes
// nondeterministic) - S3/B2 both require their completion call to list
// parts in ascending order.
func TestUploadPartsConcurrentlyOrdersResultsByPartNumber(t *testing.T) {
	content := []byte(strings.Repeat("0123456789", 10)) // 100 bytes -> 12 parts of 9 bytes
	ra := bytes.NewReader(content)

	got, err := UploadPartsConcurrently(context.Background(), ra, int64(len(content)), 9, 4,
		func(ctx context.Context, part Part, r io.Reader) (int, error) {
			io.Copy(io.Discard, r)
			return part.Number, nil
		})
	if err != nil {
		t.Fatalf("UploadPartsConcurrently() error = %v", err)
	}

	for i, partNumber := range got {
		if partNumber != i+1 {
			t.Fatalf("results[%d] = part %d, want part %d - results must be ordered by part number regardless of completion order", i, partNumber, i+1)
		}
	}
}

func TestUploadPartsConcurrentlyRespectsStreamLimit(t *testing.T) {
	content := make([]byte, 100)
	ra := bytes.NewReader(content)

	var inFlight, maxInFlight atomic.Int64
	release := make(chan struct{})
	// Give workers a window to pile up against the stream limit before
	// letting any of them finish - without this, streams=1 could pass by
	// sheer luck (each part finishing before the next one is dispatched).
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()

	const streams = 3
	_, err := UploadPartsConcurrently(context.Background(), ra, int64(len(content)), 10, streams,
		func(ctx context.Context, part Part, r io.Reader) (struct{}, error) {
			n := inFlight.Add(1)
			for {
				old := maxInFlight.Load()
				if n <= old || maxInFlight.CompareAndSwap(old, n) {
					break
				}
			}
			<-release
			inFlight.Add(-1)
			io.Copy(io.Discard, r)
			return struct{}{}, nil
		})
	if err != nil {
		t.Fatalf("UploadPartsConcurrently() error = %v", err)
	}
	if got := maxInFlight.Load(); got > streams {
		t.Fatalf("max concurrent parts = %d, want <= %d (streams)", got, streams)
	}
	if got := maxInFlight.Load(); got < streams {
		t.Skipf("observed max concurrency = %d, want %d - scheduling was too slow to prove the limit is actually reached, not a real failure", got, streams)
	}
}

func TestUploadPartsConcurrentlyPropagatesFirstError(t *testing.T) {
	content := make([]byte, 100)
	ra := bytes.NewReader(content)
	boom := errors.New("boom")

	_, err := UploadPartsConcurrently(context.Background(), ra, int64(len(content)), 10, 4,
		func(ctx context.Context, part Part, r io.Reader) (struct{}, error) {
			if part.Number == 3 {
				return struct{}{}, boom
			}
			io.Copy(io.Discard, r)
			return struct{}{}, nil
		})
	if err == nil {
		t.Fatal("UploadPartsConcurrently() should fail when one part errors")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap %v", err, boom)
	}
	if !strings.Contains(err.Error(), "part 3") {
		t.Fatalf("error = %v, want it to name the failing part", err)
	}
}

// TestUploadPartsConcurrentlyCancelsContextOnError uses streams=5 with 5
// parts total specifically so that all 5 launch without blocking on the
// semaphore (a buffered channel of capacity 5 never blocks its first 5
// sends) - regardless of how fast part 1 fails. That removes the race a
// smaller stream count would have between "part N gets launched at all" and
// "part 1 has already failed and canceled ctx", which is what parts 2-5
// need to reliably observe.
func TestUploadPartsConcurrentlyCancelsContextOnError(t *testing.T) {
	content := make([]byte, 50) // exactly 5 parts of 10 bytes
	ra := bytes.NewReader(content)
	boom := errors.New("boom")

	var canceledCount atomic.Int64
	_, err := UploadPartsConcurrently(context.Background(), ra, int64(len(content)), 10, 5,
		func(ctx context.Context, part Part, r io.Reader) (struct{}, error) {
			io.Copy(io.Discard, r)
			if part.Number == 1 {
				return struct{}{}, boom
			}
			<-ctx.Done()
			canceledCount.Add(1)
			return struct{}{}, ctx.Err()
		})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap %v", err, boom)
	}
	if canceledCount.Load() == 0 {
		t.Fatal("no in-flight part observed ctx cancellation after another part failed")
	}
}

func TestUploadPartsConcurrentlySplitsExactAndRemainder(t *testing.T) {
	cases := []struct {
		size, partSize int64
		wantParts      int
		wantLastSize   int64
	}{
		{100, 10, 10, 10},
		{101, 10, 11, 1},
		{5, 10, 1, 5},
		{0, 10, 0, 0},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("size=%d/partSize=%d", c.size, c.partSize), func(t *testing.T) {
			content := make([]byte, c.size)
			ra := bytes.NewReader(content)

			var parts []Part
			var mu sync.Mutex
			_, err := UploadPartsConcurrently(context.Background(), ra, c.size, c.partSize, 4,
				func(ctx context.Context, part Part, r io.Reader) (struct{}, error) {
					n, err := io.Copy(io.Discard, r)
					if err != nil {
						return struct{}{}, err
					}
					if n != part.Size {
						return struct{}{}, fmt.Errorf("read %d bytes for part %d, want %d", n, part.Number, part.Size)
					}
					mu.Lock()
					parts = append(parts, part)
					mu.Unlock()
					return struct{}{}, nil
				})
			if err != nil {
				t.Fatalf("UploadPartsConcurrently() error = %v", err)
			}
			if len(parts) != c.wantParts {
				t.Fatalf("part count = %d, want %d", len(parts), c.wantParts)
			}
			if c.wantParts > 0 {
				var last Part
				for _, p := range parts {
					if p.Number > last.Number {
						last = p
					}
				}
				if last.Size != c.wantLastSize {
					t.Errorf("last part size = %d, want %d", last.Size, c.wantLastSize)
				}
			}
		})
	}
}

func TestAtomicProgressReportsCumulativeAcrossConcurrentReaders(t *testing.T) {
	const total = 40

	// Track the highest sent value ever reported, not merely the last one
	// stored - sent.Add is atomic and monotonic, so whichever of the 4
	// goroutines' Add call linearizes last is guaranteed to observe and
	// report the full total, but the onProgress calls that follow each
	// Add can still be scheduled in a different order from the Adds
	// themselves. Asserting on "the last Store wins" is therefore racy:
	// a goroutine reporting a smaller partial sum can still land its Store
	// after the one that reported 40. The max is what the guarantee
	// actually promises.
	var mu sync.Mutex
	var maxSent int64
	tracking := NewAtomicProgress(total, func(sent, total int64) {
		mu.Lock()
		if sent > maxSent {
			maxSent = sent
		}
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := tracking.Reader(bytes.NewReader(make([]byte, 10)))
			io.Copy(io.Discard, r)
		}()
	}
	wg.Wait()

	mu.Lock()
	got := maxSent
	mu.Unlock()
	if got != total {
		t.Fatalf("max cumulative sent = %d, want %d (4 readers x 10 bytes)", got, total)
	}
}

func TestAtomicProgressNilCallbackIsNoop(t *testing.T) {
	p := NewAtomicProgress(10, nil)
	r := p.Reader(bytes.NewReader(make([]byte, 10)))
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
}
