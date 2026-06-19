package watcher

import (
	"context"
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
	games    []*gameWatch
	pathMap  map[string]*gameWatch
	cb       Callback
	debounce time.Duration
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
		eventsPath := filepath.Join(g.WatchPath, g.EventsFile)
		gw := &gameWatch{
			game:       g,
			eventsPath: eventsPath,
		}

		if _, err := os.Stat(eventsPath); os.IsNotExist(err) {
			gw.watchingDir = true
			if err2 := fw.Add(g.WatchPath); err2 != nil {
				fw.Close()
				return nil, err2
			}
			w.pathMap[g.WatchPath] = gw
		} else {
			if err2 := fw.Add(eventsPath); err2 != nil {
				fw.Close()
				return nil, err2
			}
			w.pathMap[eventsPath] = gw
		}

		w.games = append(w.games, gw)
	}

	return w, nil
}

func (w *Watcher) Start(ctx context.Context) error {
	for _, gw := range w.games {
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
	gw, ok := w.pathMap[event.Name]
	if !ok {
		// event.Name may be an absolute path; also check directory case where
		// the event fires on a file inside a watched directory.
		dir := filepath.Dir(event.Name)
		gw, ok = w.pathMap[dir]
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
		delete(w.pathMap, gw.game.WatchPath)

		if err := w.fw.Add(gw.eventsPath); err == nil {
			gw.mu.Lock()
			gw.watchingDir = false
			gw.mu.Unlock()
			w.pathMap[gw.eventsPath] = gw
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
	for _, gw := range w.games {
		gw.mu.Lock()
		if gw.timer != nil {
			gw.timer.Stop()
			gw.timer = nil
		}
		gw.mu.Unlock()
	}
}
