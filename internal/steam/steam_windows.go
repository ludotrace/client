//go:build windows

package steam

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"

	"github.com/ludotrace/client/internal/config"
)

const (
	steamRegKey   = `SOFTWARE\Valve\Steam`
	steamRegValue = "SteamPath"
)

// Discover scans Steam's default and additional library folders for games
// in Registry, returning a config.Game for each one found on disk that
// isn't already accounted for (dedupe against existing config is the
// caller's job — see steam.DedupeNew).
//
// Steam not being detected (registry key missing/unreadable) is treated as
// "nothing found" rather than an error — see known-games-registry.md and
// the I/O matrix's "Registry read fails" row. It is logged at slog.Warn,
// not surfaced as a user-facing error.
func Discover() ([]config.Game, error) {
	steamPath, ok := readSteamPath()
	if !ok {
		return nil, nil
	}
	return ScanLibraries(libraryRoots(steamPath)), nil
}

// DefaultBrowseDir returns the folder the picker should default to when
// Steam is detected: <SteamPath>\steamapps\common. ok is false if Steam
// wasn't detected, in which case the caller falls back to the home
// directory.
func DefaultBrowseDir() (string, bool) {
	steamPath, ok := readSteamPath()
	if !ok {
		return "", false
	}
	return filepath.Join(steamPath, "steamapps", "common"), true
}

func readSteamPath() (string, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, steamRegKey, registry.QUERY_VALUE)
	if err != nil {
		slog.Warn("steam: could not open registry key", "key", steamRegKey, "err", err)
		return "", false
	}
	defer k.Close()

	v, _, err := k.GetStringValue(steamRegValue)
	if err != nil || v == "" {
		slog.Warn("steam: SteamPath registry value missing or unreadable", "err", err)
		return "", false
	}
	return v, true
}

// libraryRoots returns the default Steam install path plus any additional
// library folders declared in libraryfolders.vdf. A missing/unparsable VDF
// falls back to just the default path — the default library alone is still
// a useful partial result.
func libraryRoots(steamPath string) []string {
	roots := []string{steamPath}

	f, err := os.Open(filepath.Join(steamPath, "config", "libraryfolders.vdf"))
	if err != nil {
		return roots
	}
	defer f.Close()

	parsed, err := ParseLibraryFolders(f)
	if err != nil {
		slog.Warn("steam: error scanning libraryfolders.vdf, some library paths may be missing", "err", err)
	}

	seen := map[string]bool{strings.ToLower(steamPath): true}
	for _, p := range parsed {
		key := strings.ToLower(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		roots = append(roots, p)
	}
	return roots
}
