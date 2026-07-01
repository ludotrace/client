package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ludotrace/client/internal/config"
)

const testDebounce = 50 * time.Millisecond

// callTracker records callbacks per gameID.
type callTracker struct {
	mu    sync.Mutex
	calls map[string]int
}

func newCallTracker() *callTracker {
	return &callTracker{calls: make(map[string]int)}
}

func (ct *callTracker) cb(gameID string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.calls[gameID]++
}

func (ct *callTracker) count(gameID string) int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.calls[gameID]
}

func waitFor(t *testing.T, condition func() bool, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func startWatcher(t *testing.T, games []config.Game, cb Callback) (context.CancelFunc, <-chan error) {
	t.Helper()
	w, err := New(games, cb, testDebounce)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx)
	}()
	return cancel, done
}

// TestStartupTrigger: events file exists → callback fires immediately; missing file does not.
func TestStartupTrigger(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	existsFile := "exists.jsonl"
	missingFile := "missing.jsonl"

	if err := os.WriteFile(filepath.Join(dir, existsFile), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	games := []config.Game{
		{GameID: "game-exists", WatchPath: dir, EventsFile: existsFile},
		{GameID: "game-missing", WatchPath: dir, EventsFile: missingFile},
	}

	ct := newCallTracker()
	cancel, done := startWatcher(t, games, ct.cb)
	defer func() {
		cancel()
		<-done
	}()

	if !waitFor(t, func() bool { return ct.count("game-exists") >= 1 }, 500*time.Millisecond) {
		t.Error("expected startup callback for game-exists")
	}

	time.Sleep(100 * time.Millisecond)
	if n := ct.count("game-missing"); n != 0 {
		t.Errorf("expected no startup callback for game-missing, got %d", n)
	}
}

// TestWriteEvent: writing to the events file triggers a callback after debounce.
func TestWriteEvent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	eventsFile := "events.jsonl"
	eventsPath := filepath.Join(dir, eventsFile)

	if err := os.WriteFile(eventsPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	games := []config.Game{
		{GameID: "game1", WatchPath: dir, EventsFile: eventsFile},
	}

	ct := newCallTracker()
	cancel, done := startWatcher(t, games, ct.cb)
	defer func() {
		cancel()
		<-done
	}()

	// Wait past startup trigger.
	if !waitFor(t, func() bool { return ct.count("game1") >= 1 }, 500*time.Millisecond) {
		t.Fatal("startup callback not received")
	}
	before := ct.count("game1")

	f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if !waitFor(t, func() bool { return ct.count("game1") > before }, time.Second) {
		t.Error("expected callback after write event")
	}
}

// TestDebounceCoalesces: a burst of writes produces only one callback.
func TestDebounceCoalesces(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	eventsFile := "events.jsonl"
	eventsPath := filepath.Join(dir, eventsFile)

	if err := os.WriteFile(eventsPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	games := []config.Game{
		{GameID: "burst", WatchPath: dir, EventsFile: eventsFile},
	}

	ct := newCallTracker()
	cancel, done := startWatcher(t, games, ct.cb)
	defer func() {
		cancel()
		<-done
	}()

	// Wait past startup trigger.
	if !waitFor(t, func() bool { return ct.count("burst") >= 1 }, 500*time.Millisecond) {
		t.Fatal("startup callback not received")
	}
	before := ct.count("burst")

	// Write rapidly — all within the debounce window.
	for i := 0; i < 5; i++ {
		f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString("{}\n")
		f.Close()
		time.Sleep(5 * time.Millisecond)
	}

	// Wait for debounce to settle.
	time.Sleep(testDebounce + 50*time.Millisecond)

	got := ct.count("burst") - before
	if got != 1 {
		t.Errorf("expected 1 coalesced callback, got %d", got)
	}
}

// TestMissingEventsFile: creating the file triggers a callback.
func TestMissingEventsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	eventsFile := "new.jsonl"
	eventsPath := filepath.Join(dir, eventsFile)

	games := []config.Game{
		{GameID: "newgame", WatchPath: dir, EventsFile: eventsFile},
	}

	ct := newCallTracker()
	cancel, done := startWatcher(t, games, ct.cb)
	defer func() {
		cancel()
		<-done
	}()

	// Give watcher time to start watching the directory.
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(eventsPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	if !waitFor(t, func() bool { return ct.count("newgame") >= 1 }, time.Second) {
		t.Error("expected callback after events file was created")
	}
}

// TestCancellationStopsCallbacks: no callbacks are fired after ctx is cancelled.
func TestCancellationStopsCallbacks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	eventsFile := "events.jsonl"
	eventsPath := filepath.Join(dir, eventsFile)

	if err := os.WriteFile(eventsPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	games := []config.Game{
		{GameID: "cancel-game", WatchPath: dir, EventsFile: eventsFile},
	}

	var fired atomic.Bool
	// Only count callbacks after cancel.
	cancelled := make(chan struct{})
	cb := func(gameID string) {
		select {
		case <-cancelled:
			fired.Store(true)
		default:
		}
	}

	w, err := New(games, cb, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx)
	}()

	// Wait for watcher to be live.
	time.Sleep(50 * time.Millisecond)

	cancel()
	close(cancelled)

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("watcher did not stop after cancel")
		return
	}

	// Write after cancel — no callback should fire.
	f, _ := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if f != nil {
		f.WriteString("{}\n")
		f.Close()
	}

	time.Sleep(testDebounce + 50*time.Millisecond)

	if fired.Load() {
		t.Error("callback fired after context was cancelled")
	}
}

