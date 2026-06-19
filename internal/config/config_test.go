package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withConfigFile(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := loadFrom
	loadFrom = func() (string, error) { return path, nil }
	t.Cleanup(func() { loadFrom = orig })
}

func TestLoad_ValidTOML(t *testing.T) {
	tmpDir := t.TempDir()
	watchPath := filepath.Join(tmpDir, "fo4")
	if err := os.Mkdir(watchPath, 0o755); err != nil {
		t.Fatal(err)
	}

	withConfigFile(t, `
core_url = "https://core.ludotrace.gg"

[[games]]
game_id     = "fallout4"
watch_path  = "`+watchPath+`"
events_file = "lt_fo4_events.jsonl"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CoreURL != "https://core.ludotrace.gg" {
		t.Errorf("CoreURL = %q", cfg.CoreURL)
	}
	if len(cfg.Games) != 1 {
		t.Fatalf("expected 1 game, got %d", len(cfg.Games))
	}
	if cfg.Games[0].GameID != "fallout4" {
		t.Errorf("GameID = %q", cfg.Games[0].GameID)
	}
}

func TestLoad_MissingFile_ReturnsDefaults(t *testing.T) {
	orig := loadFrom
	loadFrom = func() (string, error) {
		return filepath.Join(t.TempDir(), "config.toml"), nil
	}
	t.Cleanup(func() { loadFrom = orig })

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CoreURL != "https://core.ludotrace.gg" {
		t.Errorf("default CoreURL = %q", cfg.CoreURL)
	}
	if len(cfg.Games) != 0 {
		t.Errorf("expected 0 games, got %d", len(cfg.Games))
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	withConfigFile(t, `core_url = "https://core.ludotrace.gg"`)
	t.Setenv("LUDOTRACE_CORE_URL", "http://localhost:8080")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CoreURL != "http://localhost:8080" {
		t.Errorf("CoreURL = %q, want http://localhost:8080", cfg.CoreURL)
	}
}

func TestLoad_MissingWatchPath_SkipsGame(t *testing.T) {
	withConfigFile(t, `
core_url = "https://core.ludotrace.gg"

[[games]]
game_id     = "fallout4"
watch_path  = "/does/not/exist/at/all"
events_file = "lt_fo4_events.jsonl"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Games) != 0 {
		t.Errorf("expected 0 games after skip, got %d", len(cfg.Games))
	}
}

func TestLoad_InvalidCoreURL(t *testing.T) {
	withConfigFile(t, `core_url = "not-a-url"`)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid core_url")
	}
}

func TestLoad_MissingGameID_SkipsGame(t *testing.T) {
	tmpDir := t.TempDir()
	watchPath := filepath.Join(tmpDir, "game")
	if err := os.Mkdir(watchPath, 0o755); err != nil {
		t.Fatal(err)
	}

	withConfigFile(t, `
core_url = "https://core.ludotrace.gg"

[[games]]
game_id     = ""
watch_path  = "`+watchPath+`"
events_file = "lt_foo_events.jsonl"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Games) != 0 {
		t.Errorf("expected 0 games, got %d", len(cfg.Games))
	}
}

func TestAppendGame_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	watchPath := filepath.Join(tmpDir, "stardew")
	if err := os.Mkdir(watchPath, 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(tmpDir, "config.toml")
	orig := loadFrom
	loadFrom = func() (string, error) { return configPath, nil }
	t.Cleanup(func() { loadFrom = orig })

	g := Game{
		GameID:     "stardew",
		WatchPath:  watchPath,
		EventsFile: "lt_stardew_events.jsonl",
	}
	if err := AppendGame(g); err != nil {
		t.Fatalf("AppendGame: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after AppendGame: %v", err)
	}
	if len(cfg.Games) != 1 {
		t.Fatalf("expected 1 game, got %d", len(cfg.Games))
	}
	if cfg.Games[0].GameID != "stardew" {
		t.Errorf("GameID = %q", cfg.Games[0].GameID)
	}
}

func TestPathHelpers(t *testing.T) {
	tests := []struct {
		name   string
		fn     func() (string, error)
		suffix string
	}{
		{"Dir", Dir, "ludotrace"},
		{"FilePath", FilePath, filepath.Join("ludotrace", "config.toml")},
		{"QueuePath", QueuePath, filepath.Join("ludotrace", "queue.json")},
		{"LockPath", LockPath, filepath.Join("ludotrace", "ludotrace.lock")},
		{"OffsetPath", func() (string, error) { return OffsetPath("fallout4") },
			filepath.Join("ludotrace", "offsets", "fallout4.offset")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.fn()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.HasSuffix(got, tt.suffix) {
				t.Errorf("got %q, want suffix %q", got, tt.suffix)
			}
		})
	}
}
