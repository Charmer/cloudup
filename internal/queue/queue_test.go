package queue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloudup/internal/history"
	"cloudup/internal/provider"
)

// fakeProvider lets each test control Upload's behavior directly.
type fakeProvider struct {
	typeName string
	uploadFn func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error)
}

func (p fakeProvider) Type() string                                                { return p.typeName }
func (p fakeProvider) DisplayName() string                                         { return p.typeName }
func (p fakeProvider) TestConnection(ctx context.Context) error                    { return nil }
func (p fakeProvider) Download(ctx context.Context, t provider.DownloadTask) error { return nil }
func (p fakeProvider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	return nil, nil
}
func (p fakeProvider) Delete(ctx context.Context, remotePath string) error { return nil }
func (p fakeProvider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	return p.uploadFn(ctx, task)
}

// readingUploader is a fakeProvider Upload behavior that reads the whole
// body (like a real provider would while streaming) and returns its SHA256.
func readingUploader() func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	return func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		h := sha256.New()
		if _, err := io.Copy(h, task.Reader); err != nil {
			return provider.UploadResult{}, err
		}
		if task.Progress != nil {
			task.Progress(task.Size, task.Size)
		}
		return provider.UploadResult{
			RemotePath:   task.RemotePath,
			ChecksumAlgo: "sha256",
			Checksum:     hex.EncodeToString(h.Sum(nil)),
		}, nil
	}
}

// verifyingProvider additionally implements provider.ChecksumVerifier, so
// tests can observe whether Manager's optional post-upload verification
// (SetVerifyAfterUpload) actually calls it.
type verifyingProvider struct {
	fakeProvider
	verifyFn func(ctx context.Context, remotePath, algo, checksum string) (bool, error)
}

func (p verifyingProvider) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	return p.verifyFn(ctx, remotePath, algo, checksum)
}

func openTestHistory(t *testing.T) *history.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := history.Open(path)
	if err != nil {
		t.Fatalf("history.Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func req(localPath, remotePath string, data []byte) UploadRequest {
	return UploadRequest{
		LocalPath:  localPath,
		RemotePath: remotePath,
		Size:       int64(len(data)),
		Open:       func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	}
}

func waitForEvent(t *testing.T, events <-chan Event, timeout time.Duration, match func(Event) bool) Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e := <-events:
			if match(e) {
				return e
			}
		case <-deadline:
			t.Fatal("timed out waiting for matching event")
		}
	}
}

func fastRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: 5 * time.Millisecond, MaxDelay: 10 * time.Millisecond}
}

func TestEnqueueUploadsSuccessfully(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	p := fakeProvider{typeName: "fake", uploadFn: readingUploader()}

	id := m.Enqueue("conn1", p, req("/local/a.txt", "/a.txt", []byte("hello")))

	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusSuccess
	})

	page, err := h.List(context.Background(), history.Filter{ProviderID: "conn1"})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	entries := page.Entries
	if len(entries) != 1 || entries[0].Status != history.StatusSuccess {
		t.Fatalf("history entries = %+v, want single success entry", entries)
	}
	if entries[0].Checksum == "" {
		t.Fatal("history entry missing checksum")
	}
}

func TestSequentialWithinProvider(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	var mu sync.Mutex
	var order []string
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		order = append(order, task.RemotePath)
		mu.Unlock()
		return provider.UploadResult{}, nil
	}}

	var ids []string
	for i := 0; i < 5; i++ {
		remote := "/f" + string(rune('a'+i))
		ids = append(ids, m.Enqueue("conn1", p, req("/local"+remote, remote, []byte("x"))))
	}

	for _, id := range ids {
		waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
			return e.TaskID == id && e.Status == StatusSuccess
		})
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"/fa", "/fb", "/fc", "/fd", "/fe"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestParallelAcrossProviders(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	started := make(chan string, 2)
	release := make(chan struct{})
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		started <- task.RemotePath
		<-release
		return provider.UploadResult{}, nil
	}}

	m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))
	m.Enqueue("conn2", p, req("/local/b", "/b", []byte("x")))

	seen := map[string]bool{}
	timeout := time.After(time.Second)
	for len(seen) < 2 {
		select {
		case remote := <-started:
			seen[remote] = true
		case <-timeout:
			t.Fatalf("only saw %v starting within timeout, want both providers running in parallel", seen)
		}
	}
	close(release)
}

