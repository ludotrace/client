// Package proc answers one question — is a process still running? — across
// the three target platforms.
//
// It exists for the update swap: the outgoing binary must be fully gone before
// its executable can be replaced, and on Windows the file stays locked for a
// moment after the parent process has exited. Waiting on the parent PID is the
// deterministic way to know that moment has passed.
package proc

import "time"

// pollInterval is how often WaitForExit re-checks. Short enough that the swap
// is not perceptibly delayed, long enough to be a rounding error in CPU terms.
const pollInterval = 50 * time.Millisecond

// WaitForExit blocks until pid is no longer running, or until timeout elapses.
// It reports whether the process is gone.
//
// A false return is not fatal to a caller: it means "still running, or could
// not be determined", and the caller should fall back to retrying whatever it
// needed the exit for.
func WaitForExit(pid int, timeout time.Duration) bool {
	if pid <= 0 {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		if !alive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}
