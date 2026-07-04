// Package knowngames sources the Add Game picker's list of LudoTrace-supported
// games from Core's authoritative GET /v1/games, instead of a compile-time
// constant that goes stale the moment Core adds a new game (client#29).
//
// A disk cache backs the live call: List writes the cache on every
// successful fetch and falls back to reading it when Core is unreachable or
// the client isn't signed in, so the picker still offers something useful
// offline. Only when neither a live call nor a cache is available does List
// return an error.
package knowngames

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ludotrace/client/internal/auth"
	"github.com/ludotrace/client/internal/config"
)

// Game is a LudoTrace-supported game as reported by Core's GET /v1/games.
type Game struct {
	GameID string `json:"game_id"`
	Name   string `json:"name"`
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// List returns the known-games list, preferring a live call to
// {coreURL}/v1/games and falling back to the last-cached response.
func List(ctx context.Context, coreURL string, authClient auth.Client) ([]Game, error) {
	games, err := fetch(ctx, coreURL, authClient)
	if err == nil {
		if cerr := writeCache(games); cerr != nil {
			slog.Warn("knowngames: failed to write cache", "err", cerr)
		}
		return games, nil
	}

	cached, cerr := ReadCache()
	if cerr != nil {
		return nil, fmt.Errorf("knowngames: fetch failed (%w) and no cache available (%v)", err, cerr)
	}
	slog.Warn("knowngames: fetch failed, using cached known-games list", "err", err)
	return cached, nil
}

func fetch(ctx context.Context, coreURL string, authClient auth.Client) ([]Game, error) {
	token, err := authClient.GetToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coreURL+"/v1/games", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var body struct {
		Games []Game `json:"games"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return body.Games, nil
}

func writeCache(games []Game) error {
	path, err := config.KnownGamesCachePath()
	if err != nil {
		return err
	}
	data, err := json.Marshal(games)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ReadCache reads the last-cached known-games list without making a network
// call. Exported so callers with no context/auth handy (e.g. a post-add
// display-name lookup, moments after List already warmed the cache) can
// reuse it cheaply.
func ReadCache() ([]Game, error) {
	path, err := config.KnownGamesCachePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var games []Game
	if err := json.Unmarshal(data, &games); err != nil {
		return nil, err
	}
	return games, nil
}