func TestSetConcurrencyAllowsParallelWithinProvider(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	m.SetConcurrency(2)

	started := make(chan string, 3)
	release := make(chan struct{})
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		started <- task.RemotePath
		<-release
		return provider.UploadResult{}, nil
	}}

	m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))
	m.Enqueue("conn1", p, req("/local/b", "/b", []byte("x")))
	m.Enqueue("conn1", p, req("/local/c", "/c", []byte("x")))

	seen := map[string]bool{}
	timeout := time.After(time.Second)
	for len(seen) < 2 {
		select {
		case remote := <-started:
			seen[remote] = true
		case <-timeout:
			t.Fatalf("only saw %v starting within timeout, want 2 concurrent uploads within one provider", seen)
		}
	}

	// The third task must stay pending - concurrency is capped at 2.
	select {
	case remote := <-started:
		t.Fatalf("third task %q started before either of the first two finished, concurrency limit not enforced", remote)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
}

func TestSetConcurrencyUpdatesAlreadyRunningQueue(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy()) // starts at concurrency 1

	started := make(chan string, 2)
	release := make(chan struct{})
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		started <- task.RemotePath
		<-release
		return provider.UploadResult{}, nil
	}}

	m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))
	m.Enqueue("conn1", p, req("/local/b", "/b", []byte("x")))

	<-started // first task is running; second is stuck pending at concurrency 1

	m.SetConcurrency(2)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("second task did not start after raising concurrency on a queue that already had a task in flight")
	}

	close(release)
}

func TestVerifyAfterUploadOffByDefault(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	// SetVerifyAfterUpload deliberately not called - default must be off.

	var verifyCalls atomic.Int32
	p := verifyingProvider{
		fakeProvider: fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
			return provider.UploadResult{Checksum: "abc", ChecksumAlgo: "sha256"}, nil
		}},
		verifyFn: func(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
			verifyCalls.Add(1)
			return true, nil
		},
	}

	id := m.Enqueue("conn1", p, req("/local/a.txt", "/a.txt", []byte("hello")))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusSuccess
	})

	time.Sleep(50 * time.Millisecond) // give a wrongly-firing verify goroutine a chance to run
	if n := verifyCalls.Load(); n != 0 {
		t.Fatalf("VerifyChecksum called %d times, want 0 (verify-after-upload defaults to off)", n)
	}
}

func TestVerifyAfterUploadUpdatesHistory(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	m.SetVerifyAfterUpload(map[string]bool{"fake": true})

	var verifyCalls atomic.Int32
	p := verifyingProvider{
		fakeProvider: fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
			return provider.UploadResult{Checksum: "abc", ChecksumAlgo: "sha256"}, nil
		}},
		verifyFn: func(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
			verifyCalls.Add(1)
			return true, nil
		},
	}

	id := m.Enqueue("conn1", p, req("/local/a.txt", "/a.txt", []byte("hello")))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusSuccess
	})

	// finish() kicks off verification in its own goroutine after emitting
	// the terminal event (see its doc comment for why), so poll briefly
	// for the history record to pick up the check result.
	deadline := time.After(time.Second)
	for {
		page, err := h.List(context.Background(), history.Filter{ProviderID: "conn1"})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		entries := page.Entries
		if len(entries) == 1 && entries[0].LastCheckStatus == history.CheckOK {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("history entry never got verified: %+v", entries)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if n := verifyCalls.Load(); n != 1 {
		t.Fatalf("VerifyChecksum called %d times, want 1", n)
	}
}

// TestVerifyAfterUploadIsPerProviderType is the actual point of keying the
// setting by provider type instead of one global switch: enabling it for
// "fake-cheap" must never trigger a verification for an upload through
// "fake-expensive", even though both run through the same Manager at once.
func TestVerifyAfterUploadIsPerProviderType(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	m.SetVerifyAfterUpload(map[string]bool{"fake-cheap": true})

	var cheapCalls, expensiveCalls atomic.Int32
	cheap := verifyingProvider{
		fakeProvider: fakeProvider{typeName: "fake-cheap", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
			return provider.UploadResult{Checksum: "abc", ChecksumAlgo: "sha256"}, nil
		}},
		verifyFn: func(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
			cheapCalls.Add(1)
			return true, nil
		},
	}
	expensive := verifyingProvider{
		fakeProvider: fakeProvider{typeName: "fake-expensive", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
			return provider.UploadResult{Checksum: "abc", ChecksumAlgo: "sha256"}, nil
		}},
		verifyFn: func(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
			expensiveCalls.Add(1)
			return true, nil
		},
	}

	id1 := m.Enqueue("conn-cheap", cheap, req("/local/a.txt", "/a.txt", []byte("hello")))
	id2 := m.Enqueue("conn-expensive", expensive, req("/local/b.txt", "/b.txt", []byte("world")))
	// Both run concurrently (different provider queues) on one shared
	// events channel - waiting for each ID in its own waitForEvent call
	// would race, since whichever call runs first discards *any*
	// non-matching event, including the other task's terminal one. A
	// single loop tracking both IDs avoids that.
	seen := map[string]bool{}
	deadline := time.After(time.Second)
	for !seen[id1] || !seen[id2] {
		select {
		case e := <-m.Events():
			if (e.TaskID == id1 || e.TaskID == id2) && e.Status == StatusSuccess {
				seen[e.TaskID] = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for both uploads to succeed: seen = %v", seen)
		}
	}

	verifyDeadline := time.After(time.Second)
	for {
		if cheapCalls.Load() == 1 {
			break
		}
		select {
		case <-verifyDeadline:
			t.Fatal("fake-cheap was never verified despite being enabled for it")
		case <-time.After(10 * time.Millisecond):
		}
	}

	time.Sleep(50 * time.Millisecond) // give a wrongly-firing verify goroutine a chance to run
	if n := expensiveCalls.Load(); n != 0 {
		t.Fatalf("VerifyChecksum called %d times for fake-expensive, want 0 (only fake-cheap is enabled)", n)
	}
}

