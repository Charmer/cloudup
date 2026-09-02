package httpapi

import (
	"net/http"

	"cloudup/internal/watch"
)

// watchRuleView is what GET /api/v1/watches returns for each rule: the
// persisted watch.Rule plus its live status from Engine. The two live in
// separate places (Store persists, Engine watches) but a client only ever
// needs them together, so they are combined into one response here rather
// than requiring a second request per rule.
type watchRuleView struct {
	watch.Rule
	Status        string `json:"status"` // "watching", "error", or "disabled"
	StatusMessage string `json:"statusMessage,omitempty"`
}

func (s *Server) toWatchRuleView(r watch.Rule) watchRuleView {
	view := watchRuleView{Rule: r}
	if !r.Enabled {
		view.Status = "disabled"
		return view
	}
	if s.WatchEngine == nil {
		view.Status = "error"
		view.StatusMessage = "folder-watch engine is unavailable on this server"
		return view
	}
	status, message, ok := s.WatchEngine.Status(r.ID)
	if !ok {
		// Enabled in the store but not (yet) registered with the engine -
		// only possible for the brief window inside handleWatchesCreate
		// between Store.Add and Engine.AddNew, or if Engine failed to
		// start at all (covered by the nil check above).
		view.Status = "error"
		view.StatusMessage = "not yet watching"
		return view
	}
	view.Status = status
	view.StatusMessage = message
	return view
}

// enabledWatchRuleFor reports the ID of an enabled rule that watches
// through connectionID, if any - used by handleConnectionsDelete to refuse
// deleting a connection a watch still depends on.
func (s *Server) enabledWatchRuleFor(connectionID string) (ruleID string, found bool) {
	rules, err := s.WatchStore.List()
	if err != nil {
		return "", false
	}
	for _, r := range rules {
		if r.Enabled && r.ConnectionID == connectionID {
			return r.ID, true
		}
	}
	return "", false
}

func (s *Server) handleWatchesList(w http.ResponseWriter, r *http.Request) {
	rules, err := s.WatchStore.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	views := make([]watchRuleView, len(rules))
	for i, rule := range rules {
		views[i] = s.toWatchRuleView(rule)
	}
	writeJSON(w, http.StatusOK, views)
}

// watchRuleRequest is the body of POST/PUT /api/v1/watches[/{id}]. Enabled
// is only meaningful for PUT - POST always creates an enabled rule (see
// watch.Store.Add), since a rule you just created is presumably meant to
// start watching immediately; pause it afterward via PUT if that's not
// what you wanted.
type watchRuleRequest struct {
	LocalPath    string `json:"localPath"`
	ConnectionID string `json:"connectionId"`
	RemoteFolder string `json:"remoteFolder"`
	Enabled      bool   `json:"enabled"`
}

// handleWatchesCreate persists a new rule and, unlike Resume at server
// startup, immediately uploads everything already present under
// req.LocalPath (watch.Engine.AddNew) - see internal/watch's package doc
// comment for why creation and restart behave differently. A bad path
// rolls the persisted rule back rather than leaving a broken one behind -
// Engine itself stays lenient about a path that goes missing *after* being
// valid (see engine.go's start), but a client's typo shouldn't get to
// create silently-broken persisted state in the first place.
func (s *Server) handleWatchesCreate(w http.ResponseWriter, r *http.Request) {
	if s.WatchEngine == nil {
		writeError(w, http.StatusServiceUnavailable, "folder-watch engine is unavailable on this server")
		return
	}

	var req watchRuleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: %s", err)
		return
	}
	if req.LocalPath == "" || req.ConnectionID == "" {
		writeError(w, http.StatusBadRequest, "localPath and connectionId are required")
		return
	}
	if _, err := s.Config.Get(req.ConnectionID); err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}

	rule, err := s.WatchStore.Add(req.LocalPath, req.ConnectionID, req.RemoteFolder)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	if err := s.WatchEngine.AddNew(rule); err != nil {
		_ = s.WatchStore.Remove(rule.ID)
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}

	writeJSON(w, http.StatusCreated, s.toWatchRuleView(rule))
}

func (s *Server) handleWatchesUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req watchRuleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: %s", err)
		return
	}
	if req.LocalPath == "" || req.ConnectionID == "" {
		writeError(w, http.StatusBadRequest, "localPath and connectionId are required")
		return
	}
	if _, err := s.Config.Get(req.ConnectionID); err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}

	rule, err := s.WatchStore.Update(id, req.LocalPath, req.ConnectionID, req.RemoteFolder, req.Enabled)
	if err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}

	if s.WatchEngine != nil {
		if err := s.WatchEngine.Update(rule); err != nil {
			// Persisted either way - a bad path here just surfaces as an
			// error Status, matching how Resume at startup behaves for a
			// path that stopped existing (see engine.go's start doc
			// comment). Unlike Create, there is no "roll back" for an
			// update: the rule already existed before this request.
			writeJSON(w, http.StatusOK, s.toWatchRuleView(rule))
			return
		}
	}
	writeJSON(w, http.StatusOK, s.toWatchRuleView(rule))
}

func (s *Server) handleWatchesDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.WatchStore.Remove(id); err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}
	if s.WatchEngine != nil {
		s.WatchEngine.Remove(id)
	}
	w.WriteHeader(http.StatusNoContent)
}
