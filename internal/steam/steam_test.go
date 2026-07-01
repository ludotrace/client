package steam

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ludotrace/client/internal/config"
)

func TestMatchFolder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		folder string
		wantID string
		wantOK bool
	}{
		{"exact match", "Stardew Valley", "stardew", true},
		{"case-insensitive match", "stardew valley", "stardew", true},
		{"mixed case match", "STARDEW VALLEY", "stardew", true},
		{"no match", "Fallout 4", "", false},
		{"empty folder name", "", "", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kg, ok := MatchFolder(tt.folder)
			if ok != tt.wantOK {
				t.Fatalf("MatchFolder(%q) ok = %v, want %v", tt.folder, ok, tt.wantOK)
			}
			if ok && kg.GameID != tt.wantID {
				t.Errorf("MatchFolder(%q) game_id = %q, want %q", tt.folder, kg.GameID, tt.wantID)
			}
		})
	}
}

func TestGameFromFolder(t *testing.T) {
	t.Parallel()

	kg := KnownGame{GameID: "stardew", SteamFolder: "Stardew Valley", DisplayName: "Stardew Valley", EventsFile: "lt_stardew_events.jsonl"}
	g := GameFromFolder(kg, "/some/watch/path")

	if g.GameID != "stardew" || g.WatchPath != "/some/watch/path" || g.EventsFile != "lt_stardew_events.jsonl" {
		t.Errorf("GameFromFolder = %+v, unexpected", g)
	}
}

func TestDedupeNew(t *testing.T) {
	t.Parallel()

	discovered := []config.Game{
		{GameID: "stardew", WatchPath: "/a", EventsFile: "lt_stardew_events.jsonl"},
		{GameID: "otherid", WatchPath: "/b", EventsFile: "lt_other_events.jsonl"},
	}

	t.Run("no overlap: both pass through", func(t *testing.T) {
		t.Parallel()
		out := DedupeNew(discovered, nil)
		if len(out) != 2 {
			t.Fatalf("len(out) = %d, want 2", len(out))
		}
	})

	t.Run("existing already has the game: filtered out", func(t *testing.T) {
		t.Parallel()
		existing := []config.Game{{GameID: "stardew", WatchPath: "/existing", EventsFile: "x"}}
		out := DedupeNew(discovered, existing)
		if len(out) != 1 || out[0].GameID != "otherid" {
			t.Fatalf("DedupeNew = %+v, want only otherid", out)
		}
	})

	t.Run("everything already configured: empty result", func(t *testing.T) {
		t.Parallel()
		existing := []config.Game{
			{GameID: "stardew"},
			{GameID: "otherid"},
		}
		out := DedupeNew(discovered, existing)
		if len(out) != 0 {
			t.Fatalf("DedupeNew = %+v, want empty", out)
		}
	})

	t.Run("nothing discovered", func(t *testing.T) {
		t.Parallel()
		out := DedupeNew(nil, nil)
		if len(out) != 0 {
			t.Fatalf("DedupeNew = %+v, want empty", out)
		}
	})
}

func TestParseLibraryFolders(t *testing.T) {
	t.Parallel()

	vdf := `"libraryfolders"
{
	"0"
	{
		"path"		"C:\\Program Files (x86)\\Steam"
		"label"		""
		"contentid"		"123456"
	}
	"1"
	{
		"path"		"D:\\SteamLibrary"
		"label"		""
	}
}
`
	got, err := ParseLibraryFolders(strings.NewReader(vdf))
	if err != nil {
		t.Fatalf("ParseLibraryFolders returned err: %v", err)
	}
	want := []string{`C:\Program Files (x86)\Steam`, `D:\SteamLibrary`}

	if len(got) != len(want) {
		t.Fatalf("ParseLibraryFolders = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseLibraryFoldersEscapedQuote(t *testing.T) {
	t.Parallel()

	vdf := `"path"		"D:\\Games\\Valve\"s Library"` + "\n"
	got, err := ParseLibraryFolders(strings.NewReader(vdf))
	if err != nil {
		t.Fatalf("ParseLibraryFolders returned err: %v", err)
	}
	want := []string{`D:\Games\Valve"s Library`}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("ParseLibraryFolders = %v, want %v", got, want)
	}
}

func TestParseLibraryFoldersEmpty(t *testing.T) {
	t.Parallel()

	got, err := ParseLibraryFolders(strings.NewReader(""))
	if err != nil {
		t.Errorf("ParseLibraryFolders(empty) returned err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ParseLibraryFolders(empty) = %v, want empty", got)
	}

	got, err = ParseLibraryFolders(strings.NewReader("garbage\nnot vdf at all\n"))
	if err != nil {
		t.Errorf("ParseLibraryFolders(garbage) returned err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ParseLibraryFolders(garbage) = %v, want empty", got)
	}
}

func TestScanLibraries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	common := filepath.Join(root, "steamapps", "common")
	if err := os.MkdirAll(filepath.Join(common, "Stardew Valley"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(common, "Some Other Game"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file (not a directory) with a matching name must not match.
	if err := os.WriteFile(filepath.Join(common, "not-a-dir"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	found := ScanLibraries([]string{root})
	if len(found) != 1 {
		t.Fatalf("ScanLibraries found %d games, want 1: %+v", len(found), found)
	}
	if found[0].GameID != "stardew" {
		t.Errorf("found[0].GameID = %q, want stardew", found[0].GameID)
	}
	wantPath := filepath.Join(common, "Stardew Valley")
	if found[0].WatchPath != wantPath {
		t.Errorf("found[0].WatchPath = %q, want %q", found[0].WatchPath, wantPath)
	}
}

func TestScanLibrariesMissingCommonDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir() // no steamapps/common at all
	found := ScanLibraries([]string{root})
	if len(found) != 0 {
		t.Errorf("ScanLibraries on empty root = %+v, want empty", found)
	}
}

func TestScanLibrariesDedupesAcrossRoots(t *testing.T) {
	t.Parallel()

	rootA := t.TempDir()
	rootB := t.TempDir()
	for _, root := range []string{rootA, rootB} {
		if err := os.MkdirAll(filepath.Join(root, "steamapps", "common", "Stardew Valley"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	found := ScanLibraries([]string{rootA, rootB})
	if len(found) != 1 {
		t.Fatalf("ScanLibraries across duplicate roots = %+v, want exactly 1 (first root wins)", found)
	}
}