// TestAddGame: a game added while the watcher is stopped is picked up.
func TestAddGame(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	eventsFile := "added.jsonl"
	eventsPath := filepath.Join(dir, eventsFile)
	if err := os.WriteFile(eventsPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := New(nil, func(string) {}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.fw.Close()

	g := config.Game{GameID: "added-game", WatchPath: dir, EventsFile: eventsFile}
	if err := w.AddGame(g); err != nil {
		t.Fatalf("AddGame: %v", err)
	}

	w.mu.RLock()
	n := len(w.games)
	_, inMap := w.pathMap[eventsPath]
	w.mu.RUnlock()

	if n != 1 {
		t.Errorf("len(games) = %d, want 1", n)
	}
	if !inMap {
		t.Error("expected eventsPath registered in pathMap")
	}
}

// TestAddGameConcurrent: calling AddGame repeatedly from multiple goroutines
// while Start()'s event loop is running must not race on games/pathMap.
// Run with -race to be meaningful.
func TestAddGameConcurrent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	w, err := New(nil, func(string) {}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			gameDir := filepath.Join(dir, fmt.Sprintf("game-%d", i))
			if err := os.MkdirAll(gameDir, 0o755); err != nil {
				errs <- err
				return
			}
			g := config.Game{
				GameID:     fmt.Sprintf("game-%d", i),
				WatchPath:  gameDir,
				EventsFile: "events.jsonl",
			}
			if err := w.AddGame(g); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("AddGame: %v", err)
	}

	w.mu.RLock()
	got := len(w.games)
	w.mu.RUnlock()
	if got != n {
		t.Errorf("len(games) = %d, want %d", got, n)
	}
}

// TestAddGameMissingPath: a watch_path that doesn't exist is rejected and
// nothing is registered.
func TestAddGameMissingPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")

	w, err := New(nil, func(string) {}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.fw.Close()

	g := config.Game{GameID: "missing-path", WatchPath: missing, EventsFile: "events.jsonl"}
	if err := w.AddGame(g); err == nil {
		t.Fatal("expected error for missing watch_path, got nil")
	}

	w.mu.RLock()
	n := len(w.games)
	w.mu.RUnlock()
	if n != 0 {
		t.Errorf("len(games) = %d, want 0 (nothing should be registered)", n)
	}
}

// TestAddGamePathIsFile: a watch_path that exists but is a file, not a
// directory, is rejected.
func TestAddGamePathIsFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "im-a-file")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := New(nil, func(string) {}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.fw.Close()

	g := config.Game{GameID: "file-path", WatchPath: filePath, EventsFile: "events.jsonl"}
	if err := w.AddGame(g); err == nil {
		t.Fatal("expected error for non-directory watch_path, got nil")
	}

	w.mu.RLock()
	n := len(w.games)
	w.mu.RUnlock()
	if n != 0 {
		t.Errorf("len(games) = %d, want 0", n)
	}
}

// TestAddGamePathVanishesBeforeAdd: simulates the "picked/discovered folder
// deleted between selection and AddGame()" edge case from the spec's I/O
// matrix — the caller only has a config.Game value, built before the
// directory was removed.
func TestAddGamePathVanishesBeforeAdd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	vanished := filepath.Join(dir, "will-vanish")
	if err := os.Mkdir(vanished, 0o755); err != nil {
		t.Fatal(err)
	}

	g := config.Game{GameID: "vanished", WatchPath: vanished, EventsFile: "events.jsonl"}

	// Simulate the external drive being ejected / folder deleted between
	// pick and add.
	if err := os.Remove(vanished); err != nil {
		t.Fatal(err)
	}

	w, err := New(nil, func(string) {}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.fw.Close()

	if err := w.AddGame(g); err == nil {
		t.Fatal("expected error when watch_path vanished before AddGame, got nil")
	}

	w.mu.RLock()
	n := len(w.games)
	_, inMap := w.pathMap[vanished]
	w.mu.RUnlock()
	if n != 0 || inMap {
		t.Error("expected no registration for a vanished watch_path")
	}
}

// TestAddGameWhileRunningIsWatched: a game added via AddGame() while
// Start()'s event loop is live is actually watched — a write to its events
// file triggers the callback, with no restart.
func TestAddGameWhileRunningIsWatched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	eventsFile := "live.jsonl"
	eventsPath := filepath.Join(dir, eventsFile)
	if err := os.WriteFile(eventsPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	ct := newCallTracker()
	w, err := New(nil, ct.cb, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// Let the event loop start selecting before we add.
	time.Sleep(50 * time.Millisecond)

	g := config.Game{GameID: "live-game", WatchPath: dir, EventsFile: eventsFile}
	if err := w.AddGame(g); err != nil {
		t.Fatalf("AddGame: %v", err)
	}

	f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if !waitFor(t, func() bool { return ct.count("live-game") >= 1 }, time.Second) {
		t.Error("expected callback after write to a game added while running")
	}
}