func TestRetryOnFailureThenSucceeds(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	var attempts atomic.Int32
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		n := attempts.Add(1)
		if n < 3 {
			return provider.UploadResult{}, errors.New("transient failure")
		}
		return provider.UploadResult{Checksum: "ok"}, nil
	}}

	id := m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))

	e := waitForEvent(t, m.Events(), 2*time.Second, func(e Event) bool {
		return e.TaskID == id && (e.Status == StatusSuccess || e.Status == StatusError)
	})
	if e.Status != StatusSuccess {
		t.Fatalf("final status = %v, want %v", e.Status, StatusSuccess)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestRetryExhaustedReportsError(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, RetryPolicy{MaxAttempts: 2, BaseDelay: 5 * time.Millisecond, MaxDelay: 10 * time.Millisecond})

	wantErr := errors.New("permanent failure")
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, wantErr
	}}

	id := m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))

	e := waitForEvent(t, m.Events(), 2*time.Second, func(e Event) bool {
		return e.TaskID == id && (e.Status == StatusSuccess || e.Status == StatusError)
	})
	if e.Status != StatusError {
		t.Fatalf("final status = %v, want %v", e.Status, StatusError)
	}

	page, err := h.List(context.Background(), history.Filter{ProviderID: "conn1"})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	entries := page.Entries
	if len(entries) != 1 || entries[0].Status != history.StatusError {
		t.Fatalf("history entries = %+v, want single error entry", entries)
	}
}

func TestCancelPendingTask(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	block := make(chan struct{})
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		<-block
		return provider.UploadResult{}, nil
	}}

	firstID := m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == firstID && e.Status == StatusUploading
	})

	secondID := m.Enqueue("conn1", p, req("/local/b", "/b", []byte("x")))
	m.CancelTask("conn1", secondID)

	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == secondID && e.Status == StatusCancelled
	})

	close(block)
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == firstID && e.Status == StatusSuccess
	})
}

func TestCancelInFlightTask(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		<-ctx.Done()
		return provider.UploadResult{}, ctx.Err()
	}}

	id := m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusUploading
	})

	m.CancelTask("conn1", id)

	e := waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && (e.Status == StatusCancelled || e.Status == StatusError)
	})
	if e.Status != StatusCancelled {
		t.Fatalf("status after cancel = %v, want %v", e.Status, StatusCancelled)
	}
}

func TestPauseStopsNewTasksUntilResume(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	block := make(chan struct{})
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		<-block
		return provider.UploadResult{}, nil
	}}

	firstID := m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == firstID && e.Status == StatusUploading
	})

	m.Pause("conn1")
	close(block) // let the first task finish; the queue should not start the second while paused

	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == firstID && e.Status == StatusSuccess
	})

	secondID := m.Enqueue("conn1", p, req("/local/b", "/b", []byte("x")))

	deadline := time.After(150 * time.Millisecond)
