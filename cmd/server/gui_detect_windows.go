//go:build windows

package main

import "os"

// hasGUISession is a best-effort heuristic on Windows: SESSIONNAME is set
// by Terminal Services for interactive sessions (the physical console is
// "Console", an RDP session is "RDP-Tcp#<n>") and is typically absent for
// processes started by the Service Control Manager or Task Scheduler
// running non-interactively in session 0. This is weaker evidence than the
// Linux DISPLAY/WAYLAND_DISPLAY check (see gui_detect_linux.go) - pass
// -gui=false explicitly when installing cloudup as a Windows service
// instead of relying on this.
func hasGUISession() bool {
	return os.Getenv("SESSIONNAME") != ""
}
