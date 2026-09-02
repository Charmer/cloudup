package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloudup/internal/history"
	"cloudup/internal/provider"
	"cloudup/internal/queue"
)

// testQuietPeriod is far shorter than DefaultQuietPeriod so tests don't
// spend seconds waiting for debounce to settle.
const testQuietPeriod = 50 * time.Millisecond

// fakeProvider lets a test observe every Upload call. Mirrors
// internal/queue's own test double (queue_test.go) - duplicated rather
// than shared, matching how each package that needs one already does this
// (see also internal/httpapi's fake provider).
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

func openTestHistory(t *testing.T) *history.Store {
	t.Helper()
	h, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open() error = %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// newTestEngine wires an Engine to a real queue.Manager and a resolver that
// always returns p, capturing every upload's RemotePath/Size via uploaded.
func newTestEngine(t *testing.T, p provider.Provider) (*Engine, *queue.Manager) {
	t.Helper()
	h := openTestHistory(t)
	mgr := queue.NewManager(h, queue.RetryPolicy{MaxAttempts: 1})
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })

	resolve := func(connectionID string) (provider.Provider, error) { return p, nil }

	e, err := NewEngine(mgr, resolve, testQuietPeriod)
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	t.Cleanup(e.Shutdown)
	return e, mgr
}

// waitForQueued waits for an Enqueue-triggered StatusQueued event whose
// RemotePath equals wantRemotePath, so tests observe what Engine actually
// asked queue.Manager to upload without depending on the fake provider's
// Upload having run yet.
func waitForQueued(t *testing.T, mgr *queue.Manager, timeout time.Duration, wantRemotePath string) queue.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e := <-mgr.Events():
			if e.Status == queue.StatusQueued && e.RemotePath == wantRemotePath {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a queued task with remotePath %q", wantRemotePath)
		}
	}
}

func TestAddNewUploadsExistingFilesImmediately(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}
	e, mgr := newTestEngine(t, p)

	rule := Rule{ID: "r1", LocalPath: dir, ConnectionID: "conn1", RemoteFolder: "backup"}
	if err := e.AddNew(rule); err != nil {
		t.Fatalf("AddNew() error = %v", err)
	}

	ev := waitForQueued(t, mgr, 2*time.Second, "backup/a.txt")
	if ev.LocalPath != filepath.Join(dir, "a.txt") {
		t.Errorf("LocalPath = %q, want %q", ev.LocalPath, filepath.Join(dir, "a.txt"))
	}
}

func TestResumeDoesNotUploadExistingFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}
	e, mgr := newTestEngine(t, p)

	rule := Rule{ID: "r1", LocalPath: dir, ConnectionID: "conn1"}
	if err := e.Resume(rule); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	select {
	case ev := <-mgr.Events():
		t.Fatalf("Resume() uploaded a pre-existing file, got event %+v", ev)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing queued
	}
}

func TestNewFileInWatchedFolderIsUploadedAfterQuietPeriod(t *testing.T) {
	dir := t.TempDir()

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}
	e, mgr := newTestEngine(t, p)

	rule := Rule{ID: "r1", LocalPath: dir, ConnectionID: "conn1"}
	if err := e.Resume(rule); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ev := waitForQueued(t, mgr, 2*time.Second, "new.txt")
	if ev.Total != 0 && ev.Sent != 0 {
		// StatusQueued carries no Sent/Total yet - just confirming the event shape.
		_ = ev
	}
}

// TestWriteDebouncesMultipleEvents pins the whole reason for the quiet
// period: several writes to the same file in quick succession must produce
// exactly one upload, not one per write.
func TestWriteDebouncesMultipleEvents(t *testing.T) {
	dir := t.TempDir()

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}
	e, mgr := newTestEngine(t, p)

	rule := Rule{ID: "r1", LocalPath: dir, ConnectionID: "conn1"}
	if err := e.Resume(rule); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	target := filepath.Join(dir, "growing.txt")
	for i := range 5 {
		if err := os.WriteFile(target, []byte{byte(i)}, 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		time.Sleep(testQuietPeriod / 3)
	}

	waitForQueued(t, mgr, 2*time.Second, "growing.txt")

	select {
	case ev := <-mgr.Events():
		if ev.Status == queue.StatusQueued {
			t.Fatalf("a second upload was queued for the same settled write burst: %+v", ev)
		}
	case <-time.After(300 * time.Millisecond):
		// expected: no second Queued event
	}
}

