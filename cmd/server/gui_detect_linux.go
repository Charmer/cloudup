//go:build linux

package main

import "os"

// hasGUISession reports whether this process appears to be running in an
// environment with a graphical session available. It is used to
// auto-select GUI (tray+browser) vs headless-service mode when -gui isn't
// passed explicitly - see run() in main.go.
//
// On Linux this checks the same environment variables every X11/Wayland-
// aware tool checks (DISPLAY, WAYLAND_DISPLAY) - a standard, widely relied
// upon signal: unset on a bare VM/container/systemd service, set in any
// real desktop session (including one reached over SSH with X11
// forwarding).
func hasGUISession() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}
