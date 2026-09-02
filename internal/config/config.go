// Package config stores the non-secret half of every connection (provider
// type, display name, and provider-specific fields such as URL/bucket/
// region) as a single local JSON file. Secret fields (passwords, access
// keys, OAuth tokens) never appear here - they live in the OS keychain via
// internal/secrets, addressed by the same connection ID.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"cloudup/internal/appdir"
	"time"
)

// connectionIDField is the JSON key injected into every connection's
// ProviderConfig blob so provider factories (see internal/registry) can
// read their own connection ID out of the same cfg they otherwise use for
// non-secret settings - e.g. webdav.Config.ConnectionID.
const connectionIDField = "connectionId"

// Connection is one configured storage connection.
type Connection struct {
	ID             string          `json:"id"`
	ProviderType   string          `json:"providerType"`
	DisplayName    string          `json:"displayName"`
	ProviderConfig json.RawMessage `json:"providerConfig"`
	CreatedAt      time.Time       `json:"createdAt"`
}

type fileFormat struct {
	Connections []Connection `json:"connections"`
}

// Store is a JSON-file-backed CRUD store for Connections. Safe for
// concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
}

// DefaultPath returns the standard location for the config file: a "data"
// directory next to the running executable (see internal/appdir).
func DefaultPath() (string, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	return filepath.Join(dir, "config.json"), nil
}

// Open loads (or, if absent, initializes) the config file at path.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := s.save(fileFormat{Connections: []Connection{}}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("config: stat %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) load() (fileFormat, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fileFormat{}, fmt.Errorf("config: reading %s: %w", s.path, err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return fileFormat{}, fmt.Errorf("config: parsing %s: %w", s.path, err)
	}
	return f, nil
}

// save writes f atomically: write to a temp file in the same directory,
// then rename over the destination, so a crash/power loss mid-write never
// leaves a truncated or partially-written config.json behind.
func (s *Store) save(f fileFormat) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: creating %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("config: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("config: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: closing temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("config: replacing %s: %w", s.path, err)
	}
	return nil
}

// List returns every configured connection.
func (s *Store) List() ([]Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Connections, nil
}

// Get returns a single connection by ID.
func (s *Store) Get(id string) (Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return Connection{}, err
	}
	for _, c := range f.Connections {
		if c.ID == id {
			return c, nil
		}
	}
	return Connection{}, fmt.Errorf("config: connection %q not found", id)
}

// Add creates a new connection. fields holds the provider's non-secret
// config values (the ConfigSchema fields whose Type is not FieldPassword),
// keyed by FieldSpec.Key. A fresh ID is generated and injected into fields
// under connectionIDField before it is marshalled into ProviderConfig, so
// the provider's own factory can read it back (see webdav.Config).
func (s *Store) Add(providerType, displayName string, fields map[string]string) (Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return Connection{}, err
	}

	id, err := newID()
	if err != nil {
		return Connection{}, err
	}

	raw, err := encodeProviderConfig(id, fields)
	if err != nil {
		return Connection{}, err
	}

	conn := Connection{
		ID:             id,
		ProviderType:   providerType,
		DisplayName:    displayName,
		ProviderConfig: raw,
		CreatedAt:      time.Now().UTC(),
	}

	f.Connections = append(f.Connections, conn)
	if err := s.save(f); err != nil {
		return Connection{}, err
	}
	return conn, nil
}

// Update replaces the display name and non-secret fields of an existing
// connection. ProviderType and CreatedAt are immutable.
func (s *Store) Update(id, displayName string, fields map[string]string) (Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return Connection{}, err
	}

	for i, c := range f.Connections {
		if c.ID != id {
			continue
		}
		raw, err := encodeProviderConfig(id, fields)
		if err != nil {
			return Connection{}, err
		}
		f.Connections[i].DisplayName = displayName
		f.Connections[i].ProviderConfig = raw
		if err := s.save(f); err != nil {
			return Connection{}, err
		}
		return f.Connections[i], nil
	}
	return Connection{}, fmt.Errorf("config: connection %q not found", id)
}

// Remove deletes a connection from the config file. It does not touch the
// secret store - callers must separately clean up secrets (e.g. via
// secrets.Store.DeleteConnection) using the provider's ConfigSchema to know
// which secret keys to remove.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load()
	if err != nil {
		return err
	}

	for i, c := range f.Connections {
		if c.ID == id {
			f.Connections = append(f.Connections[:i], f.Connections[i+1:]...)
			return s.save(f)
		}
	}
	return fmt.Errorf("config: connection %q not found", id)
}

func encodeProviderConfig(connectionID string, fields map[string]string) (json.RawMessage, error) {
	merged := make(map[string]string, len(fields)+1)
	for k, v := range fields {
		merged[k] = v
	}
	merged[connectionIDField] = connectionID

	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("config: encoding provider config: %w", err)
	}
	return raw, nil
}

func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("config: generating id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
