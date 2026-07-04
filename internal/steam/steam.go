// Package steam implements Steam library discovery for the Add Game flow.
//
// Discover() is build-tagged per-OS (steam_windows.go does real registry +
// VDF discovery, steam_other.go is a no-op) since Steam's install location
// is only knowable via the Windows registry. Everything else in this file —
// the known-games Registry, folder-name matching, config.Game resolution,
// dedupe, and VDF parsing — is OS-agnostic and shared by both.
package steam

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ludotrace/client/internal/config"
)

// KnownGame is a locally known LudoTrace-supported game, keyed by its Steam
// library folder name under steamapps/common. MVP-scoped: no Core API
// dependency, no free-text game_id field. See
// internal/_bmad-output/specs/spec-add-game-dialog/known-games-registry.md
// for why only Stardew Valley is wired (its watch_path is its Steam install
// folder; other games, e.g. Fallout 4, need a different resolution mode).
type KnownGame struct {
	GameID      string
	SteamFolder string // folder name under steamapps/common/<SteamFolder>
	DisplayName string
	EventsFile  string
}

// Registry is the MVP known-games table.
var Registry = []KnownGame{
	{
		GameID:      "stardew",
		SteamFolder: "Stardew Valley",
		DisplayName: "Stardew Valley",
		EventsFile:  "lt_stardew_events.jsonl",
	},
}

// MatchFolder looks up a KnownGame by Steam folder name (case-insensitive).
// Used both by Windows registry+VDF discovery and by the folder-picker's
// recognized-folder path.
func MatchFolder(folderName string) (KnownGame, bool) {
	for _, kg := range Registry {
		if strings.EqualFold(kg.SteamFolder, folderName) {
			return kg, true
		}
	}
	return KnownGame{}, false
}

// GameFromFolder resolves a config.Game for kg using watchPath as the
// watch_path. True for every entry in Registry today: the Steam folder IS
// the watch path (see known-games-registry.md — this stops holding for a
// game like Fallout 4, not yet in scope).
func GameFromFolder(kg KnownGame, watchPath string) config.Game {
	return config.Game{
		GameID:     kg.GameID,
		WatchPath:  watchPath,
		EventsFile: kg.EventsFile,
	}
}

// EventsFileName returns the conventional events-file name for a game_id
// (root CLAUDE.md's JSONL schema convention: lt_<game>_events.jsonl). Used to
// resolve EventsFile for games sourced from Core's GET /v1/games, which
// carries only game_id + name — not the file name (client#29 Part B).
func EventsFileName(gameID string) string {
	return fmt.Sprintf("lt_%s_events.jsonl", gameID)
}

// DedupeNew filters discovered down to games whose GameID is not already
// present in existing. Order of discovered is preserved.
func DedupeNew(discovered, existing []config.Game) []config.Game {
	have := make(map[string]bool, len(existing))
	for _, g := range existing {
		have[g.GameID] = true
	}

	var out []config.Game
	for _, g := range discovered {
		if have[g.GameID] {
			continue
		}
		out = append(out, g)
	}
	return out
}

// vdfUnescaper unescapes VDF string values in file order: escaped quotes
// before escaped backslashes, so `\"` isn't first mangled by a stray
// backslash removal.
var vdfUnescaper = strings.NewReplacer(`\"`, `"`, `\\`, `\`)

// ParseLibraryFolders hand-parses Valve's KeyValues VDF format (as used by
// libraryfolders.vdf), extracting every "path" value in file order. Good
// enough for libraryfolders.vdf's flat structure — no general VDF library is
// a dependency. Escaped backslashes ("\\\\") and escaped quotes (`\"`) are
// unescaped. The returned error is a scan error (e.g. an I/O failure) that
// may have truncated the result before every "path" line was read; paths
// collected before the error are still returned.
func ParseLibraryFolders(r io.Reader) ([]string, error) {
	var paths []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, `"path"`) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, `"path"`))
		rest = strings.Trim(rest, `"`)
		if rest == "" {
			continue
		}
		paths = append(paths, vdfUnescaper.Replace(rest))
	}
	return paths, sc.Err()
}

// ScanLibraries checks steamapps/common under each library root for
// directories matching a Registry entry's SteamFolder, returning a
// config.Game for each match found on disk. Pure filesystem walk — no
// registry dependency, shared by Discover() implementations (and directly
// testable with t.TempDir() fixtures on any OS).
func ScanLibraries(roots []string) []config.Game {
	var found []config.Game
	seen := make(map[string]bool)
	for _, root := range roots {
		commonDir := filepath.Join(root, "steamapps", "common")
		entries, err := os.ReadDir(commonDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			kg, ok := MatchFolder(e.Name())
			if !ok || seen[kg.GameID] {
				continue
			}
			seen[kg.GameID] = true
			found = append(found, GameFromFolder(kg, filepath.Join(commonDir, e.Name())))
		}
	}
	return found
}
