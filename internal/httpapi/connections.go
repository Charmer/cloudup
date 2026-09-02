package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"cloudup/internal/config"
	"cloudup/internal/registry"
)

// providerTypeView is one entry of GET /api/v1/provider-types. This used to
// be a bare array of type-name strings; it grew a per-type object once a
// client needed to know which types require an interactive authorization
// step before a connection of that type can work (googledrive, dropbox) -
// otherwise the frontend would have to hardcode that list, which is exactly
// the per-provider knowledge the core is built to avoid.
type providerTypeView struct {
	Type string `json:"type"`

	// RequiresOAuth means "creating a connection of this type is not
	// enough: POST /connections/{id}/oauth/authorize has to be run for it
	// too" (and the app-wide client credentials configured first, see
	// /provider-types/{type}/oauth-credentials).
	RequiresOAuth bool `json:"requiresOAuth"`
}

func (s *Server) handleProviderTypes(w http.ResponseWriter, r *http.Request) {
	types := registry.Types()
	views := make([]providerTypeView, len(types))
	for i, t := range types {
		views[i] = providerTypeView{Type: t, RequiresOAuth: registry.RequiresOAuth(t)}
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleProviderSchema(w http.ResponseWriter, r *http.Request) {
	fields, err := registry.Schema(r.PathValue("type"))
	if err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}
	writeJSON(w, http.StatusOK, fields)
}

// connectionView is what connection endpoints return: the non-secret
// config fields flattened out of config.Connection.ProviderConfig (minus
// the internal connectionId field every provider factory reads back), so
// clients never have to parse an opaque JSON blob themselves.
type connectionView struct {
	ID           string            `json:"id"`
	ProviderType string            `json:"providerType"`
	DisplayName  string            `json:"displayName"`
	Fields       map[string]string `json:"fields"`
	CreatedAt    time.Time         `json:"createdAt"`
}

func toConnectionView(c config.Connection) connectionView {
	fields := map[string]string{}
	_ = json.Unmarshal(c.ProviderConfig, &fields)
	delete(fields, "connectionId")
	return connectionView{
		ID:           c.ID,
		ProviderType: c.ProviderType,
		DisplayName:  c.DisplayName,
		Fields:       fields,
		CreatedAt:    c.CreatedAt,
	}
}

func (s *Server) handleConnectionsList(w http.ResponseWriter, r *http.Request) {
	conns, err := s.Config.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}
	views := make([]connectionView, len(conns))
	for i, c := range conns {
		views[i] = toConnectionView(c)
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleConnectionsGet(w http.ResponseWriter, r *http.Request) {
	conn, err := s.Config.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}
	writeJSON(w, http.StatusOK, toConnectionView(conn))
}

// connectionRequest is the body of POST/PUT /api/v1/connections[/{id}].
// Fields holds every non-secret ConfigSchema value (FieldText/FieldSelect);
// Secrets holds every FieldPassword value, routed straight to the OS
// keychain via internal/secrets and never persisted in config.json.
type connectionRequest struct {
	ProviderType string            `json:"providerType"`
	DisplayName  string            `json:"displayName"`
	Fields       map[string]string `json:"fields"`
	Secrets      map[string]string `json:"secrets"`
}

func (s *Server) handleConnectionsCreate(w http.ResponseWriter, r *http.Request) {
	var req connectionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: %s", err)
		return
	}
	if req.ProviderType == "" || req.DisplayName == "" {
		writeError(w, http.StatusBadRequest, "providerType and displayName are required")
		return
	}

	conn, err := s.Config.Add(req.ProviderType, req.DisplayName, req.Fields)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	for key, value := range req.Secrets {
		if err := s.Secrets.Set(conn.ID, key, value); err != nil {
			writeError(w, http.StatusInternalServerError, "storing secret %q: %s", key, err)
			return
		}
	}

	writeJSON(w, http.StatusCreated, toConnectionView(conn))
}

func (s *Server) handleConnectionsUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req connectionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: %s", err)
		return
	}

	conn, err := s.Config.Update(id, req.DisplayName, req.Fields)
	if err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}
	for key, value := range req.Secrets {
		if err := s.Secrets.Set(id, key, value); err != nil {
			writeError(w, http.StatusInternalServerError, "storing secret %q: %s", key, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, toConnectionView(conn))
}

func (s *Server) handleConnectionsDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	conn, err := s.Config.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}

	// Refusing to delete a connection with active uploads, rather than
	// silently letting them run to completion against a now-deleted
	// config, also keeps queue.Manager's per-connection dispatcher
	// goroutine (and its providerQueue entry) from staying alive with
	// nothing left in internal/config to ever reference it again - the
	// client is expected to cancel first (POST .../cancel-all) if it
	// really wants the connection gone regardless.
	if s.Queue.HasActiveTasks(id) {
		writeError(w, http.StatusConflict, "connection %q has active or pending uploads - cancel them first (POST /api/v1/connections/%s/cancel-all)", id, id)
		return
	}

	// Same reasoning as the active-uploads check above: an enabled watch
	// rule referencing this connection would be left silently pointing at
	// nothing (see watches.go's handleWatchesCreate for the matching check
	// on the other side - a rule can't be created against an unknown
	// connection either). The client disables or deletes the rule first.
	if watchRuleID, ok := s.enabledWatchRuleFor(id); ok {
		writeError(w, http.StatusConflict, "connection %q is used by watch rule %q - disable or delete it first", id, watchRuleID)
		return
	}

	if err := s.Config.Remove(id); err != nil {
		writeError(w, http.StatusInternalServerError, "%s", err)
		return
	}

	// Best-effort secret cleanup - deleting an already-absent secret is a
	// no-op, so this never fails a delete that otherwise succeeded.
	_ = s.Secrets.DeleteConnection(id, registry.ConnectionSecretKeys(conn.ProviderType))

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConnectionsTest(w http.ResponseWriter, r *http.Request) {
	conn, err := s.Config.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}
	p, err := registry.Create(conn.ProviderType, conn.ProviderConfig, s.Secrets)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}

	ctx, cancel := ctxWithTimeout(r, 30*time.Second)
	defer cancel()
	if err := p.TestConnection(ctx); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
