package proc

import "golang.org/x/sys/windows"

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not exited (STILL_ACTIVE / STATUS_PENDING). x/sys/windows does not
// export it under that name.
const stillActive = 259

// alive reports whether pid names a live process.
//
// A handle that cannot be opened means the process is gone (or unreachable,
// which for the update swap amounts to the same thing — we cannot learn
// anything more and the caller retries anyway). An open handle still has to be
// interrogated: Windows keeps the handle valid after exit so the exit code
// remains readable, and STILL_ACTIVE is what distinguishes the two.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
