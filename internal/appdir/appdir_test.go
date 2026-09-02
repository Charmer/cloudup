package appdir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirIsNextToTheExecutableAndCreated(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	resolvedExe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks(%q) error = %v", exe, err)
	}
	wantParent := filepath.Dir(resolvedExe)

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	if got := filepath.Dir(dir); got != wantParent {
		t.Errorf("Dir() parent = %q, want %q (next to the executable)", got, wantParent)
	}
	if filepath.Base(dir) != "data" {
		t.Errorf("Dir() = %q, want a \"data\" subdirectory", dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("Dir() = %q was not created as a directory: %v", dir, err)
	}
}

func TestDirIsIdempotent(t *testing.T) {
	first, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(first) })

	second, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error = %v", err)
	}
	if first != second {
		t.Errorf("Dir() = %q then %q, want the same path both times", first, second)
	}
}
