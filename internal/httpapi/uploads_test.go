package httpapi

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloudup/internal/provider"
	"cloudup/internal/queue"
)

// TestUploadEnqueuesSpoolsAndSurfacesInTasks is the end-to-end path a
// browser takes: a multipart POST is accepted with 202 and a task ID, the
// body is spooled to disk (so a retry can reopen it), and the task shows up
// in the polling endpoints. Waiting is done by polling with a deadline
// rather than sleeping, so the test cannot become flaky on a slow machine.
func TestUploadEnqueuesSpoolsAndSurfacesInTasks(t *testing.T) {
	env := newTestEnv(t)

	conn := env.createConnection(fakeType, "Uploads", map[string]string{"url": "u"}, nil)

	// Hold the upload inside the provider so the spool file and the
	// in-progress task are both observable.
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	behaviorFor(t, conn.ID).upload = func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		if task.Progress != nil {
			task.Progress(task.Size, task.Size)
		}
		return provider.UploadResult{RemotePath: task.RemotePath, ChecksumAlgo: "sha256", Checksum: "abc"}, nil
	}

	payload := []byte("hello spooled world")
	body, contentType := multipartUpload(t, "hello.txt", "/remote/hello.txt", payload)
	accepted := decodeBody[map[string]string](t, env.serve(env.newUploadRequest(conn.ID, body, contentType)), http.StatusAccepted)
	taskID := accepted["taskId"]
	if taskID == "" {
		t.Fatal("upload response carries no taskId")
	}

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the provider's Upload was never called")
	}

	// The body was spooled to a reopenable file with the right contents.
	entries, err := os.ReadDir(env.SpoolDir)
	if err != nil {
		t.Fatalf("reading spool dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool dir has %d entries, want 1", len(entries))
	}
	spooled, err := os.ReadFile(env.SpoolDir + string(os.PathSeparator) + entries[0].Name())
	if err != nil {
		t.Fatalf("reading spool file: %v", err)
	}
	if string(spooled) != string(payload) {
		t.Errorf("spooled content = %q, want %q", spooled, payload)
	}

	// The task is visible through both polling endpoints, with the
	// connection and remote path a client needs to render it.
	var snap taskSnapshot
	waitFor(t, 3*time.Second, "the task to appear in GET /tasks", func() bool {
		for _, s := range decodeBody[[]taskSnapshot](t, env.do(http.MethodGet, "/api/v1/tasks", nil), http.StatusOK) {
			if s.ID == taskID {
				snap = s
				return true
			}
		}
		return false
	})
	if snap.ConnectionID != conn.ID {
		t.Errorf("task connectionId = %q, want %q", snap.ConnectionID, conn.ID)
	}
	if snap.RemotePath != "/remote/hello.txt" {
		t.Errorf("task remotePath = %q, want /remote/hello.txt", snap.RemotePath)
	}
	if snap.LocalPath != "hello.txt" {
		t.Errorf("task localPath = %q, want the uploaded filename", snap.LocalPath)
	}

	single := decodeBody[taskSnapshot](t, env.do(http.MethodGet, "/api/v1/tasks/"+taskID, nil), http.StatusOK)
	if single.ID != taskID {
		t.Errorf("GET /tasks/{id} returned task %q, want %q", single.ID, taskID)
	}

	// Filtering by another connection must not include it.
	other := decodeBody[[]taskSnapshot](t, env.do(http.MethodGet, "/api/v1/tasks?connectionId=someone-else", nil), http.StatusOK)
	for _, s := range other {
		if s.ID == taskID {
			t.Errorf("connectionId filter returned a task belonging to %q", s.ConnectionID)
		}
	}

	close(release)

	waitFor(t, 5*time.Second, "the task to reach success", func() bool {
		s := decodeBody[taskSnapshot](t, env.do(http.MethodGet, "/api/v1/tasks/"+taskID, nil), http.StatusOK)
		return s.Status == string(queue.StatusSuccess)
	})
	final := decodeBody[taskSnapshot](t, env.do(http.MethodGet, "/api/v1/tasks/"+taskID, nil), http.StatusOK)
	if final.Error != "" {
		t.Errorf("successful task carries an error: %q", final.Error)
	}
	if final.HistoryID == 0 {
		t.Error("successful task has no historyId - the client cannot link to the history entry")
	}
}

