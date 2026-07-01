//go:build windows

package lock

import "golang.org/x/sys/windows"

// stillActive is the Windows API STILL_ACTIVE sentinel (259), not exported
// by golang.org/x/sys/windows.
const stillActive = 259

// isAlive reports whether pid names a running process, by attempting to
// open a limited-info handle to it. A dead or never-existed PID fails to
// open; a live process (ours or another user's, since we hold no elevated
// privilege requirement here) opens successfully.
func isAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}
