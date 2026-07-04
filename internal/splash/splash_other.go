//go:build !windows

package splash

// show is a no-op on non-Windows platforms. The windowless-double-click problem
// that motivates the splash is Windows-specific, and a native window on macOS
// (Cocoa) / Linux (GTK) needs CGO — an explicit follow-up, deliberately not
// blocking this. Returns an already-finished handle so callers stay uniform.
func show() *Handle {
	return noopHandle()
}
