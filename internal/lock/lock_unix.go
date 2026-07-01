//go:build !windows

package lock

import (
	"errors"
	"os"
	"syscall"
)

// isAlive reports whether pid names a running process. Signal 0 performs
// no-op existence/permission checks without actually signaling the process.
func isAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the process exists but is owned by someone else — still alive.
	return errors.Is(err, syscall.EPERM)
}
