//go:build !windows

package autostart

// Register is a no-op on non-Windows platforms.
func Register() {}
