package httpapi

import (
	"io"
	"net/http"
	"os"
	"time"

	"cloudup/internal/queue"
	"cloudup/internal/registry"
)

// maxUploadMemory is how much of a multipart/form-data body
// http.Request.ParseMultipartForm buffers in memory before spilling to a
// temp file itself; the file part is then copied into our own spool file
// regardless (see below), so this only bounds ParseMultipartForm's own
// scratch usage, not the eventual upload size.
const maxUploadMemory = 32 << 20 // 32 MiB

// handleUploadCreate accepts a browser-submitted file (multipart/form-data,
// field "file", plus a "remotePath" field) and enqueues it on
// internal/queue.Manager. The body is first copied into UploadSpoolDir
// because queue.UploadRequest.Open is called once per attempt - an
// HTTP request body can only be read once, but a retried upload needs to
// reopen its source, so the body is spooled to a file that can be reopened
// as many times as the retry policy needs.
func (s *Server) handleUploadCreate(w http.ResponseWriter, r *http.Request) {
	connectionID := r.PathValue("id")

	conn, err := s.Config.Get(connectionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}
	p, err := registry.Create(conn.ProviderType, conn.ProviderConfig, s.Secrets)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}

	if err := r.ParseMultipartForm(maxUploadMemory); err != nil {
		writeError(w, http.StatusBadRequest, "parsing multipart form: %s", err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing \"file\" part: %s", err)
		return
	}
	defer file.Close()

	remotePath := r.FormValue("remotePath")
	if remotePath == "" {
		remotePath = header.Filename
	}

	if err := os.MkdirAll(s.UploadSpoolDir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "preparing upload spool: %s", err)
		return
	}
	spoolFile, err := os.CreateTemp(s.UploadSpoolDir, "upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "preparing upload spool: %s", err)
		return
	}
	spoolPath := spoolFile.Name()
	size, err := io.Copy(spoolFile, file)
	closeErr := spoolFile.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(spoolPath)
		writeError(w, http.StatusInternalServerError, "spooling upload: %s", err)
		return
	}

	taskID := s.Queue.Enqueue(connectionID, p, queue.UploadRequest{
		LocalPath:  header.Filename,
		RemotePath: remotePath,
		Size:       size,
		Open:       func() (io.ReadCloser, error) { return os.Open(spoolPath) },
	})

	// Enqueue already sent a StatusQueued Event on the (buffered) events
	// channel, but that only means taskTracker.consume will *eventually*
	// process it - nothing guarantees it has by the time this handler
	// returns. Recording the same snapshot here too, synchronously, closes
	// a real race: without this, a client fast enough to call GET
	// /tasks/{id} or POST /tasks/{id}/cancel immediately after seeing this
	// 202 could get a spurious 404 for a task that unquestionably exists.
	// record() is safe to call from here - taskTracker's "one goroutine"
	// rule (see its doc comment) is about who may range over Events(),
	// not about who may write a snapshot; the real Queued event arriving
	// later just overwrites this with identical data.
	s.tasks.record(queue.Event{ProviderID: connectionID, TaskID: taskID, LocalPath: header.Filename, RemotePath: remotePath, Status: queue.StatusQueued})

	go s.cleanupSpoolWhenDone(taskID, spoolPath)

	writeJSON(w, http.StatusAccepted, map[string]string{"taskId": taskID})
}

// cleanupSpoolWhenDone removes the spooled file once taskID reaches a
// terminal state, polling the same taskTracker snapshot the /tasks
// endpoints read - there's no separate "task finished" channel to wait on
// without letting two goroutines range over queue.Manager.Events() at
// once, which Events()'s doc comment rules out.
func (s *Server) cleanupSpoolWhenDone(taskID, spoolPath string) {
	defer os.Remove(spoolPath)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		snap, ok := s.tasks.get(taskID)
		if !ok {
			continue
		}
		switch snap.Status {
		case string(queue.StatusSuccess), string(queue.StatusError), string(queue.StatusCancelled):
			return
		}
	}
}
