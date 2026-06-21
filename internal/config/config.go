package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	CoreURL string `toml:"core_url"`
	AppURL  string `toml:"app_url"`
	Games   []Game `toml:"games"`
}

type Game struct {
	GameID     string `toml:"game_id"`
	WatchPath  string `toml:"watch_path"`
	EventsFile string `toml:"events_file"`
}

// loadFrom is the internal loader used by Load and tests.
var loadFrom = func() (string, error) {
	return FilePath()
}

func Load() (*Config, error) {
	path, err := loadFrom()
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		CoreURL: "https://core.ludotrace.com",
		AppURL:  "https://app.ludotrace.com",
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil
	}

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if v := os.Getenv("LUDOTRACE_CORE_URL"); v != "" {
		cfg.CoreURL = v
	}
	if v := os.Getenv("LUDOTRACE_APP_URL"); v != "" {
		cfg.AppURL = v
	}

	if err := validateURL(cfg.CoreURL); err != nil {
		return nil, fmt.Errorf("config: core_url: %w", err)
	}
	if err := validateURL(cfg.AppURL); err != nil {
		return nil, fmt.Errorf("config: app_url: %w", err)
	}

	cfg.Games = filterGames(cfg.Games)

	return cfg, nil
}

func Save(cfg *Config) error {
	path, err := loadFrom()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: mkdir: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("config: create %s: %w", path, err)
	}
	defer f.Close()

	return toml.NewEncoder(f).Encode(cfg)
}

func AppendGame(g Game) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	cfg.Games = append(cfg.Games, g)
	return Save(cfg)
}

func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: user config dir: %w", err)
	}
	return filepath.Join(base, "ludotrace"), nil
}

func FilePath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.toml"), nil
}

func QueuePath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "queue.json"), nil
}

func LockPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "ludotrace.lock"), nil
}

func OffsetPath(gameID string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "offsets", gameID+".offset"), nil
}

func validateURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("must be an absolute URL with scheme and host, got %q", raw)
	}
	return nil
}

func filterGames(games []Game) []Game {
	out := games[:0]
	for _, g := range games {
		if g.GameID == "" || g.EventsFile == "" {
			slog.Warn("config: skipping game with missing game_id or events_file", "game", g)
			continue
		}
		info, err := os.Stat(g.WatchPath)
		if err != nil || !info.IsDir() {
			slog.Warn("config: skipping game with missing or non-directory watch_path",
				"game_id", g.GameID, "watch_path", g.WatchPath)
			continue
		}
		out = append(out, g)
	}
	return out
}
