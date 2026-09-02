package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloudup/internal/history"
)

// recordEntry inserts one history row directly through the store (uploads
// are exercised separately in uploads_test.go; here the rows are fixtures).
func (e *testEnv) recordEntry(entry history.Entry) history.Entry {
	e.t.Helper()
	id, err := e.History.Record(context.Background(), entry)
	if err != nil {
		e.t.Fatalf("history.Record() error = %v", err)
	}
	stored, err := e.History.Get(context.Background(), id)
	if err != nil {
		e.t.Fatalf("history.Get() error = %v", err)
	}
	return stored
}

// TestHistoryListAppliesFilters covers both the unfiltered listing and the
// two query parameters openapi.yaml documents (connectionId, status) - a
// filter silently ignored would look like a working endpoint returning the
// wrong rows.
func TestHistoryListAppliesFilters(t *testing.T) {
	env := newTestEnv(t)

	env.recordEntry(history.Entry{LocalPath: "/a", ProviderID: "conn-a", ProviderType: fakeType, RemotePath: "/a", Status: history.StatusSuccess})
	env.recordEntry(history.Entry{LocalPath: "/b", ProviderID: "conn-b", ProviderType: fakeType, RemotePath: "/b", Status: history.StatusSuccess})
	env.recordEntry(history.Entry{LocalPath: "/c", ProviderID: "conn-a", ProviderType: fakeType, RemotePath: "/c", Status: history.StatusError})

	all := decodeBody[historyPage](t, env.do(http.MethodGet, "/api/v1/history", nil), http.StatusOK)
	if len(all.Entries) != 3 || all.Total != 3 {
		t.Fatalf("unfiltered history = %d entries (total %d), want 3 (total 3)", len(all.Entries), all.Total)
	}

	byConn := decodeBody[historyPage](t, env.do(http.MethodGet, "/api/v1/history?connectionId=conn-a", nil), http.StatusOK)
	if len(byConn.Entries) != 2 || byConn.Total != 2 {
		t.Errorf("history?connectionId=conn-a = %d entries (total %d), want 2 (total 2)", len(byConn.Entries), byConn.Total)
	}
	for _, e := range byConn.Entries {
		if e.ProviderID != "conn-a" {
			t.Errorf("connectionId filter returned an entry for %q", e.ProviderID)
		}
	}

	byStatus := decodeBody[historyPage](t, env.do(http.MethodGet, "/api/v1/history?status=error", nil), http.StatusOK)
	if len(byStatus.Entries) != 1 || byStatus.Entries[0].Status != history.StatusError {
		t.Errorf("history?status=error = %+v, want the single error entry", byStatus)
	}

	both := decodeBody[historyPage](t, env.do(http.MethodGet, "/api/v1/history?connectionId=conn-b&status=error", nil), http.StatusOK)
	if len(both.Entries) != 0 || both.Total != 0 {
		t.Errorf("combined filters = %+v, want no entries", both)
	}
}