waitWhilePaused:
	for {
		select {
		case e := <-m.Events():
			if e.TaskID == secondID && e.Status == StatusUploading {
				t.Fatal("second task started while queue was paused")
			}
		case <-deadline:
			break waitWhilePaused
		}
	}

	m.Resume("conn1")
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == secondID && e.Status == StatusSuccess
	})
}

func TestShutdownCancelsInFlightAndReturns(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		<-ctx.Done()
		return provider.UploadResult{}, ctx.Err()
	}}

	id := m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusUploading
	})

	go func() {
		for range m.Events() {
			// drain so finish() doesn't block on a full/abandoned channel
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

// TestPauseBeforeFirstEnqueueIsHonoured is a regression test for a bug
// found while adding internal/httpapi's test suite: Pause used to be a
// silent no-op for a provider whose queue did not exist yet.
//
// Queues are created lazily on first Enqueue, and Pause only touched an
// existing one - so the natural client sequence "pause this connection,
// then add files to it" lost the pause entirely and the files uploaded
// immediately. Through the REST API that looked especially wrong: the
// pause call returned 204, so nothing indicated it had been dropped.
func TestPauseBeforeFirstEnqueueIsHonoured(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	started := make(chan string, 4)
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		started <- task.RemotePath
		return provider.UploadResult{}, nil
	}}

	// Pause before this connection has ever been used - no queue exists yet.
	m.Pause("conn-never-used")

	id := m.Enqueue("conn-never-used", p, req("/local/a", "/a", []byte("x")))

	select {
	case path := <-started:
		t.Fatalf("upload of %q started while the connection was paused before its first enqueue", path)
	case <-time.After(150 * time.Millisecond):
		// Correct: still held.
	}

	m.Resume("conn-never-used")
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusSuccess
	})
}

// TestResumeBeforeFirstEnqueueDoesNotPause guards the mirror case: a
// Resume recorded before the queue exists must not leave it paused.
func TestResumeBeforeFirstEnqueueDoesNotPause(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}

	m.Pause("conn1")
	m.Resume("conn1")

	id := m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusSuccess
	})
}

// TestHasActiveTasks covers the check DELETE /api/v1/connections/{id}
// relies on to refuse deleting a connection with work still in flight -
// see internal/httpapi/connections.go's handleConnectionsDelete.
func TestHasActiveTasks(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	t.Cleanup(func() { m.Shutdown(context.Background()) })

	if m.HasActiveTasks("conn1") {
		t.Fatal("HasActiveTasks() = true for a provider never enqueued to, want false")
	}

	block := make(chan struct{})
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		<-block
		return provider.UploadResult{}, nil
	}}

	id := m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusUploading
	})
	if !m.HasActiveTasks("conn1") {
		t.Fatal("HasActiveTasks() = false for a provider with a task in flight, want true")
	}

	close(block)
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusSuccess
	})
	// The completion goroutine removes the task from q.current after
	// emitting the terminal event, not before - poll briefly rather than
	// asserting immediately after the event to avoid a flaky race.
	deadline := time.After(time.Second)
	for m.HasActiveTasks("conn1") {
		select {
		case <-deadline:
			t.Fatal("HasActiveTasks() stayed true after the only task finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestSweepIdleQueuesEvictsAfterTwoIdleTicks locks in sweepIdleQueues'
// two-observation eviction rule (see its doc comment): a queue is only
// removed once it has been seen idle on two consecutive sweeps, and a
// queue that is busy on the second sweep must not be evicted just because
// it happened to be idle on the first.
func TestSweepIdleQueuesEvictsAfterTwoIdleTicks(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	t.Cleanup(func() { m.Shutdown(context.Background()) })

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}

	id := m.Enqueue("conn1", p, req("/local/a", "/a", []byte("x")))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusSuccess
	})
	for m.HasActiveTasks("conn1") {
		time.Sleep(5 * time.Millisecond)
	}

	m.mu.Lock()
	_, exists := m.queues["conn1"]
	m.mu.Unlock()
	if !exists {
		t.Fatal("providerQueue for conn1 vanished before any sweep ran")
	}

	m.sweepIdleQueues() // 1st idle observation - not evicted yet
	m.mu.Lock()
	_, exists = m.queues["conn1"]
	m.mu.Unlock()
	if !exists {
		t.Fatal("sweepIdleQueues() evicted conn1 on its first idle observation, want it to wait for a second")
	}

	m.sweepIdleQueues() // 2nd consecutive idle observation - now evicted
	m.mu.Lock()
	_, exists = m.queues["conn1"]
	m.mu.Unlock()
	if exists {
		t.Fatal("sweepIdleQueues() did not evict conn1 after two consecutive idle observations")
	}

	// A queue that's busy is never evicted, however many sweeps run.
	block := make(chan struct{})
	busy := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		<-block
		return provider.UploadResult{}, nil
	}}
	busyID := m.Enqueue("conn2", busy, req("/local/b", "/b", []byte("x")))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == busyID && e.Status == StatusUploading
	})

	m.sweepIdleQueues()
	m.sweepIdleQueues()
	m.mu.Lock()
	_, exists = m.queues["conn2"]
	m.mu.Unlock()
	if !exists {
		t.Fatal("sweepIdleQueues() evicted conn2 while a task was still in flight")
	}
	close(block)
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == busyID && e.Status == StatusSuccess
	})
}

