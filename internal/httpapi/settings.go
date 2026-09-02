package httpapi

import (
	"net/http"
	"time"

	"cloudup/internal/settings"
)

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.Settings.Get()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleSettingsSet persists the new settings and, for the ones
// internal/queue.Manager can apply live (concurrency, verify-after-upload,
// multi-thread-streams, upload bandwidth limit, idle-connection timeout),
// pushes them into the running Manager immediately - no restart needed.
func (s *Server) handleSettingsSet(w http.ResponseWriter, r *http.Request) {
	var v settings.Settings
	if err := decodeJSON(r, &v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: %s", err)
		return
	}

	if err := s.Settings.Set(v); err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	saved, err := s.Settings.Get()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}

	s.Queue.SetConcurrency(saved.MaxConcurrentUploadsPerProvider)
	s.Queue.SetVerifyAfterUpload(saved.VerifyChecksumAfterUpload)
	s.Queue.SetMultiThreadStreams(saved.MultiThreadThresholdBytes, saved.MultiThreadStreams)
	s.Queue.SetUploadBandwidthLimit(saved.MaxUploadBytesPerSecond)
	s.Queue.SetIdleQueueSweepInterval(time.Duration(saved.IdleConnectionTimeoutMinutes) * time.Minute)

	writeJSON(w, http.StatusOK, saved)
}