// TestUploadRemotePathDefaultsToFilename - the "remotePath" form field is
// optional per openapi.yaml; omitting it must fall back to the uploaded
// file's own name rather than enqueueing an empty remote path.
func TestUploadRemotePathDefaultsToFilename(t *testing.T) {
	env := newTestEnv(t)
	conn := env.createConnection(fakeType, "Defaults", map[string]string{"url": "u"}, nil)

	body, contentType := multipartUpload(t, "defaulted.bin", "", []byte("x"))
	accepted := decodeBody[map[string]string](t, env.serve(env.newUploadRequest(conn.ID, body, contentType)), http.StatusAccepted)

	var snap taskSnapshot
	waitFor(t, 3*time.Second, "the task to appear", func() bool {
		s, ok := env.Server.tasks.get(accepted["taskId"])
		snap = s
		return ok
	})
	if snap.RemotePath != "defaulted.bin" {
		t.Errorf("remotePath = %q, want the uploaded filename", snap.RemotePath)
	}
}

// TestUploadRejectsBadRequests - each rejection must be a 400/404 with a
// message, and must not leave a stray spool file behind.
func TestUploadRejectsBadRequests(t *testing.T) {
	env := newTestEnv(t)
	conn := env.createConnection(fakeType, "Rejects", map[string]string{"url": "u"}, nil)

	// Unknown connection.
	body, contentType := multipartUpload(t, "a.txt", "/a.txt", []byte("x"))
	errorMessage(t, env.serve(env.newUploadRequest("ghost", body, contentType)), http.StatusNotFound)

	// A connection whose provider cannot be constructed.
	broken := env.createConnection(fakeType, "Broken", map[string]string{"url": "u", "fail": "nope"}, nil)
	body, contentType = multipartUpload(t, "a.txt", "/a.txt", []byte("x"))
	errorMessage(t, env.serve(env.newUploadRequest(broken.ID, body, contentType)), http.StatusBadRequest)

	// Not a multipart body at all.
	req := env.newUploadRequest(conn.ID, strings.NewReader("plain body"), "text/plain")
	errorMessage(t, env.serve(req), http.StatusBadRequest)

	// Multipart, but without the "file" part.
	body, contentType = multipartUpload(t, "", "/a.txt", nil)
	msg := errorMessage(t, env.serve(env.newUploadRequest(conn.ID, body, contentType)), http.StatusBadRequest)
	if !strings.Contains(msg, "file") {
		t.Errorf("error = %q, want it to name the missing part", msg)
	}

	if entries, err := os.ReadDir(env.SpoolDir); err == nil && len(entries) != 0 {
		t.Errorf("rejected uploads left %d spool files behind", len(entries))
	}
}

// TestTaskCancelCancelsTheRunningUpload: cancel is looked up by task ID
// alone (the handler resolves the owning connection from the snapshot), so
// this also protects that lookup.
func TestTaskCancelCancelsTheRunningUpload(t *testing.T) {
	env := newTestEnv(t)
	conn := env.createConnection(fakeType, "Cancellable", map[string]string{"url": "u"}, nil)

	started := make(chan struct{}, 1)
	behaviorFor(t, conn.ID).upload = func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done() // cancelled by the queue
		return provider.UploadResult{}, ctx.Err()
	}

	body, contentType := multipartUpload(t, "big.bin", "/big.bin", []byte("payload"))
	accepted := decodeBody[map[string]string](t, env.serve(env.newUploadRequest(conn.ID, body, contentType)), http.StatusAccepted)
	taskID := accepted["taskId"]

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upload never started")
	}

	if rec := env.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/cancel", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d, want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	waitFor(t, 5*time.Second, "the task to leave the running state", func() bool {
		s := decodeBody[taskSnapshot](t, env.do(http.MethodGet, "/api/v1/tasks/"+taskID, nil), http.StatusOK)
		return s.Status == string(queue.StatusCancelled) || s.Status == string(queue.StatusError)
	})
}

