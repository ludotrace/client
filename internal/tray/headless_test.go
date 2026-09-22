package tray

import (
	"context"
	"testing"
	"time"
)

// newTestTray builds a Tray with only the channels the headless loop touches.
// New() needs an auth.Client and a *queue.Queue that RunHeadless never calls.
func newTestTray() *Tray {
	return &Tray{
		stateCh:    make(chan State, 8),
		updateCh:   make(chan updateNote, 1),
		retryCh:    make(chan struct{}, 1),
		signedInCh: make(chan struct{}, 1),
	}
}

func TestRunHeadlessReturnsOnContextCancel(t *testing.T) {
	tr := newTestTray()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		tr.RunHeadless(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunHeadless did not return after context cancellation")
	}
}

// The reason RunHeadless drains rather than simply blocking on ctx. stateCh is
// buffered 8, so without a consumer the ninth SetState blocks its caller — the
// upload worker — forever. This sends well past the buffer to prove a headless
// daemon cannot wedge its own pipeline.
func TestRunHeadlessKeepsStateProducersUnblocked(t *testing.T) {
	tr := newTestTray()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go tr.RunHeadless(ctx)

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < cap(tr.stateCh)*10; i++ {
			tr.SetState(StateIdle)
		}
	}()

	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatalf("SetState blocked: headless loop is not draining stateCh (buffer %d)", cap(tr.stateCh))
	}
}

// NotifyUpdateReady must not block either — nothing in a headless run can click
// "Restart to Update", so the note is logged and discarded.
func TestRunHeadlessKeepsUpdateProducersUnblocked(t *testing.T) {
	tr := newTestTray()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go tr.RunHeadless(ctx)

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < 20; i++ {
			tr.NotifyUpdateReady("v1.2.3", "/tmp/pending", nil)
		}
	}()

	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("NotifyUpdateReady blocked: headless loop is not draining updateCh")
	}
}

// logState reads the message fields each state renders; a nil-safe pass over
// every state guards against a panic reaching the daemon's only event loop.
func TestLogStateCoversEveryState(t *testing.T) {
	tr := newTestTray()
	tr.errorMsg = "boom"
	tr.queuedMsg = "2 sessions queued"
	tr.uploadingGame = "fallout4"
	tr.signInErr = "timed out"

	for _, s := range []State{
		StateIdle, StateUploading, StateError, StateLimitReached,
		StateNotAuth, StateNoGames, StateQueued, State(99),
	} {
		tr.logState(s) // must not panic
	}
}

func TestStateString(t *testing.T) {
	cases := map[State]string{
		StateIdle:         "idle",
		StateUploading:    "uploading",
		StateError:        "error",
		StateLimitReached: "limit_reached",
		StateNotAuth:      "not_signed_in",
		StateNoGames:      "no_games",
		StateQueued:       "queued",
		State(99):         "state(99)",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}
