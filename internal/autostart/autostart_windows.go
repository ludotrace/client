//go:build windows

package autostart

import (
	"log/slog"
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey    = `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`
	valueName = "LudoTrace"
	// autostartArg marks a launch as coming from the Run key (login autostart)
	// rather than an interactive double-click. main uses it to suppress the
	// launch splash on every boot. Keep in sync with the literal checked in
	// internal/splash (DecideLaunch); the two packages don't share a const to
	// avoid an import just for this string.
	autostartArg = "--autostart"
)

// Register writes the current executable path to the Windows startup registry
// key so the client launches automatically on login. Idempotent — safe to call
// on every startup.
//
// The value is quoted ("path\to\exe.exe") so Windows interprets it as a
// single argument even when the path contains spaces, and carries the
// --autostart marker so main can tell a login launch from an interactive
// double-click (and skip the launch splash on boot).
func Register() {
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("autostart: could not resolve executable path", "err", err)
		return
	}

	// Wrap in quotes so Windows shell splits the command correctly when the
	// path contains spaces (e.g. C:\Users\John Smith\AppData\ludotrace.exe),
	// then append the --autostart marker as a separate argument.
	value := `"` + exe + `" ` + autostartArg

	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		slog.Warn("autostart: could not open registry key", "err", err)
		return
	}
	defer k.Close()

	// Skip the write if the stored value already matches. Use EqualFold for
	// Windows path case-insensitivity.
	existing, _, err := k.GetStringValue(valueName)
	if err == nil && strings.EqualFold(existing, value) {
		return
	}

	if err := k.SetStringValue(valueName, value); err != nil {
		slog.Warn("autostart: could not write registry value", "err", err)
		return
	}
	slog.Info("autostart: registered in Windows startup", "path", exe)
}