// TestSetIdleQueueSweepIntervalClampsAndApplies covers
// SetIdleQueueSweepInterval's floor (a value below
// idleQueueSweepIntervalFloor is raised, not rejected) and that the change
// is visible to sweepIdleQueuesLoop's next read - see
// idleQueueSweepInterval, the unexported getter both use.
func TestSetIdleQueueSweepIntervalClampsAndApplies(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	t.Cleanup(func() { m.Shutdown(context.Background()) })

	if got := m.idleQueueSweepInterval(); got != DefaultIdleQueueSweepInterval {
		t.Fatalf("initial interval = %v, want default %v", got, DefaultIdleQueueSweepInterval)
	}

	m.SetIdleQueueSweepInterval(30 * time.Second)
	if got := m.idleQueueSweepInterval(); got != idleQueueSweepIntervalFloor {
		t.Errorf("SetIdleQueueSweepInterval(30s) = %v, want clamped up to the floor %v", got, idleQueueSweepIntervalFloor)
	}

	m.SetIdleQueueSweepInterval(5 * time.Minute)
	if got := m.idleQueueSweepInterval(); got != 5*time.Minute {
		t.Errorf("SetIdleQueueSweepInterval(5m) = %v, want 5m (above the floor, kept as-is)", got)
	}
}

// multipartFakeProvider is a fakeProvider that also implements
// provider.MultipartUploader, recording which of the two paths the Manager
// chose and with what part size.
type multipartFakeProvider struct {
	fakeProvider

	mu            sync.Mutex
	singleCalls   int
	multiCalls    int
	lastPartSize  int64
	lastMultiSize int64
}

func (p *multipartFakeProvider) Upload(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
	p.mu.Lock()
	p.singleCalls++
	p.mu.Unlock()
	return p.fakeProvider.Upload(ctx, task)
}

func (p *multipartFakeProvider) UploadMultipart(ctx context.Context, task provider.UploadTask, partSize int64) (provider.UploadResult, error) {
	p.mu.Lock()
	p.multiCalls++
	p.lastPartSize = partSize
	p.lastMultiSize = task.Size
	p.mu.Unlock()
	return p.fakeProvider.Upload(ctx, task)
}

func (p *multipartFakeProvider) counts() (single, multi int, partSize int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.singleCalls, p.multiCalls, p.lastPartSize
}

func newMultipartFake() *multipartFakeProvider {
	return &multipartFakeProvider{
		fakeProvider: fakeProvider{typeName: "fake", uploadFn: readingUploader()},
	}
}

