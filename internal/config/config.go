package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/BurntSushi/toml"
)

// defaultQueueMaxAgeDays bounds how long a session may sit in the local upload
// queue before it is dropped. 30 days = 2x Core's UPLOAD_WINDOW_DAYS (15):
// enough runway for a transient overage on any tier to self-resolve as old
// jobs age out, while still bounding steady-state disk growth for the
// structural Free-tier case (5 uploads / rolling 15d) where arrival rate
// permanently exceeds admission rate. Overridable via QUEUE_MAX_AGE_DAYS.
const defaultQueueMaxAgeDays = 30

type Config struct {
	CoreURL string `toml:"core_url"`
	AppURL  string `toml:"app_url"`
	Games   []Game `toml:"games"`

	// QueueMaxAgeDays is env-driven (QUEUE_MAX_AGE_DAYS), not a TOML field —
	// it mirrors Core's env-configured UPLOAD_WINDOW_DAYS rather than being a
	// user-facing config-file setting. Always populated by Load().
	QueueMaxAgeDays int `toml:"-"`
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
		CoreURL:         "https://core.ludotrace.com",
		AppURL:          "https://app.ludotrace.com",
		QueueMaxAgeDays: queueMaxAgeDays(),
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

// CredentialPath is the on-disk fallback location for the opaque token, used
// only when the OS keychain is unavailable. On Windows the file content is
// DPAPI-encrypted; on other platforms no secure fallback exists and the path
// is unused.
func CredentialPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "credentials.bin"), nil
}

func OffsetPath(gameID string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "offsets", gameID+".offset"), nil
}

// KnownGamesCachePath is the last-known-good cache of Core's GET /v1/games
// response, used by the Add Game picker when Core is unreachable.
func KnownGamesCachePath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "known_games.json"), nil
}

// queueMaxAgeDays reads QUEUE_MAX_AGE_DAYS, falling back to
// defaultQueueMaxAgeDays when unset, unparseable, or non-positive. A
// non-positive value is treated as "use the default" rather than "never
// evict" — disabling the bound is not an env-typo outcome we want to honour.
func queueMaxAgeDays() int {
	v := os.Getenv("QUEUE_MAX_AGE_DAYS")
	if v == "" {
		return defaultQueueMaxAgeDays
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("config: invalid QUEUE_MAX_AGE_DAYS, using default",
			"value", v, "default", defaultQueueMaxAgeDays)
		return defaultQueueMaxAgeDays
	}
	return n
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
