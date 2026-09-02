package httpapi

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"cloudup/internal/queue"
)

// taskSnapshot is one task's last known state, refreshed from
// queue.Manager.Events() - see taskTracker.consume. It mirrors queue.Event
// but adds ConnectionID as a JSON-friendly name and drops nothing, since
// GET /api/v1/tasks is a polling client's only view of upload progress
// (see server.go's doc comment on why polling was chosen over a socket).
type taskSnapshot struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connectionId"`
	LocalPath    string `json:"localPath"`
	RemotePath   string `json:"remotePath"`
	Status       string `json:"status"`
	Sent         int64  `json:"sent"`
	Total        int64  `json:"total"`
	Error        string `json:"error,omitempty"`
	HistoryID    int64  `json:"historyId,omitempty"`

	// terminalAt is when this task reached a final status (success, error
	// or cancelled), used only by evictFinished below to bound taskTracker's
	// memory - it is unexported so encoding/json (used by handleTasksList/
	// handleTasksGet) never reports it to clients.
	terminalAt time.Time
}

func (s taskSnapshot) isTerminal() bool {
	switch queue.Status(s.Status) {
	case queue.StatusSuccess, queue.StatusError, queue.StatusCancelled:
		return true
	default:
		return false
	}
}

// taskRetention and sweepInterval bound taskTracker's memory: cloudup can
// run for weeks as an embedded backend service, continuously uploading
// files, and every distinct TaskID ever
// seen would otherwise stay in the map forever - a slow, unbounded RAM leak
// for exactly that long-running scenario. A finished task is safe to drop
// once every reasonable poller has had time to observe its final state;
// the outcome is never lost, since it is already durably recorded in
// internal/history by the time this Event is emitted (see HistoryID).
const (
	taskRetention = 15 * time.Minute
	sweepInterval = time.Minute
)

// taskTracker mirrors queue.Manager's event stream into an in-memory map so
// HTTP handlers can answer polling GETs instantly instead of blocking on
// the channel themselves (only one goroutine may safely range over
// Events()).
type taskTracker struct {
	mu    sync.RWMutex
	tasks map[string]taskSnapshot
}

func newTaskTracker() *taskTracker {
	return &taskTracker{tasks: make(map[string]taskSnapshot)}
}

// consume owns the events channel exclusively (only one goroutine may range
// over queue.Manager.Events()) and, on the same loop, periodically sweeps
// long-finished tasks out of the map - see taskRetention. It returns once
// events is closed, which only happens at process shutdown.
func (t *taskTracker) consume(events <-chan queue.Event) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case e, ok := <-events:
			if !ok {
				return
			}
			t.record(e)
		case <-ticker.C:
			t.evictFinished()
		}
	}
}

func (t *taskTracker) record(e queue.Event) {
	snap := taskSnapshot{
		ID:           e.TaskID,
		ConnectionID: e.ProviderID,
		LocalPath:    e.LocalPath,
		RemotePath:   e.RemotePath,
		Status:       string(e.Status),
		Sent:         e.Sent,
		Total:        e.Total,
		HistoryID:    e.HistoryID,
	}
	if e.Err != nil {
		snap.Error = e.Err.Error()
	}
	if snap.isTerminal() {
		snap.terminalAt = time.Now()
	}

	t.mu.Lock()
	t.tasks[e.TaskID] = snap
	t.mu.Unlock()
}

func (t *taskTracker) evictFinished() {
	cutoff := time.Now().Add(-taskRetention)

	t.mu.Lock()
	defer t.mu.Unlock()
	for id, snap := range t.tasks {
		if !snap.terminalAt.IsZero() && snap.terminalAt.Before(cutoff) {
			delete(t.tasks, id)
		}
	}
}

func (t *taskTracker) list(connectionID string) []taskSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]taskSnapshot, 0, len(t.tasks))
	for _, snap := range t.tasks {
		if connectionID != "" && snap.ConnectionID != connectionID {
			continue
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (t *taskTracker) get(id string) (taskSnapshot, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	snap, ok := t.tasks[id]
	return snap, ok
}

func (s *Server) handleTasksList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.tasks.list(r.URL.Query().Get("connectionId")))
}

func (s *Server) handleTasksGet(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.tasks.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "task %q not found", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleTaskCancel needs the owning connection ID, which queue.Manager
// requires but a bare task ID doesn't carry - look it up from the last
// known snapshot rather than adding a connectionId param the client would
// have to track itself.
func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap, ok := s.tasks.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "task %q not found", id)
		return
	}
	s.Queue.CancelTask(snap.ConnectionID, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleQueuePause(w http.ResponseWriter, r *http.Request) {
	s.Queue.Pause(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleQueueResume(w http.ResponseWriter, r *http.Request) {
	s.Queue.Resume(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleQueueCancelAll(w http.ResponseWriter, r *http.Request) {
	s.Queue.CancelAll(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}
