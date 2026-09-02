// Package providertest offers test doubles for the contracts in
// internal/provider, so every storage-provider package does not have to
// hand-roll the same fakes.
//
// It is a normal (non-_test.go) package because Go only shares test helpers
// across packages that way - the same reason net/http/httptest and
// testing/fstest exist as ordinary packages. Living under internal/ keeps it
// out of any public API surface.
package providertest

import (
	"sync"

	"cloudup/internal/provider"
)

// MemSecretStore is an in-memory provider.SecretStore for tests. It mirrors
// internal/secrets' key namespacing (connectionID + ":" + key), so a test
// storing a secret for one connection cannot accidentally satisfy a lookup
// for another - which is exactly the mistake worth catching, given that
// provider factories read app-wide credentials and per-connection secrets
// through the same interface.
//
// A missing secret returns ("", nil) rather than an error, matching
// internal/secrets.Store: "not set" is a normal state, not a failure.
//
// Safe for concurrent use, since some providers (b2) resolve secrets from
// whichever goroutine first needs a session.
type MemSecretStore struct {
	mu     sync.RWMutex
	values map[string]string
}

// NewMemSecretStore returns an empty in-memory secret store.
func NewMemSecretStore() *MemSecretStore {
	return &MemSecretStore{values: map[string]string{}}
}

// Compile-time proof that this double actually satisfies the contract it
// stands in for - otherwise a change to provider.SecretStore would surface
// as a confusing error inside each provider's tests instead of here.
var _ provider.SecretStore = (*MemSecretStore)(nil)

func (s *MemSecretStore) key(connectionID, k string) string { return connectionID + ":" + k }

func (s *MemSecretStore) Get(connectionID, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.values[s.key(connectionID, key)], nil
}

func (s *MemSecretStore) Set(connectionID, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[s.key(connectionID, key)] = value
	return nil
}

func (s *MemSecretStore) Delete(connectionID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, s.key(connectionID, key))
	return nil
}

// WithSecrets is a convenience constructor for the common "seed a few
// secrets, then build a provider" test opening. Keys are given as
// connectionID -> key -> value.
func WithSecrets(seed map[string]map[string]string) *MemSecretStore {
	s := NewMemSecretStore()
	for connectionID, kv := range seed {
		for k, v := range kv {
			_ = s.Set(connectionID, k, v)
		}
	}
	return s
}
