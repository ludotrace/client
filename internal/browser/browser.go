// Package browser opens a URL or file path in the user's default OS handler.
//
// It exists as one shared implementation so the platform-specific launch quirks
// live in exactly one place — previously both internal/auth and internal/tray
// carried their own copy, and a Windows-only truncation bug had to be fixed in
// both.
package browser

import (
	"os/exec"
	"runtime"
)

// Open launches target (a URL or file path) in the OS default handler.
func Open(target string) error {
	return buildCmd(target).Start()
}

// buildCmd builds the launch command for the current OS. Split from Open so the
// platform branching is unit-testable without actually spawning a browser.
func buildCmd(target string) *exec.Cmd {
	return buildCmdFor(runtime.GOOS, target)
}

// buildCmdFor is the goos-parameterised core, so tests can exercise every
// platform's branch from a single host.
//
// On Windows it deliberately does NOT use `cmd /c start <url>`: cmd.exe's line
// parser treats an unquoted "&" as a command separator, so a URL carrying a
// query string like "...&state=..." is silently truncated at the first "&"
// before start.exe ever sees it — which surfaced as Core rejecting the sign-in
// with "missing state". rundll32 receives target as a single argv entry and
// never goes through the cmd.exe parser, so the "&" is preserved.
func buildCmdFor(goos, target string) *exec.Cmd {
	switch goos {
	case "darwin":
		return exec.Command("open", target)
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default: // linux and others
		return exec.Command("xdg-open", target)
	}
}
