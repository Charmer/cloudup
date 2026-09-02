// Package registry lets provider implementations register themselves by
// type name (typically from an init() in their own package), so the core
// never needs a switch/case over concrete provider types. Adding a new
// storage backend means adding a new package and importing it for its
// side-effecting init() - nothing in this package or its callers changes.
package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"cloudup/internal/provider"
)

// Factory builds a provider.Provider instance from its (non-secret) JSON
// config and a handle to the secret store for reading credentials.
type Factory func(cfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error)

// SchemaFunc returns a provider type's connection-form field descriptions.
// Unlike Factory, it needs no config or secrets - every provider's
// ConfigFields() is static (it never reads instance state), so this lets
// the UI render an "add connection" form before any config exists yet,
// sidestepping the chicken-and-egg problem of needing a valid Config to
// construct a Provider just to ask it what its Config should contain.
type SchemaFunc func() []provider.FieldSpec

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
	schemas   = map[string]SchemaFunc{}
	oauths    = map[string]provider.OAuthFlow{}
)

// Register associates a provider type identifier (e.g. "s3") with the
// factory that constructs it. Intended to be called from an init() in the
// provider's own package. Panics on duplicate registration, since that
// indicates a programming error (two packages claiming the same type), not
// a runtime condition to recover from.
func Register(providerType string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := factories[providerType]; exists {
		panic(fmt.Sprintf("registry: provider type %q already registered", providerType))
	}
	factories[providerType] = f
}

// RegisterSchema associates a provider type identifier with the function
// that describes its connection-form fields. Intended to be called from the
// same init() as Register, alongside it - see e.g.
// internal/providers/webdav's init().
func RegisterSchema(providerType string, f SchemaFunc) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := schemas[providerType]; exists {
		panic(fmt.Sprintf("registry: schema for provider type %q already registered", providerType))
	}
	schemas[providerType] = f
}

// Schema returns the connection-form fields for a registered provider type.
func Schema(providerType string) ([]provider.FieldSpec, error) {
	mu.RLock()
	f, ok := schemas[providerType]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("registry: unknown provider type %q", providerType)
	}
	return f(), nil
}

// RegisterOAuth associates a provider type identifier with the interactive
// authorization flow its connections need before they can be constructed at
// all (see provider.OAuthFlow for why this is a registered value rather
// than an optional interface on Provider). Intended to be called from the
// same init() as Register - only the two OAuth-based providers
// (googledrive, dropbox) call it; every other type simply never appears in
// this map. Panics on duplicate registration, for the same reason Register
// does.
func RegisterOAuth(providerType string, f provider.OAuthFlow) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := oauths[providerType]; exists {
		panic(fmt.Sprintf("registry: oauth flow for provider type %q already registered", providerType))
	}
	oauths[providerType] = f
}

// OAuth returns the interactive authorization flow registered for a
// provider type. The bool result is false for every type that needs no such
// step (webdav, s3, b2) - that is the normal case, not an error, so callers
// branch on it rather than getting an error back.
func OAuth(providerType string) (provider.OAuthFlow, bool) {
	mu.RLock()
	defer mu.RUnlock()
	f, ok := oauths[providerType]
	return f, ok
}

// RequiresOAuth reports whether a provider type has a registered
// interactive authorization flow. It exists so a UI layer can decide
// whether to offer an "Authorize" action for a connection without having to
// receive (and ignore) the whole OAuthFlow value - see
// internal/httpapi's GET /api/v1/provider-types.
func RequiresOAuth(providerType string) bool {
	_, ok := OAuth(providerType)
	return ok
}

// ConnectionSecretKeys returns every SecretStore key a connection of this
// provider type may own: its FieldPassword form fields, plus the refresh
// token if the type has an interactive OAuth flow (that key is deliberately
// absent from the schema - see provider.OAuthFlow - so nothing else can
// derive it). Callers use it to clean up after a deleted connection without
// hardcoding either list.
//
// It goes through Schema rather than constructing the provider, so it also
// works for a connection that could never be constructed in the first place
// - an OAuth connection deleted before it was ever authorized is exactly
// the case where the leftover secrets most need clearing. An unknown
// provider type yields no keys rather than an error: there is nothing to
// clean up and nothing the caller could do about it.
func ConnectionSecretKeys(providerType string) []string {
	fields, err := Schema(providerType)
	if err != nil {
		return nil
	}
	var keys []string
	for _, f := range fields {
		if f.Type == provider.FieldPassword {
			keys = append(keys, f.Key)
		}
	}
	if flow, ok := OAuth(providerType); ok {
		keys = append(keys, flow.RefreshTokenKey)
	}
	return keys
}

// Create instantiates a provider by its registered type identifier.
func Create(providerType string, cfg json.RawMessage, secrets provider.SecretStore) (provider.Provider, error) {
	mu.RLock()
	f, ok := factories[providerType]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("registry: unknown provider type %q", providerType)
	}
	return f(cfg, secrets)
}

// Types returns the currently registered provider type identifiers, sorted
// for stable output (e.g. populating a "provider type" dropdown in the UI).
func Types() []string {
	mu.RLock()
	defer mu.RUnlock()
	types := make([]string, 0, len(factories))
	for t := range factories {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}
