// Package watch keeps a local record of "watch a local file/folder, upload
// anything that changes" rules and, given a queue.Manager and a way to
// resolve a connection ID into a provider.Provider, actually watches them
// (see Engine in engine.go).
//
// This is deliberately independent of internal/registry/internal/config,
// the same way internal/queue is: it takes an already-resolved
// provider.Provider rather than resolving one itself, so it stays testable
// with fakes and has no opinion on how a connection's config/secrets are
// stored. cmd/server is the only place that wires the three together.
//
// A watch only ever makes sense against the local filesystem of the
// machine cmd/server itself is running on - REST is just the protocol
// other programs use to talk to cloudup, not a filesystem window onto
// wherever those other programs happen to run, so this is not a
// limitation, just a fact about what a filesystem event can observe.
package watch

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cloudup/internal/appdir"
)

// Rule is one configured "watch this local path, upload changes through
// this connection" rule.
type Rule struct {
	ID           string    `json:"id"`
	LocalPath    string    `json:"localPath"`
	ConnectionID string    `json:"connectionId"`
	RemoteFolder string    `json:"remoteFolder"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"createdAt"`
}

type fileFormat struct {
	Rules []Rule `json:"rules"`
}

// Store is a JSON-file-backed CRUD store for Rules. Safe for concurrent
// use. Mirrors internal/config.Store's shape (atomic writes, random hex
// IDs) - see that package's doc comments for the reasoning, not repeated
// here.
type Store struct {
	mu   sync.Mutex
	path string
}

// DefaultPath returns the standard location for the watch rules file: a
// "data" directory next to the running executable (see internal/appdir).
func DefaultPath() (string, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return "", fmt.Errorf("watch: %w", err)
	}
	return filepath.Join(dir, "watches.json"), nil
}

// Open loads (or, if absent, initializes) the rules file at path.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := s.save(fileFormat{Rules: []Rule{}}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("watch: stat %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) load() (fileFormat, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fileFormat{}, fmt.Errorf("watch: reading %s: %w", s.path, err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return fileFormat{}, fmt.Errorf("watch: parsing %s: %w", s.path, err)
	}
	return f, nil
}

// save writes f atomically: write to a temp file in the same directory,
// then rename over the destination - see internal/config.Store.save's doc
// comment for why (crash/power-loss safety).
func (s *Store) save(f fileFormat) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("watch: creating %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("watch: encoding: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "watches-*.json.tmp")
	if err != nil {
		return fmt.Errorf("watch: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("watch: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("watch: closing temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("watch: replacing %s: %w", s.path, err)
	}
	return nil
}

// List returns every configured rule.
func (s *Store) List() ([]Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Rules, nil
}

// Get returns a single rule by ID.
func (s *Store) Get(id string) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return Rule{}, err
	}
	for _, r := range f.Rules {
		if r.ID == id {
			return r, nil
		}
	}
	return Rule{}, fmt.Errorf("watch: rule %q not found", id)
}

// Add creates a new rule. A fresh ID is generated; Enabled defaults to true
// (a rule you just created is presumably meant to watch immediately).
func (s *Store) Add(localPath, connectionID, remoteFolder string) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return Rule{}, err
	}

	id, err := newID()
	if err != nil {
		return Rule{}, err
	}

	rule := Rule{
		ID:           id,
		LocalPath:    localPath,
		ConnectionID: connectionID,
		RemoteFolder: remoteFolder,
		Enabled:      true,
		CreatedAt:    time.Now().UTC(),
	}

	f.Rules = append(f.Rules, rule)
	if err := s.save(f); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

// Update replaces an existing rule's path/connection/remote folder/enabled
// state. ID and CreatedAt are immutable.
func (s *Store) Update(id, localPath, connectionID, remoteFolder string, enabled bool) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return Rule{}, err
	}

	for i, r := range f.Rules {
		if r.ID != id {
			continue
		}
		f.Rules[i].LocalPath = localPath
		f.Rules[i].ConnectionID = connectionID
		f.Rules[i].RemoteFolder = remoteFolder
		f.Rules[i].Enabled = enabled
		if err := s.save(f); err != nil {
			return Rule{}, err
		}
		return f.Rules[i], nil
	}
	return Rule{}, fmt.Errorf("watch: rule %q not found", id)
}

// Remove deletes a rule from the store.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return err
	}

	for i, r := range f.Rules {
		if r.ID == id {
			f.Rules = append(f.Rules[:i], f.Rules[i+1:]...)
			return s.save(f)
		}
	}
	return fmt.Errorf("watch: rule %q not found", id)
}

func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("watch: generating id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
