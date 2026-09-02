package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWatchesCRUD covers create/list/update/delete through the REST API,
// the same shape as connections_test.go's own CRUD test.
func TestWatchesCRUD(t *testing.T) {
	env := newTestEnv(t)
	conn := env.createConnection(fakeType, "Watched", map[string]string{"url": "u"}, nil)
	dir := t.TempDir()

	created := decodeBody[watchRuleView](t, env.doJSON(http.MethodPost, "/api/v1/watches", watchRuleRequest{
		LocalPath:    dir,
		ConnectionID: conn.ID,
		RemoteFolder: "backup",
	}), http.StatusCreated)
	if created.ID == "" {
		t.Fatal("create response has no ID")
	}
	if !created.Enabled {
		t.Error("a freshly created rule should be enabled")
	}
	if created.Status != "watching" {
		t.Errorf("status = %q, want %q", created.Status, "watching")
	}

	list := decodeBody[[]watchRuleView](t, env.do(http.MethodGet, "/api/v1/watches", nil), http.StatusOK)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want the one rule just created", list)
	}

	updated := decodeBody[watchRuleView](t, env.doJSON(http.MethodPut, "/api/v1/watches/"+created.ID, watchRuleRequest{
		LocalPath:    dir,
		ConnectionID: conn.ID,
		RemoteFolder: "renamed",
		Enabled:      false,
	}), http.StatusOK)
	if updated.RemoteFolder != "renamed" {
		t.Errorf("RemoteFolder after update = %q, want %q", updated.RemoteFolder, "renamed")
	}
	if updated.Status != "disabled" {
		t.Errorf("status after disabling = %q, want %q", updated.Status, "disabled")
	}

	if rec := env.do(http.MethodDelete, "/api/v1/watches/"+created.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	list = decodeBody[[]watchRuleView](t, env.do(http.MethodGet, "/api/v1/watches", nil), http.StatusOK)
	if len(list) != 0 {
		t.Errorf("list after delete = %+v, want empty", list)
	}
}

func TestWatchesCreateRejectsUnknownConnection(t *testing.T) {
	env := newTestEnv(t)
	errorMessage(t, env.doJSON(http.MethodPost, "/api/v1/watches", watchRuleRequest{
		LocalPath:    t.TempDir(),
		ConnectionID: "ghost",
	}), http.StatusBadRequest)
}

func TestWatchesCreateRejectsMissingPath(t *testing.T) {
	env := newTestEnv(t)
	conn := env.createConnection(fakeType, "Watched", map[string]string{"url": "u"}, nil)

	errorMessage(t, env.doJSON(http.MethodPost, "/api/v1/watches", watchRuleRequest{
		LocalPath:    filepath.Join(t.TempDir(), "does-not-exist"),
		ConnectionID: conn.ID,
	}), http.StatusBadRequest)

	// A rejected create must not leave a persisted rule behind - see
	// handleWatchesCreate's doc comment on rolling back.
	list := decodeBody[[]watchRuleView](t, env.do(http.MethodGet, "/api/v1/watches", nil), http.StatusOK)
	if len(list) != 0 {
		t.Errorf("list after a rejected create = %+v, want empty", list)
	}
}

// TestWatchesCreateUploadsExistingFileImmediately is the end-to-end path:
// a file already sitting in the folder when the rule is created shows up
// in GET /tasks without needing any filesystem event at all (AddNew's
// immediate scan) - see engine.go's start/scanExisting.
func TestWatchesCreateUploadsExistingFileImmediately(t *testing.T) {
	env := newTestEnv(t)
	conn := env.createConnection(fakeType, "Watched", map[string]string{"url": "u"}, nil)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "already-here.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	decodeBody[watchRuleView](t, env.doJSON(http.MethodPost, "/api/v1/watches", watchRuleRequest{
		LocalPath:    dir,
		ConnectionID: conn.ID,
		RemoteFolder: "backup",
	}), http.StatusCreated)

	waitFor(t, 3*time.Second, "the pre-existing file to appear in GET /tasks", func() bool {
		for _, s := range decodeBody[[]taskSnapshot](t, env.do(http.MethodGet, "/api/v1/tasks", nil), http.StatusOK) {
			if s.RemotePath == "backup/already-here.txt" {
				return true
			}
		}
		return false
	})
}

func TestDeleteConnectionUsedByWatchRuleIsRejected(t *testing.T) {
	env := newTestEnv(t)
	conn := env.createConnection(fakeType, "Watched", map[string]string{"url": "u"}, nil)
	dir := t.TempDir()

	rule := decodeBody[watchRuleView](t, env.doJSON(http.MethodPost, "/api/v1/watches", watchRuleRequest{
		LocalPath:    dir,
		ConnectionID: conn.ID,
	}), http.StatusCreated)

	rec := env.do(http.MethodDelete, "/api/v1/connections/"+conn.ID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete connection status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}

	// Disabling the rule clears the way.
	env.doJSON(http.MethodPut, "/api/v1/watches/"+rule.ID, watchRuleRequest{
		LocalPath:    dir,
		ConnectionID: conn.ID,
		Enabled:      false,
	})
	if rec := env.do(http.MethodDelete, "/api/v1/connections/"+conn.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete connection status after disabling the rule = %d, want 204 (body: %s)", rec.Code, rec.Body.String())
	}
}
