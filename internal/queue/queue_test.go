package queue

import (
	"errors"
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
		if _, err := q.Enqueue(item); err != nil {
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
	_, _ = q.Enqueue(Item{TmpPath: "/tmp/x"})

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
	_, _ = q.Enqueue(Item{TmpPath: "/tmp/present"})

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
	_, _ = q.Enqueue(Item{TmpPath: "/tmp/a", Attempts: 0})

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
	_, _ = q.Enqueue(Item{TmpPath: "/tmp/a"})
	_, _ = q.Enqueue(Item{TmpPath: "/tmp/b"})
	_, _ = q.Enqueue(Item{TmpPath: "/tmp/c"})

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
	_, _ = q.Enqueue(Item{TmpPath: "/tmp/a"})
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
	_, _ = q1.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/sess1", EndOffset: 42, Attempts: 1})
	_, _ = q1.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/sess2", EndOffset: 99})

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
			_, _ = q.Enqueue(Item{TmpPath: fmt.Sprintf("/tmp/%d", i)})
		}(i)
	}
	wg.Wait()
	if q.Len() != n {
		t.Errorf("expected %d items, got %d", n, q.Len())
	}
}

func off(n int64) *int64 { return &n }

func TestEnqueue_SameRegionIsDuplicate(t *testing.T) {
	q, _ := New(filepath.Join(t.TempDir(), "queue.json"))
	if _, err := q.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/a", StartOffset: off(10), EndOffset: 50}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/b", StartOffset: off(10), EndOffset: 50}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second Enqueue err = %v, want ErrDuplicate", err)
	}
	// A shorter cut from the same start is covered too.
	if _, err := q.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/c", StartOffset: off(10), EndOffset: 40}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("shorter Enqueue err = %v, want ErrDuplicate", err)
	}
	// Another game, or another start, is a different region.
	if _, err := q.Enqueue(Item{GameID: "stardew", TmpPath: "/tmp/d", StartOffset: off(10), EndOffset: 50}); err != nil {
		t.Fatalf("other game: %v", err)
	}
	if _, err := q.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/e", StartOffset: off(50), EndOffset: 90}); err != nil {
		t.Fatalf("next region: %v", err)
	}
	if q.Len() != 3 {
		t.Fatalf("queue length = %d, want 3", q.Len())
	}
}

func TestEnqueue_LongerCutSupersedesPrefix(t *testing.T) {
	q, _ := New(filepath.Join(t.TempDir(), "queue.json"))
	_, _ = q.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/short", StartOffset: off(10), EndOffset: 40})
	superseded, err := q.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/long", StartOffset: off(10), EndOffset: 50})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(superseded) != 1 || superseded[0].TmpPath != "/tmp/short" {
		t.Fatalf("superseded = %+v, want /tmp/short", superseded)
	}
	if q.Len() != 1 || !q.Contains("/tmp/long") {
		t.Fatalf("queue = %+v, want only /tmp/long", q.Items())
	}
}

// Items without a StartOffset come from a pre-#95 queue.json; they are
// matched on EndOffset alone so retiring one clears its copies.
func TestRemoveCovered_LegacyCopies(t *testing.T) {
	q, _ := New(filepath.Join(t.TempDir(), "queue.json"))
	for _, p := range []string{"/tmp/1", "/tmp/2", "/tmp/3"} {
		_, _ = q.Enqueue(Item{GameID: "fo4", TmpPath: p, EndOffset: 50})
	}
	_, _ = q.Enqueue(Item{GameID: "fo4", TmpPath: "/tmp/other", EndOffset: 90})

	removed, err := q.RemoveCovered(Item{GameID: "fo4", TmpPath: "/tmp/1", EndOffset: 50})
	if err != nil {
		t.Fatalf("RemoveCovered: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %d, want 2", len(removed))
	}
	if q.Len() != 2 || !q.Contains("/tmp/1") || !q.Contains("/tmp/other") {
		t.Fatalf("queue = %+v", q.Items())
	}
}
