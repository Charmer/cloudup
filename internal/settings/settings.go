// Package settings stores application-wide preferences as a small local
// JSON file, separate from internal/config's per-
// connection data - these are app-level defaults, not tied to any one
// storage connection.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"cloudup/internal/appdir"
)

// Settings holds every user-configurable app-wide preference.
//
// There is deliberately no Proxy field either: every provider's http.Client
// already honors HTTP_PROXY/HTTPS_PROXY/NO_PROXY via Go's standard
// http.ProxyFromEnvironment (set explicitly by b2's custom http.Transport,
// or inherited for free by everything else through http.DefaultTransport,
// which debuglog.Transport falls back to when its RT field is nil - see
// internal/debuglog). Adding an app-level override would just be a second,
// competing source of truth for the same thing the OS environment already
// controls.
//
// There is deliberately no Theme field: light/dark appearance never needed
// the backend at all - nothing on the Go side reads it, and a frontend can
// keep its own choice in localStorage without a round trip. It used to be
// stored and served here purely because Settings looked like the natural
// home for anything user-configurable.
type Settings struct {
	// MaxConcurrentUploadsPerProvider caps how many files may upload at
	// once within a single connection's queue (internal/queue.Manager
	// still processes them in FIFO order - this only raises how many of
	// the oldest pending ones may run simultaneously). Default is 1
	// (strictly sequential per provider).
	MaxConcurrentUploadsPerProvider int `json:"maxConcurrentUploadsPerProvider"`

	// VerifyChecksumAfterUpload automatically re-checks an object's
	// checksum right after a successful upload (verification is normally
	// a manual action; this opts it into automatically) - keyed by
	// provider TYPE (e.g. "webdav", "yandexdisk"), not by connection, so
	// the same choice applies to every connection of that type. Per-type
	// rather than one global switch because the cost varies wildly and
	// isn't visible anywhere else: googledrive/b2/yandexdisk/onedrive
	// verify via a cheap metadata read (native hash), while
	// webdav/s3/dropbox have no reliable native hash and must re-download
	// the whole object to verify it - see
	// internal/queue.Manager.finish's doc comment. A type missing from
	// the map (including every type, on a fresh install) defaults to off,
	// the same as the old single bool's default. There's no server-side
	// registry of which types are "cheap" - see CONTRIBUTING.md's checksum
	// section for the authoritative list; the frontend hint is a static,
	// hand-maintained list for the same reason ConnectionsView's OAuth
	// console links are (see SettingsView.vue).
	VerifyChecksumAfterUpload map[string]bool `json:"verifyChecksumAfterUpload"`

	// Language is a UI language code (e.g. "en", "ru"). Purely informational
	// from the backend's point of view - internal/settings doesn't validate
	// it against any known set, since localization is entirely the
	// frontend's concern (see frontend/), not something the Go side
	// interprets or falls back from.
	Language string `json:"language"`

	// MultiThreadStreams, when > 1, uploads a large file's chunks
	// concurrently across this many streams instead of one at a time - like
	// rclone's --multi-thread-streams. Unlike the ordinary chunked-upload
	// internals (queue.DefaultMultipartThreshold/PartSize, deliberately not
	// exposed here), this is a genuine user trade-off - more streams can
	// raise throughput on a fast, high-latency link at the cost of more
	// simultaneous connections - so it is. Only takes effect for providers
	// whose chunked-upload protocol supports concurrent parts (S3, B2 - see
	// provider.ParallelMultipartUploader; Dropbox's upload_session protocol
	// cannot support this at all) and only for files at/above
	// MultiThreadThresholdBytes. 1 disables it, falling back to the
	// ordinary sequential chunked path. Default: 4 (queue.
	// DefaultMultiThreadStreams, rclone's own default).
	MultiThreadStreams int `json:"multiThreadStreams"`

	// MultiThreadThresholdBytes is the minimum file size before
	// MultiThreadStreams kicks in. Default: 256 MiB (queue.
	// DefaultMultiThreadThreshold, rclone's own default) - below this, the
	// ordinary chunked-upload threshold/part size still applies as before.
	MultiThreadThresholdBytes int64 `json:"multiThreadThresholdBytes"`

	// MaxUploadBytesPerSecond caps the combined upload byte rate across
	// every provider connection at once - a single global budget (like
	// rclone's --bwlimit), not a per-provider limit, so N providers
	// uploading in parallel still share it. 0 (the default) means
	// unlimited. Unlike MultiThreadStreams/MultiThreadThresholdBytes above,
	// 0 is not "unset, fall back to a default" here - it is the meaningful,
	// intentional value "no cap" (see normalize, which only clamps negative
	// values, never coerces 0 to something else).
	MaxUploadBytesPerSecond int64 `json:"maxUploadBytesPerSecond"`

	// IdleConnectionTimeoutMinutes controls how long internal/queue.Manager
	// keeps a connection's dispatcher goroutine parked after it runs out of
	// work before tearing it down (see queue.Manager.
	// SetIdleQueueSweepInterval / DefaultIdleQueueSweepInterval for the
	// full mechanism). It is not an exact deadline: eviction happens after
	// being idle across two consecutive checks spaced this many minutes
	// apart, so real idle time before cleanup is somewhere between this
	// value and twice it - a deliberate design choice so a brief pause
	// between files in the same upload (the client submits one HTTP
	// request per file, not one request per batch) is never mistaken for
	// "this connection is done".
	// Default: 10 (minutes, queue.DefaultIdleQueueSweepInterval). Values
	// below 1 are raised to 1 - see queue.Manager's own floor.
	IdleConnectionTimeoutMinutes int `json:"idleConnectionTimeoutMinutes"`
}

