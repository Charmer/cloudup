package secrets

import (
	"testing"

	"github.com/zalando/go-keyring"
)

// TestMain switches go-keyring to its in-memory mock so tests never touch
// the real OS keychain (and pass in CI/headless environments without a
// Secret Service / unlocked keychain available).
func TestMain(m *testing.M) {
	keyring.MockInit()
	m.Run()
}

func TestGetUnsetSecretReturnsEmptyNoError(t *testing.T) {
	s := New()
	value, err := s.Get("conn1", "password")
	if err != nil {
		t.Fatalf("Get() error = %v, want nil for unset secret", err)
	}
	if value != "" {
		t.Fatalf("Get() = %q, want empty string for unset secret", value)
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	s := New()
	if err := s.Set("conn1", "password", "s3cr3t"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	value, err := s.Get("conn1", "password")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if value != "s3cr3t" {
		t.Fatalf("Get() = %q, want %q", value, "s3cr3t")
	}
}

func TestSetIsolatedByConnectionAndKey(t *testing.T) {
	s := New()
	must(t, s.Set("conn1", "password", "conn1-pass"))
	must(t, s.Set("conn2", "password", "conn2-pass"))
	must(t, s.Set("conn1", "username", "conn1-user"))

	assertGet(t, s, "conn1", "password", "conn1-pass")
	assertGet(t, s, "conn2", "password", "conn2-pass")
	assertGet(t, s, "conn1", "username", "conn1-user")
}

func TestDeleteRemovesSecret(t *testing.T) {
	s := New()
	must(t, s.Set("conn1", "password", "s3cr3t"))

	if err := s.Delete("conn1", "password"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	assertGet(t, s, "conn1", "password", "")
}

func TestDeleteAbsentSecretIsNotError(t *testing.T) {
	s := New()
	if err := s.Delete("conn-never-existed", "password"); err != nil {
		t.Fatalf("Delete() on absent secret: error = %v, want nil", err)
	}
}

func TestDeleteConnectionRemovesAllListedKeys(t *testing.T) {
	s := New()
	must(t, s.Set("conn1", "username", "u"))
	must(t, s.Set("conn1", "password", "p"))

	if err := s.DeleteConnection("conn1", []string{"username", "password"}); err != nil {
		t.Fatalf("DeleteConnection() error = %v", err)
	}

	assertGet(t, s, "conn1", "username", "")
	assertGet(t, s, "conn1", "password", "")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertGet(t *testing.T, s *Store, connectionID, key, want string) {
	t.Helper()
	got, err := s.Get(connectionID, key)
	if err != nil {
		t.Fatalf("Get(%q, %q) error = %v", connectionID, key, err)
	}
	if got != want {
		t.Fatalf("Get(%q, %q) = %q, want %q", connectionID, key, got, want)
	}
}
