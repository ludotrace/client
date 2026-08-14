package queue

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func queuePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "queue.json")
}

func TestNewEmptyOnMissingFile(t *testing.T) {
	q, err := New(queuePath(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q.Len() != 0 {
		t.Fatalf("expected 0 items, got %d", q.Len())
	}
}

func TestEnqueueArrivalOrder(t *testing.T) {
	q, _ := New(queuePath(t))
	items := []Item{
		{GameID: "g1", TmpPath: "/tmp/a"},
		{GameID: "g2", TmpPath: "/tmp/b"},
		{GameID: "g3", TmpPath: "/tmp/c"},
	}
	for _, item := range items {
		if err := q.Enqueue(item); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if q.Len() != 3 {
		t.Fatalf("expected 3, got %d", q.Len())
	}
	got := q.Items()
	for i, want := range items {
		if got[i].TmpPath != want.TmpPath {
			t.Errorf("item %d: got %q, want %q", i, got[i].TmpPath, want.TmpPath)
		}
	}
}

func TestItemsReturnsCopy(t *testing.T) {
	q, _ := New(queuePath(t))
	_ = q.Enqueue(Item{TmpPath: "/tmp/x"})

	got := q.Items()
	if len(got) != 1 || got[0].TmpPath != "/tmp/x" {
		t.Fatalf("unexpected snapshot: %v", got)
	}
	got[0].TmpPath = "/tmp/mutated"
	if q.Items()[0].TmpPath != "/tmp/x" {
		t.Error("mutating the snapshot changed the queue")
	}
	if q.Len() != 1 {
		t.Fatalf("Items removed an item; Len=%d", q.Len())
	}
}

func TestItemsEmptyQueue(t *testing.T) {
	q, _ := New(queuePath(t))
	if got := q.Items(); len(got) != 0 {
		t.Fatalf("expected no items on empty queue, got %v", got)
	}
}

func TestContains(t *testing.T) {
	q, _ := New(queuePath(t))
	_ = q.Enqueue(Item{TmpPath: "/tmp/present"})

	if !q.Contains("/tmp/present") {
		t.Error("Contains returned false for queued path")
	}
	if q.Contains("/tmp/absent") {
		t.Error("Contains returned true for absent path")
	}
}

func TestUpdateAttempts(t *testing.T) {
	path := queuePath(t)
	q, _ := New(path)
	_ = q.Enqueue(Item{TmpPath: "/tmp/a", Attempts: 0})

	if err := q.UpdateAttempts("/tmp/a"); err != nil {
		t.Fatalf("UpdateAttempts: %v", err)
	}
	if err := q.UpdateAttempts("/tmp/a"); err != nil {
		t.Fatalf("UpdateAttempts: %v", err)
	}

	if item := q.Items()[0]; item.Attempts != 2 {
		t.Errorf("expected Attempts=2, got %d", item.Attempts)
	}

	// reload to confirm flush
	q2, _ := New(path)
	if item2 := q2.Items()[0]; item2.Attempts != 2 {
		t.Errorf("persisted Attempts: expected 2, got %d", item2.Attempts)
	}
}

func TestUpdateAttemptsNoop(t *testing.T) {
	q, _ := New(queuePath(t))
	if err := q.UpdateAttempts("/tmp/nonexistent"); err != nil {
		t.Fatalf("UpdateAttempts on missing path should not error: %v", err)
	}
}

func TestRemoveByTmpPath(t *testing.T) {
	path := queuePath(t)
	q, _ := New(path)
	_ = q.Enqueue(Item{TmpPath: "/tmp/a"})
	_ = q.Enqueue(Item{TmpPath: "/tmp/b"})
	_ = q.Enqueue(Item{TmpPath: "/tmp/c"})

	if err := q.Remove("/tmp/b"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if q.Len() != 2 {
		t.Fatalf("expected 2 items after Remove, got %d", q.Len())
	}
	if q.Contains("/tmp/b") {
		t.Error("removed item still found by Contains")
	}

	// verify order preserved
	if got := tmpPaths(q.Items()); got[0] != "/tmp/a" || got[1] != "/tmp/c" {
		t.Errorf("expected [/tmp/a /tmp/c], got %v", got)
	}
}

func TestRemoveNoop(t *testing.T) {
	q, _ := New(queuePath(t))
	_ = q.Enqueue(Item{TmpPath: "/tmp/a"})
	if err := q.Remove("/tmp/nonexistent"); err != nil {
		t.Fatalf("Remove of nonexistent path should not error: %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("expected 1 item, got %d", q.Len())
	}
}

func TestPersistence(t *testing.T) {
	path := queuePath(t)

	q1, _ := New(path)
	_ = q1.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/sess1", EndOffset: 42, Attempts: 1})
	_ = q1.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/sess2", EndOffset: 99})

	q2, err := New(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if q2.Len() != 2 {
		t.Fatalf("expected 2 items after reload, got %d", q2.Len())
	}
	item := q2.Items()[0]
	if item.TmpPath != "/tmp/sess1" || item.EndOffset != 42 || item.Attempts != 1 {
		t.Errorf("reloaded item mismatch: %+v", item)
	}
}

func TestConcurrentEnqueue(t *testing.T) {
	q, _ := New(queuePath(t))
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_ = q.Enqueue(Item{TmpPath: fmt.Sprintf("/tmp/%d", i)})
		}(i)
	}
	wg.Wait()
	if q.Len() != n {
		t.Errorf("expected %d items, got %d", n, q.Len())
	}
}