// TestTaskEndpointsReport404ForUnknownTask - both reads and the cancel
// action, since cancel needs the snapshot to find the connection.
func TestTaskEndpointsReport404ForUnknownTask(t *testing.T) {
	env := newTestEnv(t)

	msg := errorMessage(t, env.do(http.MethodGet, "/api/v1/tasks/ghost", nil), http.StatusNotFound)
	if !strings.Contains(msg, "ghost") {
		t.Errorf("error = %q, want it to name the task", msg)
	}
	errorMessage(t, env.do(http.MethodPost, "/api/v1/tasks/ghost/cancel", nil), http.StatusNotFound)
}

// TestTasksListIsEmptyArrayNotNull pins a JSON shape clients trip over: an
// empty task list must serialize as [] so a client can iterate it without a
// null check.
func TestTasksListIsEmptyArrayNotNull(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(http.MethodGet, "/api/v1/tasks", nil)
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("empty task list body = %q, want []", got)
	}
}

// TestQueueControlEndpointsAcceptAnyConnectionID: pause/resume/cancel-all
// are idempotent queue controls with no failure mode to report - they
// answer 204 even for a connection that has never uploaded anything, which
// is what lets a client call them without tracking queue state.
func TestQueueControlEndpointsAcceptAnyConnectionID(t *testing.T) {
	env := newTestEnv(t)
	conn := env.createConnection(fakeType, "Controlled", map[string]string{"url": "u"}, nil)

	for _, id := range []string{conn.ID, "never-used-connection"} {
		for _, action := range []string{"pause", "resume", "cancel-all"} {
			rec := env.do(http.MethodPost, "/api/v1/connections/"+id+"/"+action, nil)
			if rec.Code != http.StatusNoContent {
				t.Errorf("POST %s for %q: status = %d, want 204 (body: %s)", action, id, rec.Code, rec.Body.String())
			}
		}
	}
}

// TestPauseHoldsUploadsUntilResume checks the pause control actually
// reaches queue.Manager: once a connection is paused it must not start
// further uploads, and resuming must let them through.
//
// Note the first upload: queue.Manager creates a connection's queue lazily
// on the first Enqueue, and Manager.Pause/Resume are no-ops for a
// connection that has no queue yet (queue.Manager.withQueue). So pausing
// *before* the first upload of a session is silently dropped and that
// upload runs anyway - see the report accompanying this suite. The test
// asserts the behavior that exists rather than the one that might be
// intended.
func TestPauseHoldsUploadsUntilResume(t *testing.T) {
	env := newTestEnv(t)
	conn := env.createConnection(fakeType, "Pausable", map[string]string{"url": "u"}, nil)

	started := make(chan string, 4)
	behaviorFor(t, conn.ID).upload = func(ctx context.Context, task provider.UploadTask) (provider.UploadResult, error) {
		started <- task.RemotePath
		return provider.UploadResult{RemotePath: task.RemotePath}, nil
	}

	upload := func(remotePath string) {
		t.Helper()
		body, contentType := multipartUpload(t, "f.txt", remotePath, []byte("x"))
		if rec := env.serve(env.newUploadRequest(conn.ID, body, contentType)); rec.Code != http.StatusAccepted {
			t.Fatalf("upload %s status = %d, want 202", remotePath, rec.Code)
		}
	}

	// First upload brings the connection's queue into existence.
	upload("/first.txt")
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first upload never started")
	}

	if rec := env.do(http.MethodPost, "/api/v1/connections/"+conn.ID+"/pause", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("pause status = %d, want 204", rec.Code)
	}

	upload("/paused.txt")
	select {
	case path := <-started:
		t.Fatalf("upload %s started while the connection was paused", path)
	case <-time.After(150 * time.Millisecond):
	}

	if rec := env.do(http.MethodPost, "/api/v1/connections/"+conn.ID+"/resume", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("resume status = %d, want 204", rec.Code)
	}
	select {
	case path := <-started:
		if path != "/paused.txt" {
			t.Errorf("resumed upload = %q, want /paused.txt", path)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upload did not start after resume")
	}
}
