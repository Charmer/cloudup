package config

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return s
}

func TestOpenCreatesEmptyFile(t *testing.T) {
	s := openTestStore(t)
	conns, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(conns) != 0 {
		t.Fatalf("List() = %v, want empty", conns)
	}
}

func TestAddInjectsConnectionIDIntoProviderConfig(t *testing.T) {
	s := openTestStore(t)

	conn, err := s.Add("webdav", "My Nextcloud", map[string]string{
		"url":      "https://example.com/dav",
		"username": "alice",
	})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if conn.ID == "" {
		t.Fatal("Add() returned empty ID")
	}
	if conn.ProviderType != "webdav" || conn.DisplayName != "My Nextcloud" {
		t.Fatalf("Add() = %+v, unexpected provider type/display name", conn)
	}

	var decoded map[string]string
	if err := json.Unmarshal(conn.ProviderConfig, &decoded); err != nil {
		t.Fatalf("unmarshal ProviderConfig: %v", err)
	}
	if decoded["connectionId"] != conn.ID {
		t.Fatalf("ProviderConfig[connectionId] = %q, want %q", decoded["connectionId"], conn.ID)
	}
	if decoded["url"] != "https://example.com/dav" {
		t.Fatalf("ProviderConfig[url] = %q, want the original url", decoded["url"])
	}
}

func TestAddPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	conn, err := s1.Add("s3", "Backup bucket", map[string]string{"bucket": "backups"})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	got, err := s2.Get(conn.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.DisplayName != "Backup bucket" {
		t.Fatalf("Get() after reopen = %+v, want DisplayName %q", got, "Backup bucket")
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
	conn, err := s.Add("webdav", "Old name", map[string]string{"url": "https://old"})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	updated, err := s.Update(conn.ID, "New name", map[string]string{"url": "https://new"})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.ID != conn.ID {
		t.Fatalf("Update() changed ID: got %q, want %q", updated.ID, conn.ID)
	}
	if updated.ProviderType != "webdav" {
		t.Fatalf("Update() changed ProviderType: got %q", updated.ProviderType)
	}
	if !updated.CreatedAt.Equal(conn.CreatedAt) {
		t.Fatalf("Update() changed CreatedAt: got %v, want %v", updated.CreatedAt, conn.CreatedAt)
	}

	var decoded map[string]string
	if err := json.Unmarshal(updated.ProviderConfig, &decoded); err != nil {
		t.Fatalf("unmarshal ProviderConfig: %v", err)
	}
	if decoded["url"] != "https://new" {
		t.Fatalf("ProviderConfig[url] = %q, want %q", decoded["url"], "https://new")
	}
}

func TestUpdateUnknownIDReturnsError(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Update("does-not-exist", "x", nil); err == nil {
		t.Fatal("Update() with unknown id: expected error, got nil")
	}
}

func TestRemoveDeletesConnection(t *testing.T) {
	s := openTestStore(t)
	conn, err := s.Add("webdav", "Temp", map[string]string{"url": "https://x"})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	if err := s.Remove(conn.ID); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := s.Get(conn.ID); err == nil {
		t.Fatal("Get() after Remove(): expected error, got nil")
	}
}

func TestRemoveUnknownIDReturnsError(t *testing.T) {
	s := openTestStore(t)
	if err := s.Remove("does-not-exist"); err == nil {
		t.Fatal("Remove() with unknown id: expected error, got nil")
	}
}

func TestAddMultipleConnectionsGetUniqueIDs(t *testing.T) {
	s := openTestStore(t)
	c1, err := s.Add("webdav", "One", map[string]string{"url": "https://one"})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	c2, err := s.Add("webdav", "Two", map[string]string{"url": "https://two"})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if c1.ID == c2.ID {
		t.Fatalf("Add() produced duplicate IDs: %q", c1.ID)
	}

	conns, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(conns) != 2 {
		t.Fatalf("List() returned %d connections, want 2", len(conns))
	}
}
