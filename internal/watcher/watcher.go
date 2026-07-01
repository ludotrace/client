package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ludotrace/client/internal/config"
)

type Callback func(gameID string)

type gameWatch struct {
	game        config.Game
	eventsPath  string
	watchingDir bool
	timer       *time.Timer
	mu          sync.Mutex
}

type Watcher struct {
	fw       *fsnotify.Watcher
	cb       Callback
	debounce time.Duration

	// mu guards games and pathMap. Start()'s event loop reads them
	// concurrently once running, and AddGame can be called from another
	// goroutine at any time after the watcher is live — unlike gameWatch.mu,
	// which only ever guards a single game's own fields.
	mu      sync.RWMutex
	games   []*gameWatch
	pathMap map[string]*gameWatch
}

func New(games []config.Game, cb Callback, debounce time.Duration) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		fw:       fw,
		pathMap:  make(map[string]*gameWatch),
		cb:       cb,
		debounce: debounce,
	}

	for _, g := range games {
		gw, key, err := w.setupGameWatch(g)
		if err != nil {
			fw.Close()
			return nil, err
		}
		w.pathMap[key] = gw
		w.games = append(w.games, gw)
	}

	return w, nil
}

// AddGame registers a new game with a watcher that may already be running
// (Start()'s event loop is reading games/pathMap concurrently once
// started). It stats g.WatchPath first and returns an error without
// registering anything if the path is missing or not a directory, so a
// path that vanished between pick and add (e.g. an ejected external drive)
// never becomes a zombie watch entry — AppendGame's own AddGame bypasses
// config.Load()'s filterGames() stat check, so this is the only guard.
//
// Idempotent per GameID: if config.AppendGame fails after a prior AddGame
// call already registered the watch, and the caller retries the whole Add
// Game flow for the same game, a second AddGame call here is a no-op rather
// than appending a duplicate gameWatch/pathMap entry — there is no
// RemoveGame/rollback to undo the first registration.
func (w *Watcher) AddGame(g config.Game) error {
	info, err := os.Stat(g.WatchPath)
	if err != nil {
		return fmt.Errorf("watcher: add game %q: watch_path %q: %w", g.GameID, g.WatchPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("watcher: add game %q: watch_path %q is not a directory", g.GameID, g.WatchPath)
	}

	if w.hasGame(g.GameID) {
		return nil
	}

	gw, key, err := w.setupGameWatch(g)
	if err != nil {
		return fmt.Errorf("watcher: add game %q: %w", g.GameID, err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	for _, existing := range w.games {
		if existing.game.GameID == g.GameID {
			// Lost the race to a concurrent AddGame for the same game — the
			// fsnotify watch just added above is a harmless duplicate handle
			// on the same path; leave it, don't register a second gameWatch.
			return nil
		}
	}
	w.pathMap[key] = gw
	w.games = append(w.games, gw)

	return nil
}

// hasGame reports whether gameID is already registered.
func (w *Watcher) hasGame(gameID string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, gw := range w.games {
		if gw.game.GameID == gameID {
			return true
		}
	}
	return false
}

// setupGameWatch is the shared per-game setup used by both New() and
// AddGame(): stat the events file to decide whether to watch the file
// directly or its parent directory (promoted to a file watch later, on
// CREATE), then register with fsnotify. It does not touch w.games/w.pathMap
// — callers do that under w.mu.
func (w *Watcher) setupGameWatch(g config.Game) (*gameWatch, string, error) {
	eventsPath := filepath.Join(g.WatchPath, g.EventsFile)
	gw := &gameWatch{
		game:       g,
		eventsPath: eventsPath,
	}

	if _, err := os.Stat(eventsPath); os.IsNotExist(err) {
		gw.watchingDir = true
		if err := w.fw.Add(g.WatchPath); err != nil {
			return nil, "", err
		}
		return gw, g.WatchPath, nil
	}

	if err := w.fw.Add(eventsPath); err != nil {
		return nil, "", err
	}
	return gw, eventsPath, nil
}

func (w *Watcher) Start(ctx context.Context) error {
	for _, gw := range w.gamesSnapshot() {
		if _, err := os.Stat(gw.eventsPath); err == nil {
			w.cb(gw.game.GameID)
		}
	}

	for {
		select {
		case <-ctx.Done():
			w.fw.Close()
			w.drainTimers()
			return ctx.Err()

		case event, ok := <-w.fw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(ctx, event)

		case _, ok := <-w.fw.Errors:
			if !ok {
				return nil
			}
		}
	}
}

func (w *Watcher) handleEvent(ctx context.Context, event fsnotify.Event) {
	gw, ok := w.lookupPath(event.Name)
	if !ok {
		// event.Name may be an absolute path; also check directory case where
		// the event fires on a file inside a watched directory.
		dir := filepath.Dir(event.Name)
		gw, ok = w.lookupPath(dir)
		if !ok {
			return
		}
	}

	if gw.watchingDir {
		if event.Op&fsnotify.Create == 0 {
			return
		}
		if filepath.Base(event.Name) != gw.game.EventsFile {
			return
		}

		// Switch from directory watch to file watch.
		_ = w.fw.Remove(gw.game.WatchPath)
		w.mu.Lock()
		delete(w.pathMap, gw.game.WatchPath)
		w.mu.Unlock()

		if err := w.fw.Add(gw.eventsPath); err == nil {
			gw.mu.Lock()
			gw.watchingDir = false
			gw.mu.Unlock()

			w.mu.Lock()
			w.pathMap[gw.eventsPath] = gw
			w.mu.Unlock()
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
		w.cb(gw.game.GameID)
		return
	}

	if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
		return
	}

	gw.mu.Lock()
	defer gw.mu.Unlock()

	if gw.timer != nil {
		gw.timer.Reset(w.debounce)
		return
	}

	gameID := gw.game.GameID
	gw.timer = time.AfterFunc(w.debounce, func() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		gw.mu.Lock()
		gw.timer = nil
		gw.mu.Unlock()

		w.cb(gameID)
	})
}

func (w *Watcher) drainTimers() {
	for _, gw := range w.gamesSnapshot() {
		gw.mu.Lock()
		if gw.timer != nil {
			gw.timer.Stop()
			gw.timer = nil
		}
		gw.mu.Unlock()
	}
}

// lookupPath is a lock-guarded read of pathMap, safe to call while AddGame
// may be running concurrently on another goroutine.
func (w *Watcher) lookupPath(name string) (*gameWatch, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	gw, ok := w.pathMap[name]
	return gw, ok
}

// gamesSnapshot returns a copy of the games slice header, safe to range
// over while AddGame may append to it concurrently on another goroutine.
func (w *Watcher) gamesSnapshot() []*gameWatch {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]*gameWatch, len(w.games))
	copy(out, w.games)
	return out
}
