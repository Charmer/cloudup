package watch

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watches.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return s
}

func TestOpenCreatesEmptyFile(t *testing.T) {
	s := openTestStore(t)
	rules, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("List() = %v, want empty", rules)
	}
}

func TestAddDefaultsToEnabled(t *testing.T) {
	s := openTestStore(t)
	rule, err := s.Add("/watched", "conn1", "remote/prefix")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if rule.ID == "" {
		t.Fatal("Add() returned empty ID")
	}
	if !rule.Enabled {
		t.Fatal("Add() rule.Enabled = false, want true")
	}
	if rule.LocalPath != "/watched" || rule.ConnectionID != "conn1" || rule.RemoteFolder != "remote/prefix" {
		t.Fatalf("Add() = %+v, unexpected fields", rule)
	}
}

func TestAddPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watches.json")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	rule, err := s1.Add("/watched", "conn1", "")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	got, err := s2.Get(rule.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LocalPath != "/watched" {
		t.Fatalf("Get() after reopen = %+v, want LocalPath %q", got, "/watched")
	}
}

func TestGetUnknownIDReturnsError(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Get("does-not-exist"); err == nil {
		t.Fatal("Get() with unknown id: expected error, got nil")
	}
}

func TestUpdateReplacesFieldsKeepsIdentity(t *testing.T) {
	s := openTestStore(t)
	rule, err := s.Add("/old", "conn1", "")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	updated, err := s.Update(rule.ID, "/new", "conn2", "prefix", false)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.ID != rule.ID {
		t.Fatalf("Update() changed ID: got %q, want %q", updated.ID, rule.ID)
	}
	if !updated.CreatedAt.Equal(rule.CreatedAt) {
		t.Fatalf("Update() changed CreatedAt: got %v, want %v", updated.CreatedAt, rule.CreatedAt)
	}
	if updated.LocalPath != "/new" || updated.ConnectionID != "conn2" || updated.RemoteFolder != "prefix" || updated.Enabled {
		t.Fatalf("Update() = %+v, unexpected fields", updated)
	}
}

func TestUpdateUnknownIDReturnsError(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Update("does-not-exist", "/x", "conn1", "", true); err == nil {
		t.Fatal("Update() with unknown id: expected error, got nil")
	}
}

func TestRemoveDeletesRule(t *testing.T) {
	s := openTestStore(t)
	rule, err := s.Add("/watched", "conn1", "")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if err := s.Remove(rule.ID); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := s.Get(rule.ID); err == nil {
		t.Fatal("Get() after Remove(): expected error, got nil")
	}
}

func TestRemoveUnknownIDReturnsError(t *testing.T) {
	s := openTestStore(t)
	if err := s.Remove("does-not-exist"); err == nil {
		t.Fatal("Remove() with unknown id: expected error, got nil")
	}
}

func TestAddMultipleRulesGetUniqueIDs(t *testing.T) {
	s := openTestStore(t)
	r1, err := s.Add("/one", "conn1", "")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	r2, err := s.Add("/two", "conn1", "")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if r1.ID == r2.ID {
		t.Fatalf("Add() produced duplicate IDs: %q", r1.ID)
	}

	rules, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("List() returned %d rules, want 2", len(rules))
	}
}
