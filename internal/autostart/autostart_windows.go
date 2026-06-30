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
)

// Register writes the current executable path to the Windows startup registry
// key so the client launches automatically on login. Idempotent — safe to call
// on every startup.
//
// The value is quoted ("path\to\exe.exe") so Windows interprets it as a
// single argument even when the path contains spaces.
func Register() {
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("autostart: could not resolve executable path", "err", err)
		return
	}

	// Wrap in quotes so Windows shell splits the command correctly when the
	// path contains spaces (e.g. C:\Users\John Smith\AppData\ludotrace.exe).
	value := `"` + exe + `"`

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
