package main

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ludotrace/client/internal/auth"
	"github.com/ludotrace/client/internal/config"
	"github.com/ludotrace/client/internal/queue"
	"github.com/ludotrace/client/internal/tray"
)

// signedOutAuth is an auth.Client that is never signed in, and counts how many
// times the worker asked.
type signedOutAuth struct {
	calls atomic.Int64
}

func (a *signedOutAuth) GetToken(context.Context) (string, error) {
	a.calls.Add(1)
	return "", auth.ErrNotSignedIn
}
func (a *signedOutAuth) SignIn(context.Context) error  { return nil }
func (a *signedOutAuth) SignOut(context.Context) error { return nil }
func (a *signedOutAuth) IsSignedIn() bool              { return false }

// A signed-out worker with a non-empty queue parks on the tray's signed-in
// signal instead of re-asking on a timer, so it must ask exactly once. The park
// is now minutes long, which makes ctx cancellation the only thing that can
// still stop it promptly — a plain sleep here would hang shutdown for the whole
// fallback interval. goleak (TestMain) fails the test if the worker never
// returns at all.
func TestRunUploadWorker_SignedOutParksAndStillCancels(t *testing.T) {
	dir := t.TempDir()

	q, err := queue.New(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	if eerr := q.Enqueue(queue.Item{
		GameID: "fallout4", TmpPath: filepath.Join(dir, "lt_session.jsonl"),
		EndOffset: 100, CapturedAt: time.Now(),
	}); eerr != nil {
		t.Fatalf("Enqueue: %v", eerr)
	}

	a := &signedOutAuth{}
	tr := tray.New(a, q, "", "", "")
	cfg := &config.Config{QueueMaxAgeDays: 7}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runUploadWorker(ctx, cfg, a, q, newExtractorStore(), tr)
	}()

	// The call count only means something past the longest interval a polling
	// implementation could plausibly use — a shorter window cannot tell
	// "parked" apart from "sleeping, about to re-ask". Under -short, settle for
	// the cancellation half of the assertion.
	settle := 11 * time.Second
	if testing.Short() {
		settle = 200 * time.Millisecond
	}
	time.Sleep(settle)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not return within 2s of cancellation — parked past ctx")
	}

	if got := a.calls.Load(); got != 1 {
		t.Errorf("GetToken called %d times in %v while signed out, want 1 (no polling)", got, settle)
	}
}
