package watcher

import (
	"context"
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
