//go:build !windows

package proc

import (
	"errors"
	"syscall"
)

// alive reports whether pid names a live process. Signal 0 performs the
// permission and existence checks without delivering anything: nil means the
// process is there and ours, EPERM means it is there and someone else's.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