// TestMultipartChosenOnlyAboveThreshold pins the one place the core decides
// between a provider's single-request and chunked upload paths. Before this
// existed, provider.MultipartUploader was implemented (by s3) but never
// called by anything - and its own doc comment claimed the core dispatched
// to it, which was false.
func TestMultipartChosenOnlyAboveThreshold(t *testing.T) {
	t.Run("small file takes the single-request path", func(t *testing.T) {
		h := openTestHistory(t)
		m := NewManager(h, fastRetryPolicy())
		m.SetMultipart(1024, 256)
		p := newMultipartFake()

		id := m.Enqueue("conn1", p, req("/local/small", "/small", bytes.Repeat([]byte("x"), 100)))
		waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
			return e.TaskID == id && e.Status == StatusSuccess
		})

		single, multi, _ := p.counts()
		if single != 1 || multi != 0 {
			t.Fatalf("single=%d multi=%d, want single=1 multi=0", single, multi)
		}
	})

	t.Run("large file takes the chunked path with the configured part size", func(t *testing.T) {
		h := openTestHistory(t)
		m := NewManager(h, fastRetryPolicy())
		m.SetMultipart(1024, 256)
		p := newMultipartFake()

		body := bytes.Repeat([]byte("y"), 4096)
		id := m.Enqueue("conn1", p, req("/local/big", "/big", body))
		waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
			return e.TaskID == id && e.Status == StatusSuccess
		})

		single, multi, partSize := p.counts()
		if single != 0 || multi != 1 {
			t.Fatalf("single=%d multi=%d, want single=0 multi=1", single, multi)
		}
		if partSize != 256 {
			t.Fatalf("partSize = %d, want 256", partSize)
		}
	})

	t.Run("exactly at the threshold stays single-request", func(t *testing.T) {
		h := openTestHistory(t)
		m := NewManager(h, fastRetryPolicy())
		m.SetMultipart(1024, 256)
		p := newMultipartFake()

		id := m.Enqueue("conn1", p, req("/local/edge", "/edge", bytes.Repeat([]byte("z"), 1024)))
		waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
			return e.TaskID == id && e.Status == StatusSuccess
		})

		single, multi, _ := p.counts()
		if single != 1 || multi != 0 {
			t.Fatalf("at-threshold size: single=%d multi=%d, want single=1 multi=0", single, multi)
		}
	})
}

// TestProviderWithoutMultipartStillUploads is the compatibility guarantee:
// the type assertion must leave providers that implement only the base
// interface completely unaffected, however large the file.
func TestProviderWithoutMultipartStillUploads(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	m.SetMultipart(16, 8) // threshold far below the payload

	p := fakeProvider{typeName: "fake", uploadFn: readingUploader()}

	id := m.Enqueue("conn1", p, req("/local/big", "/big", bytes.Repeat([]byte("q"), 4096)))
	waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusSuccess
	})
}

// TestSetMultipartIgnoresNonPositiveValues documents the "adjust one knob
// without knowing the other" contract.
func TestSetMultipartIgnoresNonPositiveValues(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	m.SetMultipart(0, 0)
	if threshold, partSize := m.multipartSettings(); threshold != DefaultMultipartThreshold || partSize != DefaultMultipartPartSize {
		t.Fatalf("zero values changed settings: threshold=%d partSize=%d", threshold, partSize)
	}

	m.SetMultipart(4096, 0)
	if threshold, partSize := m.multipartSettings(); threshold != 4096 || partSize != DefaultMultipartPartSize {
		t.Fatalf("threshold-only update: threshold=%d partSize=%d", threshold, partSize)
	}
}

// readerAtCloser wraps a *bytes.Reader so a fake UploadRequest.Open can
// return something that implements io.ReaderAt in addition to
// io.ReadCloser. Plain req()'s io.NopCloser(bytes.NewReader(...)) erases
// ReaderAt (nopCloser only forwards Read) - production's *os.File-backed
// Open does not have that problem (see provider.UploadTask.ReaderAt's doc
// comment), so tests exercising the parallel dispatch path need this
// instead of req().
type readerAtCloser struct{ *bytes.Reader }

func (readerAtCloser) Close() error { return nil }

func reqReaderAt(localPath, remotePath string, data []byte) UploadRequest {
	return UploadRequest{
		LocalPath:  localPath,
		RemotePath: remotePath,
		Size:       int64(len(data)),
		Open:       func() (io.ReadCloser, error) { return readerAtCloser{bytes.NewReader(data)}, nil },
	}
}

// parallelFakeProvider additionally implements
// provider.ParallelMultipartUploader, recording how it was called.
type parallelFakeProvider struct {
	multipartFakeProvider

	mu            sync.Mutex
	parallelCalls int
	lastStreams   int
	lastReaderAt  bool
}

func (p *parallelFakeProvider) UploadMultipartParallel(ctx context.Context, task provider.UploadTask, partSize int64, streams int) (provider.UploadResult, error) {
	p.mu.Lock()
	p.parallelCalls++
	p.lastStreams = streams
	p.lastReaderAt = task.ReaderAt != nil
	p.mu.Unlock()
	return p.fakeProvider.Upload(ctx, task)
}

