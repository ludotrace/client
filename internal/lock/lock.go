// Package lock implements the client's singleton-instance guard.
//
// The lock file stores the holder's PID. A process that dies without
// running its cleanup (killed, crashed, BSOD, power loss) leaves the file
// behind; on the next Acquire, a lock file whose PID is no longer running
// is treated as stale and reclaimed rather than blocking forever.
package lock

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// ErrHeld is returned when the lock is held by a process that is still alive.
var ErrHeld = errors.New("lock: already held by a running process")

type Lock struct {
	path string
	file *os.File
}

// Acquire claims the singleton lock at path. If an existing lock file names
// a PID that is no longer running (or is empty/unparseable, e.g. left by an
// older build that didn't write a PID), it is reclaimed. Otherwise ErrHeld
// is returned.
func Acquire(path string) (*Lock, error) {
	f, err := create(path)
	if err != nil {
		if !os.IsExist(err) {
			return nil, err
		}
		if !reclaimStale(path) {
			return nil, ErrHeld
		}
		f, err = create(path)
		if err != nil {
			return nil, err
		}
	}

	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}

	return &Lock{path: path, file: f}, nil
}

// Release closes and removes the lock file.
func (l *Lock) Release() {
	l.file.Close()
	os.Remove(l.path)
}

func create(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

// reclaimStale removes path if the PID it names is not a live process.
// An empty or unparseable file (e.g. the old empty-file lock format) is
// treated as stale too, since it can never be verified as held.
func reclaimStale(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	text := strings.TrimSpace(string(data))
	if text != "" {
		pid, err := strconv.Atoi(text)
		if err == nil && isAlive(pid) {
			return false
		}
	}

	return os.Remove(path) == nil
}
