// Package appdir resolves the single directory cloudup stores every bit of
// its local state in: connection config, upload history, app settings,
// watch rules, and (in GUI mode) log files.
//
// Deliberately NOT the OS-standard per-user config location
// (os.UserConfigDir() - %APPDATA% / ~/Library/Application Support /
// ~/.config): cloudup is meant to be fully portable - unzip a release
// archive anywhere, or move that folder somewhere else entirely, and every
// bit of its state travels with it. Nothing is ever written outside the
// folder the binary itself lives in. This also means two different
// cloudup installs (e.g. a downloaded release next to a locally built
// binary) never silently share state just because they happen to run
// under the same OS user account, which os.UserConfigDir() would have
// caused.
package appdir

import (
	"fmt"
	"os"
	"path/filepath"
)

// Dir returns the "data" directory next to the running executable,
// creating it if it doesn't exist yet. Every internal/{config,history,
// settings,watch}.DefaultPath and cmd/server's log files live under here.
//
// Note for local development: `go run` compiles to a throwaway binary in
// a temporary directory that's deleted after the process exits, so its
// "next to the executable" data directory does not persist across runs -
// build a real binary first (`go build -o cloudup-server ./cmd/server`)
// for anything that needs state to survive a restart.
func Dir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("appdir: resolving executable path: %w", err)
	}
	// os.Executable() does not itself guarantee the result isn't a
	// symlink; resolving it means "next to the executable" is the real
	// installed location even if it was launched through one.
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("appdir: resolving symlinks: %w", err)
	}

	dir := filepath.Join(filepath.Dir(resolved), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("appdir: creating %s: %w - cloudup needs write access to its own folder; move it out of Program Files, /usr, or another admin-owned location and into a folder you own. Don't just run elevated to work around this: Windows won't show a system tray icon for an elevated process in a normal desktop session, and there's no way to close it except killing it from an elevated Task Manager", dir, err)
	}
	return dir, nil
}
