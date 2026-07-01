//go:build !windows

package steam

import "github.com/ludotrace/client/internal/config"

// Discover is a no-op on non-Windows platforms — Steam's install location
// is only knowable via the Windows registry (see steam_windows.go). The
// Add Game flow falls straight to the folder picker.
func Discover() ([]config.Game, error) {
	return nil, nil
}

// DefaultBrowseDir always reports "not detected" on non-Windows platforms,
// so the picker defaults to the user's home directory.
func DefaultBrowseDir() (string, bool) {
	return "", false
}
