package provider

// SecretStore is the read-only view of the secret storage (OS keychain)
// that a provider factory needs: fetch the secret fields declared through
// ConfigSchema (FieldPassword) for one connection, keyed by connectionID.
// The concrete implementation (internal/secrets) wraps go-keyring; provider
// packages never touch the keychain directly.
//
// Deliberately read-only. Writing secrets is the job of whoever accepts
// them from the user (internal/httpapi when a connection is created or its
// OAuth flow completes), and that code holds the concrete *secrets.Store,
// so nothing needs Set/Delete through this interface - a provider factory
// asking to *write* a secret would be a design smell worth noticing at
// compile time. This interface used to carry all three methods while
// claiming in this very comment to be "the narrow view a provider factory
// needs", which was not true.
//
// A missing secret is ("", nil), not an error: "not set" is a normal state
// (see internal/secrets.Store.Get).
type SecretStore interface {
	Get(connectionID, key string) (string, error)
}
