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

// A signed-out worker with a non-empty queue blocks: it asks for a token once
// and then waits, rather than spinning the loop (and with it EvictExpired, and
// the queue file) against an answer that cannot change without an event. It
// must stay interruptible while blocked — the wait is minutes long, so ctx is
// the only thing that can stop it promptly, and goleak (TestMain) fails the
// test if it never returns at all.
func TestRunUploadWorker_SignedOutBlocksAndStaysInterruptible(t *testing.T) {
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

	const settle = 200 * time.Millisecond
	time.Sleep(settle)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not return within 2s of cancellation — blocked past ctx")
	}

	// One ask, then blocked. This catches a branch that loops without waiting
	// at all; it deliberately does not try to pin the wait's length, which
	// would only encode whatever constant the fallback happens to use today.
	if got := a.calls.Load(); got != 1 {
		t.Errorf("GetToken called %d times in %v while signed out, want 1", got, settle)
	}
}
