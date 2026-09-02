package httpapi

import (
	"testing"
	"time"

	"cloudup/internal/queue"
)

// TestEvictFinishedRemovesOnlyOldTerminalTasks locks in taskTracker's
// bound on memory (see tasks.go's doc comment on taskRetention): a
// long-running embedded-backend process must not accumulate one
// taskSnapshot per upload forever.
func TestEvictFinishedRemovesOnlyOldTerminalTasks(t *testing.T) {
	tr := newTaskTracker()

	tr.record(queue.Event{ProviderID: "conn1", TaskID: "old-success", Status: queue.StatusSuccess})
	tr.record(queue.Event{ProviderID: "conn1", TaskID: "old-error", Status: queue.StatusError})
	tr.record(queue.Event{ProviderID: "conn1", TaskID: "recent-success", Status: queue.StatusSuccess})
	tr.record(queue.Event{ProviderID: "conn1", TaskID: "still-uploading", Status: queue.StatusUploading, Sent: 1, Total: 10})

	// Backdate the two tasks that should be evicted; leave the recent
	// terminal task and the still-running one alone.
	tr.mu.Lock()
	for _, id := range []string{"old-success", "old-error"} {
		snap := tr.tasks[id]
		snap.terminalAt = time.Now().Add(-taskRetention - time.Minute)
		tr.tasks[id] = snap
	}
	tr.mu.Unlock()

	tr.evictFinished()

	tr.mu.RLock()
	defer tr.mu.RUnlock()
	for _, id := range []string{"old-success", "old-error"} {
		if _, ok := tr.tasks[id]; ok {
			t.Errorf("evictFinished() did not remove old terminal task %q", id)
		}
	}
	for _, id := range []string{"recent-success", "still-uploading"} {
		if _, ok := tr.tasks[id]; !ok {
			t.Errorf("evictFinished() incorrectly removed %q", id)
		}
	}
}
