package queue

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := New(filepath.Join(t.TempDir(), "queue.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return q
}

func item(name string) Item {
	return Item{GameID: "fallout4", TmpPath: name, CapturedAt: time.Now()}
}

// pending reports whether a wake is waiting on Enqueued.
func pending(q *Queue) bool {
	select {
	case <-q.Enqueued():
		return true
	default:
		return false
	}
}

// A consumer blocked on Enqueued is what replaces polling Len, so every
// Enqueue must produce a wake.
func TestEnqueue_Signals(t *testing.T) {
	q := newTestQueue(t)
	if pending(q) {
		t.Fatal("fresh queue already has a wake pending")
	}
	if err := q.Enqueue(item("a.jsonl")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !pending(q) {
		t.Error("no wake after Enqueue")
	}
}

// The signal is retained, not dropped, when nobody is blocked at the moment it
// is raised — otherwise a consumer that enters the wait just after an Enqueue
// sleeps forever on a non-empty queue.
func TestEnqueue_SignalOutlivesAnAbsentConsumer(t *testing.T) {
	q := newTestQueue(t)
	if err := q.Enqueue(item("a.jsonl")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Consumer shows up late.
	select {
	case <-q.Enqueued():
	case <-time.After(time.Second):
		t.Fatal("wake was dropped; a late consumer would block on a non-empty queue")
	}
}

// Repeated enqueues collapse into one wake — the consumer re-reads Len when it
// wakes, so extra signals would only cost extra loop passes.
func TestEnqueue_SignalsCoalesce(t *testing.T) {
	q := newTestQueue(t)
	for _, n := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		if err := q.Enqueue(item(n)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if !pending(q) {
		t.Fatal("no wake after three enqueues")
	}
	if pending(q) {
		t.Error("second wake buffered; signals should coalesce")
	}
}
