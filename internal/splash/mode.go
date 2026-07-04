// Package splash shows a brief, borderless "the client started" flash of the
// org's hero heartbeat mark on an interactive (double-click) launch.
//
// The Windows client is built -H=windowsgui (windowless): a double-click gives
// zero visible feedback and the process goes straight to the system tray, which
// is easy to miss. The splash is pure launch confirmation — one heartbeat cycle
// at 2x speed (~2.1s), then it auto-dismisses. It never blocks startup and a
// failure to create it is logged and ignored.
//
// The window itself is Windows-only (splash_windows.go); every other platform
// gets a no-op (splash_other.go). The launch-mode decision below is pure and
// platform-independent so it can be unit-tested without opening a window.
package splash

// Launch describes how the process was started, derived purely from os.Args.
type Launch struct {
	// Interactive is true only for a bare-args launch — the double-click case.
	// Any special argument (a login-autostart marker, an update one-shot, or
	// anything else) makes the launch non-interactive and suppresses the splash.
	Interactive bool
}

// suppressArgs are the first-argument markers that identify a non-interactive
// launch. --autostart is written into the Windows Run key by
// internal/autostart; --finish-update and --check-update are update one-shots
// handled in main before the tray ever starts.
//
// The list is defensive belt-and-suspenders for --finish-update/--check-update:
// those branches return/exit in main before the splash is ever triggered, but
// classifying them here keeps DecideLaunch a complete, self-contained account
// of which launches flash.
var suppressArgs = map[string]bool{
	"--autostart":     true,
	"--finish-update": true,
	"--check-update":  true,
}

// DecideLaunch classifies a launch from its raw argv (pass os.Args directly).
// The splash fires only on a bare-args launch — no arguments beyond the program
// name, which is exactly the double-click case. Any extra argument, recognized
// suppress marker or not, is treated as a non-interactive/automated launch and
// suppresses the splash.
func DecideLaunch(args []string) Launch {
	return Launch{Interactive: len(args) <= 1}
}

// isSuppressMarker reports whether s is a recognized non-interactive launch
// marker. It exists to document and test the specific markers that must never
// flash; DecideLaunch itself suppresses on any extra argument, so this is not
// on its decision path.
func isSuppressMarker(s string) bool { return suppressArgs[s] }