// TestHistoryListPaginates covers the limit/offset query params and the
// response envelope's Total/Limit/Offset fields - GET /api/v1/history
// bounds its page size by default (see history.DefaultHistoryPageSize) so
// a long-running install with a large upload_log never returns it whole in
// one response.
func TestHistoryListPaginates(t *testing.T) {
	env := newTestEnv(t)

	for i := range 5 {
		env.recordEntry(history.Entry{LocalPath: itoa(int64(i)), ProviderID: "conn-a", ProviderType: fakeType, RemotePath: "/x", Status: history.StatusSuccess})
	}

	page1 := decodeBody[historyPage](t, env.do(http.MethodGet, "/api/v1/history?limit=2&offset=0", nil), http.StatusOK)
	if len(page1.Entries) != 2 || page1.Total != 5 || page1.Limit != 2 || page1.Offset != 0 {
		t.Fatalf("page1 = %+v, want 2 entries, total 5, limit 2, offset 0", page1)
	}

	page2 := decodeBody[historyPage](t, env.do(http.MethodGet, "/api/v1/history?limit=2&offset=2", nil), http.StatusOK)
	if len(page2.Entries) != 2 || page2.Total != 5 || page2.Offset != 2 {
		t.Fatalf("page2 = %+v, want 2 entries, total 5, offset 2", page2)
	}
	if page1.Entries[0].LocalPath == page2.Entries[0].LocalPath {
		t.Fatalf("page1 and page2 overlap: both start with %q", page1.Entries[0].LocalPath)
	}

	page3 := decodeBody[historyPage](t, env.do(http.MethodGet, "/api/v1/history?limit=2&offset=4", nil), http.StatusOK)
	if len(page3.Entries) != 1 || page3.Total != 5 {
		t.Fatalf("page3 (last, partial) = %+v, want 1 entry, total 5", page3)
	}

	oversized := decodeBody[historyPage](t, env.do(http.MethodGet, "/api/v1/history?limit=100000", nil), http.StatusOK)
	if oversized.Limit != history.MaxHistoryPageSize {
		t.Errorf("limit=100000 got clamped Limit = %d, want %d", oversized.Limit, history.MaxHistoryPageSize)
	}

	unspecified := decodeBody[historyPage](t, env.do(http.MethodGet, "/api/v1/history", nil), http.StatusOK)
	if unspecified.Limit != history.DefaultHistoryPageSize {
		t.Errorf("no limit param got Limit = %d, want default %d", unspecified.Limit, history.DefaultHistoryPageSize)
	}

	errorMessage(t, env.do(http.MethodGet, "/api/v1/history?limit=not-a-number", nil), http.StatusBadRequest)
	errorMessage(t, env.do(http.MethodGet, "/api/v1/history?offset=not-a-number", nil), http.StatusBadRequest)
}

// TestHistoryGetAndDelete - the per-entry routes, including that a
// non-numeric ID is a 400 (a client bug) while a well-formed but absent one
// is a 404.
func TestHistoryGetAndDelete(t *testing.T) {
	env := newTestEnv(t)

	stored := env.recordEntry(history.Entry{
		LocalPath: "/local/f.bin", LocalSize: 42, ProviderID: "conn-a", ProviderType: fakeType,
		RemotePath: "/remote/f.bin", Checksum: "abc", ChecksumAlgo: "sha256", Status: history.StatusSuccess,
	})

	got := decodeBody[history.Entry](t, env.do(http.MethodGet, "/api/v1/history/"+itoa(stored.ID), nil), http.StatusOK)
	if got.ID != stored.ID || got.RemotePath != "/remote/f.bin" || got.LocalSize != 42 {
		t.Errorf("get = %+v, want the recorded entry %+v", got, stored)
	}

	errorMessage(t, env.do(http.MethodGet, "/api/v1/history/not-a-number", nil), http.StatusBadRequest)
	errorMessage(t, env.do(http.MethodGet, "/api/v1/history/999999", nil), http.StatusNotFound)
	errorMessage(t, env.do(http.MethodDelete, "/api/v1/history/not-a-number", nil), http.StatusBadRequest)

	if rec := env.do(http.MethodDelete, "/api/v1/history/"+itoa(stored.ID), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec := env.do(http.MethodGet, "/api/v1/history/"+itoa(stored.ID), nil); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete status = %d, want 404", rec.Code)
	}
}

