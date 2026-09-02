package history

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cloudup/internal/provider"
)

// fakeProvider satisfies provider.Provider plus, optionally,
// provider.ExistenceChecker and provider.ChecksumVerifier, so tests can
// exercise every branch of VerifyEntry without a real backend.
type fakeProvider struct {
	existsFn func(ctx context.Context, remotePath string) (bool, error)
	verifyFn func(ctx context.Context, remotePath, algo, checksum string) (bool, error)
}

func (fakeProvider) Type() string                                                { return "fake" }
func (fakeProvider) DisplayName() string                                         { return "Fake" }
func (fakeProvider) TestConnection(ctx context.Context) error                    { return nil }
func (fakeProvider) Download(ctx context.Context, t provider.DownloadTask) error { return nil }
func (fakeProvider) List(ctx context.Context, remotePath string) ([]provider.RemoteEntry, error) {
	return nil, nil
}
func (fakeProvider) Delete(ctx context.Context, remotePath string) error { return nil }
func (fakeProvider) Upload(ctx context.Context, t provider.UploadTask) (provider.UploadResult, error) {
	return provider.UploadResult{}, nil
}

// existenceOnly implements Provider + ExistenceChecker but not ChecksumVerifier.
// Exists is defined only here (not on fakeProvider) so embedding fakeProvider
// elsewhere doesn't accidentally satisfy ExistenceChecker too.
type existenceOnly struct{ fakeProvider }

func (p existenceOnly) Exists(ctx context.Context, remotePath string) (bool, error) {
	return p.existsFn(ctx, remotePath)
}

// checksumOnly implements Provider + ChecksumVerifier but not ExistenceChecker.
type checksumOnly struct{ fakeProvider }

func (p checksumOnly) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	return p.verifyFn(ctx, remotePath, algo, checksum)
}

// full implements Provider + both optional interfaces.
type full struct{ fakeProvider }

func (p full) Exists(ctx context.Context, remotePath string) (bool, error) {
	return p.existsFn(ctx, remotePath)
}

func (p full) VerifyChecksum(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
	return p.verifyFn(ctx, remotePath, algo, checksum)
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecordAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, err := s.Record(ctx, Entry{
		LocalPath:    "/tmp/file.txt",
		LocalSize:    123,
		ProviderID:   "conn1",
		ProviderType: "webdav",
		RemotePath:   "/file.txt",
		RemoteURL:    "https://example.com/file.txt",
		Checksum:     "abc123",
		ChecksumAlgo: "sha256-self-computed",
		Status:       StatusSuccess,
	})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	e, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if e.LocalPath != "/tmp/file.txt" || e.RemotePath != "/file.txt" || e.Checksum != "abc123" {
		t.Fatalf("Get() = %+v, unexpected values", e)
	}
	if e.Status != StatusSuccess {
		t.Fatalf("Status = %q, want %q", e.Status, StatusSuccess)
	}
	if e.LastCheckedAt != nil {
		t.Fatalf("LastCheckedAt = %v, want nil for a never-verified entry", e.LastCheckedAt)
	}
	if e.UploadedAt.IsZero() {
		t.Fatal("UploadedAt is zero, want a timestamp defaulted by Record()")
	}
}

func TestDeleteRemovesEntry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id, err := s.Record(ctx, Entry{LocalPath: "/tmp/a.txt", ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/a.txt", Status: StatusSuccess})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := s.Get(ctx, id); err == nil {
		t.Fatal("Get() after Delete(): expected error, got nil")
	}
}

func TestDeleteUnknownIDIsNotAnError(t *testing.T) {
	s := openTestStore(t)
	if err := s.Delete(context.Background(), 9999); err != nil {
		t.Fatalf("Delete() of a never-existing id: error = %v, want nil", err)
	}
}

func TestGetUnknownIDReturnsError(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Get(context.Background(), 9999); err == nil {
		t.Fatal("Get() with unknown id: expected error, got nil")
	}
}

func TestListFiltersByProviderAndStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mustRecord(t, s, Entry{LocalPath: "a", ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/a", Status: StatusSuccess})
	mustRecord(t, s, Entry{LocalPath: "b", ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/b", Status: StatusError})
	mustRecord(t, s, Entry{LocalPath: "c", ProviderID: "conn2", ProviderType: "s3", RemotePath: "/c", Status: StatusSuccess})

	byProvider, err := s.List(ctx, Filter{ProviderID: "conn1"})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(byProvider.Entries) != 2 || byProvider.Total != 2 {
		t.Fatalf("List(ProviderID=conn1) = %d entries (total %d), want 2 (total 2)", len(byProvider.Entries), byProvider.Total)
	}

	byStatus, err := s.List(ctx, Filter{Status: StatusSuccess})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(byStatus.Entries) != 2 || byStatus.Total != 2 {
		t.Fatalf("List(Status=success) = %d entries (total %d), want 2 (total 2)", len(byStatus.Entries), byStatus.Total)
	}

	combined, err := s.List(ctx, Filter{ProviderID: "conn1", Status: StatusError})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(combined.Entries) != 1 || combined.Entries[0].LocalPath != "b" || combined.Total != 1 {
		t.Fatalf("List(conn1, error) = %+v, want single entry b (total 1)", combined)
	}
}

// TestListPaginates locks in Filter.Limit/Offset and Page.Total: List must
// bound how many rows it returns per call (see Filter.Limit's doc comment
// on why an unbounded page defeats the point) while still reporting how
// many entries exist in total across every page.
func TestListPaginates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i := range 5 {
		mustRecord(t, s, Entry{LocalPath: fmt.Sprintf("file-%d", i), ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/x", Status: StatusSuccess})
	}

	page1, err := s.List(ctx, Filter{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page1.Entries) != 2 || page1.Total != 5 {
		t.Fatalf("page1 = %d entries (total %d), want 2 (total 5)", len(page1.Entries), page1.Total)
	}

	page2, err := s.List(ctx, Filter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page2.Entries) != 2 || page2.Total != 5 {
		t.Fatalf("page2 = %d entries (total %d), want 2 (total 5)", len(page2.Entries), page2.Total)
	}
	if page1.Entries[0].LocalPath == page2.Entries[0].LocalPath {
		t.Fatalf("page1 and page2 overlap: both start with %q", page1.Entries[0].LocalPath)
	}

	page3, err := s.List(ctx, Filter{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page3.Entries) != 1 || page3.Total != 5 {
		t.Fatalf("page3 (last, partial) = %d entries (total %d), want 1 (total 5)", len(page3.Entries), page3.Total)
	}

	unbounded, err := s.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(unbounded.Entries) != 5 {
		t.Fatalf("List() with no Limit = %d entries, want all 5 (well under DefaultHistoryPageSize)", len(unbounded.Entries))
	}
}

func mustRecord(t *testing.T, s *Store, e Entry) int64 {
	t.Helper()
	id, err := s.Record(context.Background(), e)
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	return id
}

func TestVerifyEntryMissing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := mustRecord(t, s, Entry{LocalPath: "a", ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/a", Status: StatusSuccess})
	e, _ := s.Get(ctx, id)

	p := existenceOnly{fakeProvider{existsFn: func(ctx context.Context, remotePath string) (bool, error) {
		return false, nil
	}}}

	updated, err := s.VerifyEntry(ctx, e, p)
	if err != nil {
		t.Fatalf("VerifyEntry() error = %v", err)
	}
	if updated.LastCheckStatus != CheckMissing {
		t.Fatalf("LastCheckStatus = %q, want %q", updated.LastCheckStatus, CheckMissing)
	}
	if updated.LastCheckedAt == nil {
		t.Fatal("LastCheckedAt is nil, want a timestamp after VerifyEntry()")
	}
}

func TestVerifyEntryChecksumMatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := mustRecord(t, s, Entry{
		LocalPath: "a", ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/a",
		Checksum: "abc", ChecksumAlgo: "sha256-self-computed", Status: StatusSuccess,
	})
	e, _ := s.Get(ctx, id)

	p := checksumOnly{fakeProvider{verifyFn: func(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
		return checksum == "abc", nil
	}}}

	updated, err := s.VerifyEntry(ctx, e, p)
	if err != nil {
		t.Fatalf("VerifyEntry() error = %v", err)
	}
	if updated.LastCheckStatus != CheckOK {
		t.Fatalf("LastCheckStatus = %q, want %q", updated.LastCheckStatus, CheckOK)
	}
}

func TestVerifyEntryChecksumMismatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := mustRecord(t, s, Entry{
		LocalPath: "a", ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/a",
		Checksum: "abc", ChecksumAlgo: "sha256-self-computed", Status: StatusSuccess,
	})
	e, _ := s.Get(ctx, id)

	p := checksumOnly{fakeProvider{verifyFn: func(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
		return false, nil
	}}}

	updated, err := s.VerifyEntry(ctx, e, p)
	if err != nil {
		t.Fatalf("VerifyEntry() error = %v", err)
	}
	if updated.LastCheckStatus != CheckMismatch {
		t.Fatalf("LastCheckStatus = %q, want %q", updated.LastCheckStatus, CheckMismatch)
	}
}

func TestVerifyEntryUnverifiableWhenNoOptionalInterfaces(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := mustRecord(t, s, Entry{LocalPath: "a", ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/a", Status: StatusSuccess})
	e, _ := s.Get(ctx, id)

	updated, err := s.VerifyEntry(ctx, e, fakeProvider{})
	if err != nil {
		t.Fatalf("VerifyEntry() error = %v", err)
	}
	if updated.LastCheckStatus != CheckUnverifiable {
		t.Fatalf("LastCheckStatus = %q, want %q", updated.LastCheckStatus, CheckUnverifiable)
	}
}

func TestVerifyEntryExistenceErrorReportsCheckError(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := mustRecord(t, s, Entry{LocalPath: "a", ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/a", Status: StatusSuccess})
	e, _ := s.Get(ctx, id)

	p := existenceOnly{fakeProvider{existsFn: func(ctx context.Context, remotePath string) (bool, error) {
		return false, errors.New("network down")
	}}}

	updated, err := s.VerifyEntry(ctx, e, p)
	if err != nil {
		t.Fatalf("VerifyEntry() error = %v", err)
	}
	if updated.LastCheckStatus != CheckError {
		t.Fatalf("LastCheckStatus = %q, want %q", updated.LastCheckStatus, CheckError)
	}
}

func TestVerifyEntryExistsThenChecksum(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := mustRecord(t, s, Entry{
		LocalPath: "a", ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/a",
		Checksum: "abc", ChecksumAlgo: "sha256-self-computed", Status: StatusSuccess,
	})
	e, _ := s.Get(ctx, id)

	p := full{fakeProvider{
		existsFn: func(ctx context.Context, remotePath string) (bool, error) { return true, nil },
		verifyFn: func(ctx context.Context, remotePath, algo, checksum string) (bool, error) {
			return checksum == "abc", nil
		},
	}}

	updated, err := s.VerifyEntry(ctx, e, p)
	if err != nil {
		t.Fatalf("VerifyEntry() error = %v", err)
	}
	if updated.LastCheckStatus != CheckOK {
		t.Fatalf("LastCheckStatus = %q, want %q", updated.LastCheckStatus, CheckOK)
	}
}

func TestRecordDefaultsUploadedAt(t *testing.T) {
	s := openTestStore(t)
	before := time.Now().UTC().Add(-time.Second)

	id, err := s.Record(context.Background(), Entry{
		LocalPath: "a", ProviderID: "conn1", ProviderType: "webdav", RemotePath: "/a", Status: StatusSuccess,
	})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	e, _ := s.Get(context.Background(), id)
	if e.UploadedAt.Before(before) {
		t.Fatalf("UploadedAt = %v, want at/after %v", e.UploadedAt, before)
	}
}