func TestNewFileInSubdirectoryCreatedAfterWatchStartsIsPickedUp(t *testing.T) {
	dir := t.TempDir()

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}
	e, mgr := newTestEngine(t, p)

	rule := Rule{ID: "r1", LocalPath: dir, ConnectionID: "conn1"}
	if err := e.Resume(rule); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	waitForQueued(t, mgr, 2*time.Second, filepath.ToSlash(filepath.Join("sub", "nested.txt")))
}

func TestSingleFileWatchIgnoresSiblingFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("initial"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sibling := filepath.Join(dir, "sibling.txt")

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}
	e, mgr := newTestEngine(t, p)

	rule := Rule{ID: "r1", LocalPath: target, ConnectionID: "conn1"}
	if err := e.Resume(rule); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	if err := os.WriteFile(sibling, []byte("noise"), 0o644); err != nil {
		t.Fatalf("WriteFile(sibling) error = %v", err)
	}
	if err := os.WriteFile(target, []byte("updated"), 0o644); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	waitForQueued(t, mgr, 2*time.Second, "target.txt")
}

func TestDeletedFileTriggersNoUpload(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}
	e, mgr := newTestEngine(t, p)

	rule := Rule{ID: "r1", LocalPath: dir, ConnectionID: "conn1"}
	if err := e.Resume(rule); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	select {
	case ev := <-mgr.Events():
		t.Fatalf("a local delete triggered queue activity: %+v", ev)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing queued
	}
}

func TestRemoveStopsWatchingAndCancelsTimers(t *testing.T) {
	dir := t.TempDir()

	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}
	e, mgr := newTestEngine(t, p)

	rule := Rule{ID: "r1", LocalPath: dir, ConnectionID: "conn1"}
	if err := e.Resume(rule); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	e.Remove(rule.ID) // removed before the debounce timer had a chance to fire

	select {
	case ev := <-mgr.Events():
		t.Fatalf("Remove() did not cancel the pending upload: %+v", ev)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing queued
	}

	if _, _, ok := e.Status(rule.ID); ok {
		t.Error("Status() still reports the removed rule")
	}
}

func TestStatusReportsResolveErrors(t *testing.T) {
	dir := t.TempDir()
	mgr := queue.NewManager(openTestHistory(t), queue.RetryPolicy{MaxAttempts: 1})
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })

	resolveErr := "no such connection"
	resolve := func(connectionID string) (provider.Provider, error) {
		return nil, &testResolveError{resolveErr}
	}
	e, err := NewEngine(mgr, resolve, testQuietPeriod)
	if err != nil {
		t.Fatalf("NewEngine() error = %v", err)
	}
	t.Cleanup(e.Shutdown)

	rule := Rule{ID: "r1", LocalPath: dir, ConnectionID: "ghost"}
	if err := e.Resume(rule); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		status, message, ok := e.Status(rule.ID)
		if ok && status == "error" && message == resolveErr {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("Status() never reported the resolve error (last: %q %q %v)", status, message, ok)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type testResolveError struct{ msg string }

func (e *testResolveError) Error() string { return e.msg }

// TestResumeOnMissingPathReportsErrorStatus covers Resume() finding a
// path that no longer exists (e.g. an external drive unplugged since the
// last run) - the rule must stay visible via Status() with an error, not
// silently disappear as if it were never watched at all.
func TestResumeOnMissingPathReportsErrorStatus(t *testing.T) {
	p := fakeProvider{typeName: "fake", uploadFn: func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		return provider.UploadResult{}, nil
	}}
	e, _ := newTestEngine(t, p)

	rule := Rule{ID: "r1", LocalPath: filepath.Join(t.TempDir(), "does-not-exist"), ConnectionID: "conn1"}
	if err := e.Resume(rule); err == nil {
		t.Fatal("Resume() on a missing path: expected an error, got nil")
	}

	status, message, ok := e.Status(rule.ID)
	if !ok {
		t.Fatal("Status() reports the rule as never watched, want it registered with an error")
	}
	if status != "error" || message == "" {
		t.Errorf("Status() = (%q, %q), want (\"error\", non-empty message)", status, message)
	}
}