func (p *parallelFakeProvider) parallelCounts() (calls, streams int, hadReaderAt bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.parallelCalls, p.lastStreams, p.lastReaderAt
}

func newParallelFake() *parallelFakeProvider {
	return &parallelFakeProvider{multipartFakeProvider: *newMultipartFake()}
}

// TestParallelMultipartDispatch pins the three-tier dispatch order attempt
// implements: provider.ParallelMultipartUploader is only reached above the
// multi-thread threshold, with streams > 1, and with a ReaderAt-capable
// source - anything short of that falls back to the ordinary sequential
// provider.MultipartUploader path, which every case here still expects to
// succeed via (never the plain single-request Upload, since every payload
// used is already above the low sequential-multipart threshold too).
func TestParallelMultipartDispatch(t *testing.T) {
	t.Run("below multi-thread threshold uses sequential multipart", func(t *testing.T) {
		h := openTestHistory(t)
		m := NewManager(h, fastRetryPolicy())
		m.SetMultipart(256, 128)
		m.SetMultiThreadStreams(4096, 4) // threshold above the payload below

		p := newParallelFake()
		body := bytes.Repeat([]byte("y"), 1024)
		id := m.Enqueue("conn1", p, reqReaderAt("/local/big", "/big", body))
		waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
			return e.TaskID == id && e.Status == StatusSuccess
		})

		parallel, _, _ := p.parallelCounts()
		_, multi, _ := p.multipartFakeProvider.counts()
		if parallel != 0 || multi != 1 {
			t.Fatalf("parallel=%d multi=%d, want parallel=0 multi=1 below the multi-thread threshold", parallel, multi)
		}
	})

	t.Run("at/above multi-thread threshold with a ReaderAt source uses the parallel path", func(t *testing.T) {
		h := openTestHistory(t)
		m := NewManager(h, fastRetryPolicy())
		m.SetMultipart(256, 128)
		m.SetMultiThreadStreams(1024, 3)

		p := newParallelFake()
		body := bytes.Repeat([]byte("y"), 4096)
		id := m.Enqueue("conn1", p, reqReaderAt("/local/big", "/big", body))
		waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
			return e.TaskID == id && e.Status == StatusSuccess
		})

		calls, streams, hadReaderAt := p.parallelCounts()
		if calls != 1 {
			t.Fatalf("UploadMultipartParallel calls = %d, want 1", calls)
		}
		if streams != 3 {
			t.Fatalf("streams = %d, want 3", streams)
		}
		if !hadReaderAt {
			t.Fatal("UploadMultipartParallel called with task.ReaderAt == nil")
		}
	})

	t.Run("without a ReaderAt-capable source falls back to sequential multipart", func(t *testing.T) {
		h := openTestHistory(t)
		m := NewManager(h, fastRetryPolicy())
		m.SetMultipart(256, 128)
		m.SetMultiThreadStreams(1024, 3)

		p := newParallelFake()
		body := bytes.Repeat([]byte("y"), 4096)
		id := m.Enqueue("conn1", p, req("/local/big", "/big", body)) // plain req(): no ReaderAt
		waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
			return e.TaskID == id && e.Status == StatusSuccess
		})

		parallel, _, _ := p.parallelCounts()
		_, multi, _ := p.multipartFakeProvider.counts()
		if parallel != 0 || multi != 1 {
			t.Fatalf("parallel=%d multi=%d, want parallel=0 multi=1 when the source has no ReaderAt", parallel, multi)
		}
	})

	t.Run("streams == 1 disables the parallel path even above threshold", func(t *testing.T) {
		h := openTestHistory(t)
		m := NewManager(h, fastRetryPolicy())
		m.SetMultipart(256, 128)
		m.SetMultiThreadStreams(1024, 1)

		p := newParallelFake()
		body := bytes.Repeat([]byte("y"), 4096)
		id := m.Enqueue("conn1", p, reqReaderAt("/local/big", "/big", body))
		waitForEvent(t, m.Events(), time.Second, func(e Event) bool {
			return e.TaskID == id && e.Status == StatusSuccess
		})

		parallel, _, _ := p.parallelCounts()
		_, multi, _ := p.multipartFakeProvider.counts()
		if parallel != 0 || multi != 1 {
			t.Fatalf("parallel=%d multi=%d, want parallel=0 multi=1 when streams == 1", parallel, multi)
		}
	})
}