// DefaultLanguage is the language code a fresh install starts with.
// Localization itself lives entirely in the frontend - the Go side only
// stores and serves this value (see Settings.Language).
const DefaultLanguage = "en"

// DefaultMultiThreadStreams and DefaultMultiThreadThresholdBytes are the
// out-of-the-box values for the two fields above - the same numbers
// internal/queue falls back to on its own (queue.DefaultMultiThreadStreams/
// DefaultMultiThreadThreshold) when nothing ever calls SetMultiThreadStreams,
// and the same ones rclone's own --multi-thread-streams uses. Duplicated
// here rather than imported, the same way MaxConcurrentUploadsPerProvider's
// default (1) below is a literal rather than a reference to any
// internal/queue constant - this package stays a leaf with no dependency on
// internal/queue, matching internal/config's own independence.
const (
	DefaultMultiThreadStreams        = 4
	DefaultMultiThreadThresholdBytes = 256 << 20 // 256 MiB
)

// DefaultIdleConnectionTimeoutMinutes is IdleConnectionTimeoutMinutes'
// out-of-the-box value - duplicated from, and kept in sync by hand with,
// queue.DefaultIdleQueueSweepInterval (10 minutes) for the same
// leaf-package-independence reason as the two constants above.
const DefaultIdleConnectionTimeoutMinutes = 10

// Default returns the out-of-the-box settings.
func Default() Settings {
	return Settings{
		MaxConcurrentUploadsPerProvider: 1,
		VerifyChecksumAfterUpload:       map[string]bool{},
		Language:                        DefaultLanguage,
		MultiThreadStreams:              DefaultMultiThreadStreams,
		MultiThreadThresholdBytes:       DefaultMultiThreadThresholdBytes,
		MaxUploadBytesPerSecond:         0, // unlimited
		IdleConnectionTimeoutMinutes:    DefaultIdleConnectionTimeoutMinutes,
	}
}

