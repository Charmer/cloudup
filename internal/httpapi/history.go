package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"cloudup/internal/history"
	"cloudup/internal/registry"
)

// historyPage is the GET /api/v1/history response envelope. It wraps
// history.Page rather than returning a bare array so a client can tell
// "this is the last page" from Total without a separate count request -
// see history.Page's doc comment. Limit/Offset echo history.Page's
// effective (post-clamp) values, not necessarily the caller's raw query
// params, so a client that asked for an out-of-range page size can tell
// what it actually got.
type historyPage struct {
	Entries []history.Entry `json:"entries"`
	Total   int             `json:"total"`
	Limit   int             `json:"limit"`
	Offset  int             `json:"offset"`
}

func (s *Server) handleHistoryList(w http.ResponseWriter, r *http.Request) {
	limit, err := parseIntParam(r, "limit", history.DefaultHistoryPageSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid limit: %s", err)
		return
	}
	offset, err := parseIntParam(r, "offset", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid offset: %s", err)
		return
	}

	filter := history.Filter{
		ProviderID: r.URL.Query().Get("connectionId"),
		Status:     r.URL.Query().Get("status"),
		Limit:      limit,
		Offset:     offset,
	}
	page, err := s.History.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	writeJSON(w, http.StatusOK, historyPage{
		Entries: page.Entries,
		Total:   page.Total,
		Limit:   page.Limit,
		Offset:  page.Offset,
	})
}

// parseIntParam reads an integer query parameter, returning def if it is
// absent (an empty string is treated as absent, not an error, so a client
// can omit the parameter entirely without special-casing it).
func parseIntParam(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	return strconv.Atoi(raw)
}

func parseHistoryID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (s *Server) handleHistoryGet(w http.ResponseWriter, r *http.Request) {
	id, err := parseHistoryID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	entry, err := s.History.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleHistoryDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseHistoryID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.History.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleHistoryVerify resolves the provider for the entry's connection and
// runs internal/history's verification (Exists / ChecksumVerifier) against
// the live remote object.
func (s *Server) handleHistoryVerify(w http.ResponseWriter, r *http.Request) {
	id, err := parseHistoryID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	entry, err := s.History.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}

	conn, err := s.Config.Get(entry.ProviderID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "resolving connection for entry: %s", err)
		return
	}
	p, err := registry.Create(conn.ProviderType, conn.ProviderConfig, s.Secrets)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}

	ctx, cancel := ctxWithTimeout(r, 2*time.Minute)
	defer cancel()
	updated, err := s.History.VerifyEntry(ctx, entry, p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
