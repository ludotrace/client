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

// countingAuth records how many times the worker got as far as asking for a
// token — which only happens once it has found work in the queue. Returning
// ErrNotSignedIn leaves it waiting on the tray afterwards, so the count stays
// at one ask per unit of work found.
type countingAuth struct {
	calls atomic.Int64
}

func (a *countingAuth) GetToken(context.Context) (string, error) {
	a.calls.Add(1)
	return "", auth.ErrNotSignedIn
}
func (a *countingAuth) SignIn(context.Context) error  { return nil }
func (a *countingAuth) SignOut(context.Context) error { return nil }
func (a *countingAuth) IsSignedIn() bool              { return false }

// An idle worker waits on the queue rather than re-checking it on a timer: it
// must not touch auth while the queue is empty, and must pick up an enqueued
// session promptly once one arrives — not on the next tick of some interval.
func TestRunUploadWorker_WakesOnEnqueue(t *testing.T) {
	dir := t.TempDir()

	q, err := queue.New(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}

	a := &countingAuth{}
	tr := tray.New(a, q, "", "", "")
	cfg := &config.Config{QueueMaxAgeDays: 7}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runUploadWorker(ctx, cfg, a, q, newExtractorStore(), tr)
	}()

	// Let it reach the empty-queue wait and settle there.
	time.Sleep(300 * time.Millisecond)
	if got := a.calls.Load(); got != 0 {
		t.Fatalf("GetToken called %d times on an empty queue, want 0", got)
	}

	if eerr := q.Enqueue(queue.Item{
		GameID: "fallout4", TmpPath: filepath.Join(dir, "lt_session.jsonl"),
		EndOffset: 100, CapturedAt: time.Now(),
	}); eerr != nil {
		t.Fatalf("Enqueue: %v", eerr)
	}

	// Well inside any plausible poll interval: this passes on a wake and fails
	// on a tick.
	deadline := time.After(time.Second)
	for a.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("worker did not pick up the enqueued session within 1s")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not return within 2s of cancellation")
	}
}