// normalize clamps values that would otherwise stall the queue (see
// queue.Manager.SetConcurrency) or are otherwise nonsensical.
func (s Settings) normalize() Settings {
	if s.MaxConcurrentUploadsPerProvider < 1 {
		s.MaxConcurrentUploadsPerProvider = 1
	}
	// A nil map (a fresh Settings{} or a settings.json predating this
	// field's per-type shape - see UnmarshalJSON) behaves identically to
	// an empty one for lookups, but MarshalIndent below would write
	// `null` instead of `{}`; keep the JSON shape a client can always
	// range over without a nil check.
	if s.VerifyChecksumAfterUpload == nil {
		s.VerifyChecksumAfterUpload = map[string]bool{}
	}
	if s.Language == "" {
		s.Language = DefaultLanguage
	}
	// <= 0 covers both a genuinely invalid value and a settings.json
	// written before these two fields existed (they unmarshal as the Go
	// zero value 0) - both should land on the same sane default rather
	// than silently disabling multi-threading for pre-existing installs.
	// A user who wants it off explicitly sets MultiThreadStreams to 1 (see
	// its own doc comment).
	if s.MultiThreadStreams <= 0 {
		s.MultiThreadStreams = DefaultMultiThreadStreams
	}
	if s.MultiThreadThresholdBytes <= 0 {
		s.MultiThreadThresholdBytes = DefaultMultiThreadThresholdBytes
	}
	// Only negative is nonsensical here - 0 is the meaningful "unlimited"
	// value (see MaxUploadBytesPerSecond's doc comment), unlike the two
	// fields above where <= 0 means "restore the default".
	if s.MaxUploadBytesPerSecond < 0 {
		s.MaxUploadBytesPerSecond = 0
	}
	// Same <= 0 "restore the default" treatment as MultiThreadStreams above
	// (and for the same reason: a pre-existing settings.json unmarshals a
	// field it never had as 0). queue.Manager.SetIdleQueueSweepInterval
	// separately floors this at 1 minute, so a small-but-positive value
	// here is left alone and clamped there instead.
	if s.IdleConnectionTimeoutMinutes <= 0 {
		s.IdleConnectionTimeoutMinutes = DefaultIdleConnectionTimeoutMinutes
	}
	return s
}

// Store is a JSON-file-backed settings store. Safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
}

// DefaultPath returns the standard location for the settings file: a
// "data" directory next to the running executable (see internal/appdir).
func DefaultPath() (string, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return "", fmt.Errorf("settings: %w", err)
	}
	return filepath.Join(dir, "settings.json"), nil
}

// Open loads (or, if absent, initializes with Default()) the settings file
// at path.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := s.save(Default()); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("settings: stat %s: %w", path, err)
	}
	return s, nil
}

// Get returns the current settings.
func (s *Store) Get() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Set persists v (after normalize()) as the current settings.
func (s *Store) Set(v Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(v.normalize())
}

func (s *Store) load() (Settings, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: reading %s: %w", s.path, err)
	}
	var v Settings
	if err := json.Unmarshal(data, &v); err != nil {
		if v, ok := parseSettingsTolerantly(data); ok {
			return v.normalize(), nil
		}
		return Settings{}, fmt.Errorf("settings: parsing %s: %w", s.path, err)
	}
	return v.normalize(), nil
}

// parseSettingsTolerantly is the one place a settings.json predating
// VerifyChecksumAfterUpload's per-provider-type shape (a bare JSON
// boolean where an object is now expected) gets tolerated, rather than
// leaving a pre-existing install unable to start at all: it strips
// whatever value that key holds and retries. This is deliberately scoped
// to on-disk state only, not the PUT /api/v1/settings request body (see
// internal/httpapi's decodeJSON, which still rejects an unrecognized
// shape as a normal 400) - a client hitting the current, documented API
// today has no excuse for sending the old shape, but a file written by an
// older version of this program on the user's own disk does. There is no
// way to know which provider types a legacy `true` was meant to cover, so
// it is discarded rather than guessed - the result (the field absent,
// which normalize() turns into an empty map) is exactly the old field's
// own default, so no pre-existing install's behavior silently changes in
// a way that isn't "off, same as before".
func parseSettingsTolerantly(data []byte) (Settings, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Settings{}, false
	}
	delete(raw, "verifyChecksumAfterUpload")
	patched, err := json.Marshal(raw)
	if err != nil {
		return Settings{}, false
	}
	var v Settings
	if err := json.Unmarshal(patched, &v); err != nil {
		return Settings{}, false
	}
	return v, true
}

// save writes v atomically: write to a temp file in the same directory,
// then rename over the destination - same approach as
// internal/config.Store.save, for the same reason (a crash/power loss
// mid-write must never leave a truncated settings.json behind).
func (s *Store) save(v Settings) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("settings: creating %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: encoding: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "settings-*.json.tmp")
	if err != nil {
		return fmt.Errorf("settings: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("settings: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("settings: closing temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("settings: replacing %s: %w", s.path, err)
	}
	return nil
}
