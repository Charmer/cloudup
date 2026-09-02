//go:build !linux && !windows

package main

// hasGUISession has no reliable headless signal on this platform (notably
// macOS, where even background/CI processes usually still have a GUI
// session available) - default to true, matching cloudup's behavior
// before auto-detection existed. Pass -gui=false explicitly for a headless
// deployment on this platform.
func hasGUISession() bool {
	return true
}
