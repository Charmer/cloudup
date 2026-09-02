package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return s
}

func TestOpenInitializesDefaults(t *testing.T) {
	s := openTestStore(t)
	got, err := s.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Fatalf("Get() = %+v, want %+v", got, Default())
	}
}

func TestSetPersistsAcrossOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := s.Set(Settings{MaxConcurrentUploadsPerProvider: 4}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	got, err := reopened.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.MaxConcurrentUploadsPerProvider != 4 {
		t.Fatalf("MaxConcurrentUploadsPerProvider = %d, want 4", got.MaxConcurrentUploadsPerProvider)
	}
}

// TestSetDefaultsMultiThreadFieldsWhenUnset covers both a genuinely
// zero-valued request and a pre-existing settings.json written before these
// fields existed (which unmarshal the same way) - both must land on the
// real defaults, not on a silently-disabled 0/off.
func TestSetDefaultsMultiThreadFieldsWhenUnset(t *testing.T) {
	s := openTestStore(t)
	if err := s.Set(Settings{MaxConcurrentUploadsPerProvider: 1}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	got, err := s.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.MultiThreadStreams != DefaultMultiThreadStreams {
		t.Errorf("MultiThreadStreams = %d, want default %d", got.MultiThreadStreams, DefaultMultiThreadStreams)
	}
	if got.MultiThreadThresholdBytes != DefaultMultiThreadThresholdBytes {
		t.Errorf("MultiThreadThresholdBytes = %d, want default %d", got.MultiThreadThresholdBytes, DefaultMultiThreadThresholdBytes)
	}
	if got.IdleConnectionTimeoutMinutes != DefaultIdleConnectionTimeoutMinutes {
		t.Errorf("IdleConnectionTimeoutMinutes = %d, want default %d", got.IdleConnectionTimeoutMinutes, DefaultIdleConnectionTimeoutMinutes)
	}
}

// TestSetPreservesExplicitIdleConnectionTimeoutMinutes mirrors
// TestSetPreservesExplicitMultiThreadStreamsOfOne: a small-but-positive
// value is a real, storable choice (queue.Manager clamps it further, at 1
// minute, not this package - see the field's own doc comment), only <= 0
// falls back to the default.
func TestSetPreservesExplicitIdleConnectionTimeoutMinutes(t *testing.T) {
	s := openTestStore(t)
	if err := s.Set(Settings{MaxConcurrentUploadsPerProvider: 1, IdleConnectionTimeoutMinutes: 3}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	got, err := s.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.IdleConnectionTimeoutMinutes != 3 {
		t.Errorf("IdleConnectionTimeoutMinutes = %d, want 3 (explicit value preserved)", got.IdleConnectionTimeoutMinutes)
	}
}

// TestSetPreservesExplicitMultiThreadStreamsOfOne pins that 1 is a real,
// storable value (it disables the parallel path in queue.Manager's
// dispatch, but is not treated as "unset" the way 0 is - see the field's
// own doc comment).
func TestSetPreservesExplicitMultiThreadStreamsOfOne(t *testing.T) {
	s := openTestStore(t)
	if err := s.Set(Settings{MaxConcurrentUploadsPerProvider: 1, MultiThreadStreams: 1, MultiThreadThresholdBytes: 1}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	got, err := s.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.MultiThreadStreams != 1 {
		t.Errorf("MultiThreadStreams = %d, want 1 (explicit value preserved)", got.MultiThreadStreams)
	}
	if got.MultiThreadThresholdBytes != 1 {
		t.Errorf("MultiThreadThresholdBytes = %d, want 1 (explicit value preserved)", got.MultiThreadThresholdBytes)
	}
}

// TestSetPreservesZeroMaxUploadBytesPerSecond pins that 0 means "unlimited"
// and is a real, storable value here - unlike MultiThreadStreams/
// MultiThreadThresholdBytes above, it must never be coerced to some other
// default.
func TestSetPreservesZeroMaxUploadBytesPerSecond(t *testing.T) {
	s := openTestStore(t)
	if err := s.Set(Settings{MaxConcurrentUploadsPerProvider: 1, MaxUploadBytesPerSecond: 5000}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if err := s.Set(Settings{MaxConcurrentUploadsPerProvider: 1, MaxUploadBytesPerSecond: 0}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	got, err := s.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.MaxUploadBytesPerSecond != 0 {
		t.Errorf("MaxUploadBytesPerSecond = %d, want 0 (unlimited, not reset to some other default)", got.MaxUploadBytesPerSecond)
	}
}

func TestSetClampsNegativeMaxUploadBytesPerSecondToZero(t *testing.T) {
	s := openTestStore(t)
	if err := s.Set(Settings{MaxConcurrentUploadsPerProvider: 1, MaxUploadBytesPerSecond: -100}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	got, err := s.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.MaxUploadBytesPerSecond != 0 {
		t.Errorf("MaxUploadBytesPerSecond = %d, want 0 (negative clamped to unlimited)", got.MaxUploadBytesPerSecond)
	}
}

func TestSetClampsBelowOne(t *testing.T) {
	s := openTestStore(t)
	for _, n := range []int{0, -5} {
		if err := s.Set(Settings{MaxConcurrentUploadsPerProvider: n}); err != nil {
			t.Fatalf("Set(%d) error = %v", n, err)
		}
		got, err := s.Get()
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.MaxConcurrentUploadsPerProvider != 1 {
			t.Fatalf("after Set(%d): MaxConcurrentUploadsPerProvider = %d, want 1", n, got.MaxConcurrentUploadsPerProvider)
		}
	}
}

func TestVerifyChecksumAfterUploadRoundTripsPerType(t *testing.T) {
	s := openTestStore(t)
	want := map[string]bool{"yandexdisk": true, "webdav": false}
	if err := s.Set(Settings{MaxConcurrentUploadsPerProvider: 1, VerifyChecksumAfterUpload: want}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	got, err := s.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	// A false entry is meaningless (absent already means off) but must not
	// be silently dropped either - what was Set must be what comes back.
	if !reflect.DeepEqual(got.VerifyChecksumAfterUpload, want) {
		t.Fatalf("VerifyChecksumAfterUpload = %v, want %v", got.VerifyChecksumAfterUpload, want)
	}
}

// TestOpenToleratesLegacyVerifyChecksumAfterUploadBool covers a
// settings.json written before VerifyChecksumAfterUpload became
// per-provider-type: a bare JSON boolean where an object is now expected.
// Opening it must not fail (which would otherwise leave a pre-existing
// install unable to start after upgrading) and must not guess which
// provider types a legacy `true` meant - see
// parseSettingsTolerantly's doc comment for why landing on "off for
// everything" is the correct, unsurprising choice. Deliberately exercised
// through Store.Open/Get (disk state), not a bare json.Unmarshal - the
// tolerance is scoped to reading old files, not to the PUT
// /api/v1/settings request body, which still rejects this shape normally
// (see internal/httpapi's own settings tests).
func TestOpenToleratesLegacyVerifyChecksumAfterUploadBool(t *testing.T) {
	for _, raw := range []string{
		`{"maxConcurrentUploadsPerProvider": 2, "verifyChecksumAfterUpload": true}`,
		`{"maxConcurrentUploadsPerProvider": 2, "verifyChecksumAfterUpload": false}`,
	} {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open(%s): error = %v, want the legacy bool to be tolerated", raw, err)
		}
		got, err := s.Get()
		if err != nil {
			t.Fatalf("Get() after Open(%s): error = %v", raw, err)
		}
		if len(got.VerifyChecksumAfterUpload) != 0 {
			t.Errorf("Open(%s): VerifyChecksumAfterUpload = %v, want empty (legacy value discarded, not guessed)", raw, got.VerifyChecksumAfterUpload)
		}
		// The rest of the file must still load normally - migrating this
		// one field must not come at the cost of every other setting.
		if got.MaxConcurrentUploadsPerProvider != 2 {
			t.Errorf("Open(%s): MaxConcurrentUploadsPerProvider = %d, want 2 (unaffected by the migration)", raw, got.MaxConcurrentUploadsPerProvider)
		}
	}
}

// TestOpenStillRejectsRealCorruption makes sure
// parseSettingsTolerantly's fallback doesn't paper over a genuinely
// corrupt settings.json - only the one known legacy shape is tolerated.
func TestOpenStillRejectsRealCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	s, err := Open(path)
	if err != nil {
		// Open() itself only fails on a stat error, not a parse error -
		// parsing happens lazily in Get().
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := s.Get(); err == nil {
		t.Fatal("Get() on a genuinely corrupt settings.json: expected an error, got nil")
	}
}