// TestSetMultiThreadStreamsIgnoresNonPositiveValues mirrors
// TestSetMultipartIgnoresNonPositiveValues: adjusting one knob must not
// disturb the other, and non-positive values are a no-op, not a reset.
func TestSetMultiThreadStreamsIgnoresNonPositiveValues(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())

	m.SetMultiThreadStreams(0, 0)
	if threshold, streams := m.multiThreadSettings(); threshold != DefaultMultiThreadThreshold || streams != DefaultMultiThreadStreams {
		t.Fatalf("zero values changed settings: threshold=%d streams=%d", threshold, streams)
	}

	m.SetMultiThreadStreams(0, 2)
	if threshold, streams := m.multiThreadSettings(); threshold != DefaultMultiThreadThreshold || streams != 2 {
		t.Fatalf("streams-only update: threshold=%d streams=%d", threshold, streams)
	}

	m.SetMultiThreadStreams(1<<20, 0)
	if threshold, streams := m.multiThreadSettings(); threshold != 1<<20 || streams != 2 {
		t.Fatalf("threshold-only update: threshold=%d streams=%d", threshold, streams)
	}

	// streams == 1 is a real, explicit value (it disables the parallel
	// path in attempt's dispatch, but is not a "leave unchanged" sentinel).
	m.SetMultiThreadStreams(0, 1)
	if _, streams := m.multiThreadSettings(); streams != 1 {
		t.Fatalf("streams = %d, want 1 (a valid explicit disable value)", streams)
	}
}

// TestDefaultMultipartThresholdIsBelowDropboxLimit guards the reason the
// default is 64 MiB and not something larger: Dropbox's single-request
// upload endpoint hard-fails above 150 MB, so a file between the threshold
// and that limit must already be taking the chunked path.
func TestDefaultMultipartThresholdIsBelowDropboxLimit(t *testing.T) {
	const dropboxSingleRequestLimit int64 = 150 << 20
	if DefaultMultipartThreshold >= dropboxSingleRequestLimit {
		t.Fatalf("DefaultMultipartThreshold = %d, must stay below Dropbox's %d limit", DefaultMultipartThreshold, dropboxSingleRequestLimit)
	}
}

// TestUploadBandwidthLimitThrottlesUpload verifies SetUploadBandwidthLimit
// actually slows down reads through task.Reader, not just that it can be
// called. 1500 bytes at a 1000 bytes/sec cap (burst == rate, so the first
// 1000 bytes are free) must take at least ~500ms for the remaining 500
// bytes - comfortably above scheduling noise while staying well under a
// second so the test suite stays fast.
func TestUploadBandwidthLimitThrottlesUpload(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	m.SetUploadBandwidthLimit(1000)

	p := fakeProvider{typeName: "fake", uploadFn: readingUploader()}
	data := bytes.Repeat([]byte("a"), 1500)

	start := time.Now()
	id := m.Enqueue("conn1", p, req("/local/a.bin", "/a.bin", data))
	waitForEvent(t, m.Events(), 5*time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusSuccess
	})
	elapsed := time.Since(start)

	const wantMin = 400 * time.Millisecond
	if elapsed < wantMin {
		t.Fatalf("upload finished in %s, want at least %s given the configured limit", elapsed, wantMin)
	}
}

// TestUploadBandwidthLimitZeroDisablesThrottling checks that a limit can be
// lifted again at runtime, matching SetMultiThreadStreams/SetConcurrency's
// pattern of applying live without a restart.
func TestUploadBandwidthLimitZeroDisablesThrottling(t *testing.T) {
	h := openTestHistory(t)
	m := NewManager(h, fastRetryPolicy())
	m.SetUploadBandwidthLimit(1000)
	m.SetUploadBandwidthLimit(0)

	p := fakeProvider{typeName: "fake", uploadFn: readingUploader()}
	data := bytes.Repeat([]byte("a"), 1_000_000)

	start := time.Now()
	id := m.Enqueue("conn1", p, req("/local/a.bin", "/a.bin", data))
	waitForEvent(t, m.Events(), 5*time.Second, func(e Event) bool {
		return e.TaskID == id && e.Status == StatusSuccess
	})
	elapsed := time.Since(start)

	const wantMax = 2 * time.Second
	if elapsed > wantMax {
		t.Fatalf("upload finished in %s, want well under %s once the limit is lifted", elapsed, wantMax)
	}
}