// TestHistoryVerifyReportsCheckStatus - the happy path: verification runs
// the provider's Exists/VerifyChecksum and writes the outcome back onto the
// entry, which is what the response carries.
func TestHistoryVerifyReportsCheckStatus(t *testing.T) {
	env := newTestEnv(t)

	conn := env.createConnection(fakeType, "Verifiable", map[string]string{"url": "u"}, nil)
	stored := env.recordEntry(history.Entry{
		LocalPath: "/local/f", ProviderID: conn.ID, ProviderType: fakeType,
		RemotePath: "/remote/f", Checksum: "abc", ChecksumAlgo: "sha256", Status: history.StatusSuccess,
	})

	got := decodeBody[history.Entry](t, env.do(http.MethodPost, "/api/v1/history/"+itoa(stored.ID)+"/verify", nil), http.StatusOK)
	if got.LastCheckStatus != history.CheckOK {
		t.Errorf("lastCheckStatus = %q, want %q", got.LastCheckStatus, history.CheckOK)
	}
	if got.LastCheckedAt == nil {
		t.Error("lastCheckedAt not set by verify")
	}

	// A remote object that is gone is reported as a check status, not as
	// an HTTP error.
	behaviorFor(t, conn.ID).exists = func(ctx context.Context, remotePath string) (bool, error) { return false, nil }
	got = decodeBody[history.Entry](t, env.do(http.MethodPost, "/api/v1/history/"+itoa(stored.ID)+"/verify", nil), http.StatusOK)
	if got.LastCheckStatus != history.CheckMissing {
		t.Errorf("lastCheckStatus = %q, want %q", got.LastCheckStatus, history.CheckMissing)
	}
}

// TestHistoryVerifyWithUnresolvableProviderReportsError: verify has to
// resolve a provider from the entry's connection, and both ways that can
// fail must produce a clear 400 rather than a nil-provider panic inside
// history.VerifyEntry. This is the "sane error, not a panic" guarantee.
func TestHistoryVerifyWithUnresolvableProviderReportsError(t *testing.T) {
	env := newTestEnv(t)

	// (a) the entry points at a connection that no longer exists.
	orphan := env.recordEntry(history.Entry{
		LocalPath: "/local/x", ProviderID: "deleted-connection", ProviderType: fakeType,
		RemotePath: "/remote/x", Status: history.StatusSuccess,
	})
	msg := errorMessage(t, env.do(http.MethodPost, "/api/v1/history/"+itoa(orphan.ID)+"/verify", nil), http.StatusBadRequest)
	if !strings.Contains(msg, "resolving connection") {
		t.Errorf("error = %q, want it to explain the connection could not be resolved", msg)
	}

	// (b) the connection exists but its provider cannot be constructed
	// (the real case: OAuth credentials revoked or never configured).
	broken := env.createConnection(fakeType, "Broken", map[string]string{"url": "u", "fail": "no credentials"}, nil)
	entry := env.recordEntry(history.Entry{
		LocalPath: "/local/y", ProviderID: broken.ID, ProviderType: fakeType,
		RemotePath: "/remote/y", Status: history.StatusSuccess,
	})
	msg = errorMessage(t, env.do(http.MethodPost, "/api/v1/history/"+itoa(entry.ID)+"/verify", nil), http.StatusBadRequest)
	if !strings.Contains(msg, "no credentials") {
		t.Errorf("error = %q, want the factory's failure message", msg)
	}

	// (c) a provider whose Exists call fails is a check_error on the
	// entry, not a 5xx - the API call itself succeeded.
	ok := env.createConnection(fakeType, "Flaky", map[string]string{"url": "u"}, nil)
	behaviorFor(t, ok.ID).exists = func(ctx context.Context, remotePath string) (bool, error) {
		return false, errors.New("remote unreachable")
	}
	flaky := env.recordEntry(history.Entry{
		LocalPath: "/local/z", ProviderID: ok.ID, ProviderType: fakeType,
		RemotePath: "/remote/z", Status: history.StatusSuccess,
	})
	got := decodeBody[history.Entry](t, env.do(http.MethodPost, "/api/v1/history/"+itoa(flaky.ID)+"/verify", nil), http.StatusOK)
	if got.LastCheckStatus != history.CheckError {
		t.Errorf("lastCheckStatus = %q, want %q", got.LastCheckStatus, history.CheckError)
	}

	errorMessage(t, env.do(http.MethodPost, "/api/v1/history/not-a-number/verify", nil), http.StatusBadRequest)
	errorMessage(t, env.do(http.MethodPost, "/api/v1/history/999999/verify", nil), http.StatusNotFound)
}
