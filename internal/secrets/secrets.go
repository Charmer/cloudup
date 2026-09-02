// Package secrets implements provider.SecretStore on top of the OS
// keychain (Windows Credential Manager / macOS Keychain / Linux Secret
// Service) via go-keyring. Provider packages never talk to the keychain
// directly - they only see the narrow provider.SecretStore interface.
package secrets

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"

	"cloudup/internal/provider"
)

// defaultService namespaces this app's entries in the OS keychain so they
// don't collide with unrelated applications' credentials.
const defaultService = "cloudup"

// Store implements provider.SecretStore.
type Store struct {
	service string
}

var _ provider.SecretStore = (*Store)(nil)

// New returns a Store backed by the OS keychain under the default service
// name.
func New() *Store {
	return &Store{service: defaultService}
}

// account builds the per-secret account name used within the keychain
// service. Each (connectionID, key) pair - e.g. one connection's
// "password" field - is stored as its own keychain entry.
func (s *Store) account(connectionID, key string) string {
	return connectionID + ":" + key
}

// Get returns the stored secret. A secret that was never set is not an
// error condition (e.g. an optional field, or a connection created before
// its password was filled in) - it returns "", nil. Only unexpected
// keychain failures (locked keychain, missing Secret Service on Linux,
// etc.) are returned as errors.
func (s *Store) Get(connectionID, key string) (string, error) {
	value, err := keyring.Get(s.service, s.account(connectionID, key))
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("secrets: get %s/%s: %w", connectionID, key, err)
	}
	return value, nil
}

// Set stores or overwrites a secret.
func (s *Store) Set(connectionID, key, value string) error {
	if err := keyring.Set(s.service, s.account(connectionID, key), value); err != nil {
		return fmt.Errorf("secrets: set %s/%s: %w", connectionID, key, err)
	}
	return nil
}

// Delete removes a secret. Deleting an already-absent secret is not an
// error, to keep callers (e.g. "remove connection" cleanup) simple.
func (s *Store) Delete(connectionID, key string) error {
	err := keyring.Delete(s.service, s.account(connectionID, key))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("secrets: delete %s/%s: %w", connectionID, key, err)
	}
	return nil
}

// DeleteConnection removes every secret listed in keys for connectionID.
// Intended for use when a connection is removed entirely: the caller
// passes the FieldPassword keys from that provider's ConfigSchema so all of
// its secrets are cleaned up, not just a single field.
func (s *Store) DeleteConnection(connectionID string, keys []string) error {
	for _, key := range keys {
		if err := s.Delete(connectionID, key); err != nil {
			return err
		}
	}
	return nil
}
